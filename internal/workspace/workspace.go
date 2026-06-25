// Package workspace generalizes aixecutor's single-repo assumption (AIX-0020) to a
// workspace root that may be: a single git repo (today's degenerate case), a plain
// non-git directory, or a parent containing several git repos and/or plain dirs
// that one task spans. It presents a unified, workspace-relative view of the file
// set so the run baseline, diffs, and the AIX-0016 revert work across all roots
// with NO mutating git in any repo.
//
// Enumeration is "git where available, filesystem + ignore-set elsewhere": within
// each discovered git repo the read-only gateway lists tracked + untracked-non-
// ignored files (honoring that repo's .gitignore); outside repos the workspace
// walks the filesystem skipping a default + configurable ignore set. The resulting
// file set feeds the existing git snapshot/diff/restore engines unchanged, rooted
// at the workspace root.
package workspace

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jaxmef/aixecutor/internal/git"
)

// Options configures discovery and enumeration.
type Options struct {
	// MaxDepth bounds how deep beneath the root git repos are discovered (the root
	// is depth 1). Values < 1 are treated as 1.
	MaxDepth int
	// Ignore is the set of directory names skipped when walking non-git areas. `.git`
	// is always skipped. Matched at any depth by base name.
	Ignore []string
	// ExcludePrefixes are workspace-relative path prefixes always excluded from
	// enumeration (e.g. the tool's runsDir), so run artifacts never enter the
	// baseline, diffs, or a revert.
	ExcludePrefixes []string
	// Opener opens a directory as a read-only git gateway; it defaults to git.Open.
	// Tests inject a fake-gateway opener so discovery + enumeration run without any
	// real git (a `.git` marker dir is enough to be discovered as a repo root).
	Opener func(dir string) (*git.Gateway, error)
}

// repo is one discovered git repository within the workspace.
type repo struct {
	rel string       // workspace-relative path ("" when the workspace root is itself the repo)
	abs string       // absolute path
	gw  *git.Gateway // read-only gateway rooted at abs
}

// Workspace is a discovered set of roots presented as one workspace-relative tree.
type Workspace struct {
	root     string          // absolute workspace root
	repos    []repo          // discovered git repos (sorted by rel)
	ignore   map[string]bool // ignored dir base names for non-git walking
	excludes []string        // cleaned, workspace-relative excluded prefixes
	maxDepth int             // bounds BOTH repo discovery and the plain-area walk
	opener   func(dir string) (*git.Gateway, error)
	// snap is a gateway rooted at the workspace root used ONLY for its filesystem
	// engines that need no git repo: glob-based SnapshotPaths and `git diff
	// --no-index` (DiffTrees). It carries the workspace excludes so per-subtask
	// snapshots drop runsDir. It never runs a repo-scoped git command.
	snap *git.Gateway
}

// Discover builds a Workspace rooted at the absolute path root. It finds git repos
// beneath root (a directory containing a `.git` entry is a repo root and is not
// descended into), up to opts.MaxDepth, and treats everything else as plain dirs.
// A single git repo at the root, or a plain non-git dir, are the degenerate cases.
func Discover(root string, opts Options) (*Workspace, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("workspace: resolving root %q: %w", root, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("workspace: reading root %q: %w", abs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace: root %q is not a directory", abs)
	}

	maxDepth := opts.MaxDepth
	if maxDepth < 1 {
		maxDepth = 1
	}
	ignore := map[string]bool{".git": true}
	for _, name := range opts.Ignore {
		if name = strings.TrimSpace(name); name != "" {
			ignore[name] = true
		}
	}

	opener := opts.Opener
	if opener == nil {
		opener = git.Open
	}
	ws := &Workspace{
		root:     abs,
		ignore:   ignore,
		excludes: cleanRelPrefixes(opts.ExcludePrefixes),
		maxDepth: maxDepth,
		opener:   opener,
		snap:     git.NewGatewayWithRunner(abs, git.ExecRunner),
	}
	ws.snap.SetExcludePrefixes(ws.excludes...)
	if err := ws.discoverRepos(maxDepth); err != nil {
		return nil, err
	}
	return ws, nil
}

// Root returns the absolute workspace root (the analogue of a single repo's root;
// the executor's working dir and the runsDir anchor).
func (w *Workspace) Root() string { return w.root }

// Repos returns the workspace-relative paths of the discovered git repos, sorted.
// A workspace with no git repos (a plain dir) returns an empty slice.
func (w *Workspace) Repos() []string {
	out := make([]string, 0, len(w.repos))
	for _, r := range w.repos {
		if r.rel == "" {
			out = append(out, ".")
		} else {
			out = append(out, r.rel)
		}
	}
	return out
}

// discoverRepos walks the workspace root to maxDepth, recording each directory that
// contains a `.git` entry as a repo (without descending into it), and skipping the
// ignore set. Depth is measured in path segments below the root (root = depth 1).
func (w *Workspace) discoverRepos(maxDepth int) error {
	err := filepath.WalkDir(w.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("workspace: walking %q: %w", path, err)
		}
		if !d.IsDir() {
			return nil
		}
		rel := w.relOf(path)
		if rel != "" && (w.ignore[d.Name()] || w.isExcluded(rel)) {
			return fs.SkipDir
		}
		if depth(rel) > maxDepth {
			return fs.SkipDir
		}
		if isRepoRoot(path) {
			gw, err := w.opener(path)
			if err != nil {
				// A `.git` that git itself rejects (corrupt/partial) — treat the dir as
				// plain rather than failing the whole workspace.
				return nil
			}
			w.repos = append(w.repos, repo{rel: rel, abs: gw.RepoRoot(), gw: gw})
			return fs.SkipDir // do not descend into a repo
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(w.repos, func(i, j int) bool { return w.repos[i].rel < w.repos[j].rel })
	return nil
}

// CurrentRels enumerates the workspace's current file set as cleaned,
// workspace-relative paths: tracked + untracked-non-ignored within each git repo
// (via the read-only gateway), plus regular files in the plain (non-repo) area
// found by a filesystem walk honoring the ignore set. Excluded prefixes (runsDir)
// are dropped. The result is de-duplicated and sorted.
func (w *Workspace) CurrentRels(ctx context.Context) ([]string, error) {
	seen := make(map[string]struct{})
	var rels []string
	add := func(rel string) {
		rel = filepath.Clean(rel)
		if rel == "." || rel == "" || w.isExcluded(rel) {
			return
		}
		if _, ok := seen[rel]; ok {
			return
		}
		seen[rel] = struct{}{}
		rels = append(rels, rel)
	}

	// Git repos: ls-files (+ untracked-non-ignored), prefixed to workspace-relative.
	for _, r := range w.repos {
		tracked, err := r.gw.TrackedFiles(ctx)
		if err != nil {
			return nil, fmt.Errorf("workspace: enumerating tracked files in %q: %w", r.rel, err)
		}
		untracked, err := r.gw.UntrackedFiles(ctx)
		if err != nil {
			return nil, fmt.Errorf("workspace: enumerating untracked files in %q: %w", r.rel, err)
		}
		for _, f := range append(tracked, untracked...) {
			add(w.join(r.rel, f))
		}
	}

	// Plain area: walk the workspace skipping repo subtrees + the ignore set.
	if err := w.walkPlain(add); err != nil {
		return nil, err
	}

	sort.Strings(rels)
	return rels, nil
}

// walkPlain walks the workspace root, adding regular files that lie OUTSIDE every
// discovered git repo (those are enumerated via their gateway) and outside the
// ignore set / excluded prefixes. Symlinks and other non-regular files are skipped
// (the snapshot engine copies regular files only).
func (w *Workspace) walkPlain(add func(rel string)) error {
	repoAbs := make(map[string]bool, len(w.repos))
	for _, r := range w.repos {
		repoAbs[r.abs] = true
	}
	return filepath.WalkDir(w.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("workspace: walking %q: %w", path, err)
		}
		rel := w.relOf(path)
		if d.IsDir() {
			if rel == "" {
				return nil // the workspace root itself
			}
			if repoAbs[path] || w.ignore[d.Name()] || w.isExcluded(rel) {
				return fs.SkipDir
			}
			// Bound the plain-area walk to the same depth as repo discovery, so a
			// huge org tree cannot be enumerated without limit (scope guard).
			if depth(rel) > w.maxDepth {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			add(rel)
		}
		return nil
	})
}

// join composes a repo's workspace-relative prefix with a repo-relative path.
func (w *Workspace) join(repoRel, file string) string {
	if repoRel == "" {
		return filepath.Clean(file)
	}
	return filepath.Join(repoRel, file)
}

// relOf returns path relative to the workspace root ("" for the root itself).
func (w *Workspace) relOf(path string) string {
	rel, err := filepath.Rel(w.root, path)
	if err != nil || rel == "." {
		return ""
	}
	return rel
}

// isExcluded reports whether a cleaned workspace-relative path is at or under any
// excluded prefix.
func (w *Workspace) isExcluded(rel string) bool {
	clean := filepath.Clean(rel)
	for _, pre := range w.excludes {
		if clean == pre || strings.HasPrefix(clean, pre+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// isRepoRoot reports whether dir contains a `.git` entry (a repo working tree root).
func isRepoRoot(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// depth returns the number of path segments in a workspace-relative path (the root,
// rel=="" , is depth 1; a top-level child is depth 2).
func depth(rel string) int {
	if rel == "" {
		return 1
	}
	return 1 + strings.Count(rel, string(filepath.Separator)) + 1
}

// cleanRelPrefixes normalizes workspace-relative exclude prefixes, dropping empty,
// ".", absolute, or escaping entries.
func cleanRelPrefixes(prefixes []string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, p := range prefixes {
		c := filepath.Clean(p)
		if c == "." || c == "" || filepath.IsAbs(c) || c == ".." || strings.HasPrefix(c, ".."+string(filepath.Separator)) {
			continue
		}
		if _, ok := seen[c]; ok {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
