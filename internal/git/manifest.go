package git

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FileMeta is the lightweight per-file fingerprint a manifest records: modification
// time and size. It is enough to detect that a file was written (created or edited)
// during a subtask window without reading or copying content — cheaper than a
// snapshot and sufficient for the "did the executor touch paths outside its declared
// files?" check (AIX-0025).
type FileMeta struct {
	ModTime time.Time
	Size    int64
}

// Manifest maps a root-relative path to its FileMeta. It is a point-in-time listing
// of the working tree rooted at an explicit directory, enumerated through the SAME
// read-only pipeline as CaptureBaseline / FullDiff (tracked + untracked-non-ignored,
// minus the gateway's exclude prefixes), so a before/after pair of manifests sees
// exactly the paths those diffs would — no runsDir, .gitignored, or editor-dir noise.
type Manifest map[string]FileMeta

// Manifest enumerates the working tree rooted at root and returns a path->FileMeta
// listing. root lets git and the stats run in a directory other than the gateway's
// repoRoot (e.g. a linked worktree during isolated execution); an empty root defaults
// to repoRoot.
//
// Enumeration mirrors CaptureBaseline / FullDiff exactly:
//
//   - tracked files via `git ls-files`;
//   - untracked, non-ignored files via `git ls-files --others --exclude-standard`
//     (this is how .gitignore is honored — build artifacts never appear);
//   - the union is de-duplicated and passed through filterExcluded, so the gateway's
//     configured exclude prefixes (runsDir, editor dirs) are absent by construction.
//
// Each surviving path is stat'd under root. A path that git listed but that no longer
// exists on disk (e.g. a tracked file deleted in the tree) is skipped, matching the
// snapshot engine's treatment of missing paths; any other stat error is surfaced.
// The function performs only read-only git and filesystem reads — no mutating git.
func (g *Gateway) Manifest(ctx context.Context, root string) (Manifest, error) {
	if root == "" {
		root = g.repoRoot
	}
	tracked, err := g.lsFilesIn(ctx, root, "ls-files", "-z")
	if err != nil {
		return nil, fmt.Errorf("git manifest: enumerating tracked files: %w", err)
	}
	untracked, err := g.lsFilesIn(ctx, root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, fmt.Errorf("git manifest: enumerating untracked files: %w", err)
	}

	rels := g.filterExcluded(dedupePaths(append(tracked, untracked...)))
	m := make(Manifest, len(rels))
	for _, rel := range rels {
		info, err := os.Stat(filepath.Join(root, rel))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // listed but gone from the tree; nothing to fingerprint.
			}
			return nil, fmt.Errorf("git manifest: stat %q: %w", rel, err)
		}
		m[rel] = FileMeta{ModTime: info.ModTime(), Size: info.Size()}
	}
	return m, nil
}

// lsFilesIn runs an allowlisted read command (an ls-files variant) in an explicit
// dir rather than the gateway's repoRoot, and returns the NUL-split paths. It is the
// rooted counterpart of read, needed so Manifest can enumerate a linked worktree.
// The read allowlist is enforced exactly as in read, so this stays read-only.
func (g *Gateway) lsFilesIn(ctx context.Context, dir string, args ...string) ([]string, error) {
	if err := checkAllowed(args); err != nil {
		return nil, err
	}
	stdout, stderr, err := g.run(ctx, dir, args...)
	if err != nil {
		return nil, fmt.Errorf("git %s: %w%s", strings.Join(args, " "), err, stderrTail(stderr))
	}
	return splitNUL(stdout), nil
}

// Changed returns the sorted set of paths that differ between the before (receiver)
// and after manifests: additions (present only in after), removals (present only in
// before), and modifications (present in both but with a different size or mod-time).
// It is the change-diffing helper the executor uses to spot paths written outside a
// subtask's declared files.
func (before Manifest) Changed(after Manifest) []string {
	changed := make(map[string]struct{})
	for path, a := range after {
		b, ok := before[path]
		if !ok || b.Size != a.Size || !b.ModTime.Equal(a.ModTime) {
			changed[path] = struct{}{}
		}
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			changed[path] = struct{}{}
		}
	}
	out := make([]string, 0, len(changed))
	for path := range changed {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}
