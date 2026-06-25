package git

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Baseline is a snapshot of the user's working tree captured at run start. Run
// diffs are computed relative to this baseline rather than to HEAD, which has two
// consequences the project requires (CLAUDE.md §4.4):
//
//   - diffs reflect changes the run actually made, measured from the user's
//     starting point;
//   - pre-existing uncommitted changes are captured in the baseline, so they
//     appear identically in the "before" and "after" trees and are naturally
//     excluded from the run diff.
//
// The baseline copies tracked files plus untracked-but-not-ignored files;
// .gitignored paths are never snapshotted (enumeration uses ls-files
// --exclude-standard). Contents are copied with raw os I/O, no git writes.
type Baseline struct {
	// Snapshot is the on-disk copy of the captured files.
	Snapshot Snapshot
}

// Dir returns the directory holding the baseline's snapshotted files.
func (b Baseline) Dir() string { return b.Snapshot.Dir }

// CaptureBaseline snapshots the gateway's working tree into dstDir (e.g.
// <run>/.baseline) and returns the resulting Baseline. The set of files is
// enumerated entirely through read-only git:
//
//   - tracked files via `git ls-files`;
//   - untracked, non-ignored files via `git ls-files --others --exclude-standard`.
//
// The union is de-duplicated and snapshotted with snapshotFiles. warn, if
// non-nil, is called once if the snapshot exceeds the soft size ceiling (the
// caller passes its logger). The function performs no mutating git command.
func (g *Gateway) CaptureBaseline(ctx context.Context, dstDir string, warn func(bytes int64)) (Baseline, error) {
	tracked, err := g.TrackedFiles(ctx)
	if err != nil {
		return Baseline{}, fmt.Errorf("git baseline: enumerating tracked files: %w", err)
	}
	untracked, err := g.UntrackedFiles(ctx)
	if err != nil {
		return Baseline{}, fmt.Errorf("git baseline: enumerating untracked files: %w", err)
	}

	rels := g.filterExcluded(dedupePaths(append(tracked, untracked...)))
	snap, err := snapshotFiles(g.repoRoot, dstDir, rels, warn)
	if err != nil {
		return Baseline{}, err
	}
	return Baseline{Snapshot: snap}, nil
}

// SnapshotPaths snapshots an explicit set of repo-relative paths or globs into
// dstDir and returns the snapshot. It is the building block for per-subtask
// diffs: the scheduler calls it with a subtask's declared `files` before and
// after the subtask runs, then diffs the two snapshot dirs.
//
// Each entry is matched against the working tree with filepath.Glob (relative to
// repoRoot); a literal path with no glob metacharacters matches itself. Matched
// files (and the regular files under any matched directories) are copied. Entries
// matching nothing contribute nothing — a subtask that has not yet created a
// declared file simply has no "before" content, so the after-snapshot shows it as
// an addition. Enumeration and matching are filesystem reads; no git runs here.
func (g *Gateway) SnapshotPaths(dstDir string, patterns []string, warn func(bytes int64)) (Snapshot, error) {
	rels, err := g.resolvePatterns(patterns)
	if err != nil {
		return Snapshot{}, err
	}
	// Defensively drop any matched path under an excluded prefix: a subtask
	// declaring a broad glob (e.g. `**` or `.aixecutor/**`) must not pull the tool's
	// own runsDir into its per-subtask snapshot/diff. With no prefixes configured
	// this is the cheap no-op fast path, so normal declared paths are unaffected.
	return snapshotFiles(g.repoRoot, dstDir, g.filterExcluded(rels), warn)
}

// resolvePatterns expands repo-relative glob patterns against the working tree
// and returns the de-duplicated, repo-relative matches. A pattern with no glob
// metacharacters is returned as-is (so snapshotFiles handles the "declared but
// not-yet-created" case uniformly). Patterns are cleaned and rejected if they
// escape the repo root.
func (g *Gateway) resolvePatterns(patterns []string) ([]string, error) {
	var out []string
	for _, p := range patterns {
		clean := filepath.Clean(p)
		if clean == "." || clean == "" {
			continue
		}
		if filepath.IsAbs(clean) || clean == ".." || hasDotDotPrefix(clean) {
			return nil, fmt.Errorf("git snapshot: pattern %q escapes the repository root", p)
		}
		if !hasGlobMeta(clean) {
			out = append(out, clean)
			continue
		}
		matches, err := filepath.Glob(filepath.Join(g.repoRoot, clean))
		if err != nil {
			// The only error filepath.Glob returns is ErrBadPattern.
			return nil, fmt.Errorf("git snapshot: bad pattern %q: %w", p, err)
		}
		for _, m := range matches {
			rel, err := filepath.Rel(g.repoRoot, m)
			if err != nil {
				return nil, fmt.Errorf("git snapshot: relativizing match %q: %w", m, err)
			}
			out = append(out, rel)
		}
	}
	return dedupePaths(out), nil
}

// dedupePaths cleans, de-duplicates, and sorts a list of relative paths so the
// snapshot order is deterministic (helpful for tests and stable logs).
func dedupePaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		c := filepath.Clean(p)
		if c == "." || c == "" {
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

// cleanPrefixes normalizes a list of repo-relative exclusion prefixes: each is
// filepath.Clean'd, and entries that are empty, ".", absolute, or escape the repo
// root (".." / "../…") are dropped. Such an entry would mean "exclude everything"
// or "exclude something outside the tree", neither of which is a valid runsDir
// exclusion, so dropping it leaves the historical (no-exclusion) behavior. The
// result is de-duplicated and sorted for determinism.
func cleanPrefixes(prefixes []string) []string {
	seen := make(map[string]struct{}, len(prefixes))
	out := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		c := filepath.Clean(p)
		if c == "." || c == "" || filepath.IsAbs(c) || c == ".." || hasDotDotPrefix(c) {
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

// filterExcluded drops every repo-relative path that lies under one of the
// gateway's configured exclude prefixes (g.excludePrefixes). It is applied to the
// enumerated file set in BOTH CaptureBaseline and FullDiff, so the baseline and
// the full diff's current side exclude the same paths and stay symmetric. With no
// prefixes configured it returns rels unchanged (cheap fast path).
func (g *Gateway) filterExcluded(rels []string) []string {
	if len(g.excludePrefixes) == 0 {
		return rels
	}
	out := rels[:0:0] // fresh backing array; do not alias the input
	for _, rel := range rels {
		if g.isExcluded(rel) {
			continue
		}
		out = append(out, rel)
	}
	return out
}

// isExcluded reports whether the cleaned repo-relative path rel lies at or under
// any configured exclude prefix. A path is excluded when it equals a prefix or is
// nested beneath it (prefix + separator), so "x/runs" matches "x/runs" and
// "x/runs/<id>/run.yaml" but never "x/runsfoo" (segment-boundary match only).
func (g *Gateway) isExcluded(rel string) bool {
	clean := filepath.Clean(rel)
	for _, pre := range g.excludePrefixes {
		if clean == pre || strings.HasPrefix(clean, pre+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// hasGlobMeta reports whether p contains any glob metacharacter recognized by
// filepath.Match (*, ?, [, ]). Used to short-circuit literal paths so they are
// snapshotted even when they do not yet exist.
func hasGlobMeta(p string) bool {
	for i := 0; i < len(p); i++ {
		switch p[i] {
		case '*', '?', '[', ']':
			return true
		}
	}
	return false
}
