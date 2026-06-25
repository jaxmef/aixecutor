package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Diff is the result of comparing two snapshot trees: the unified-diff text plus
// a flag recording whether any differences were found. Later tickets persist
// Patch as a *.patch file under the run dir.
type Diff struct {
	// Patch is the unified-diff text (empty when the trees are identical).
	Patch string
	// HasChanges reports whether the trees differed. It is derived from `git
	// diff --no-index`'s exit code: 1 means "differences found" (success for us),
	// 0 means "identical".
	HasChanges bool
}

// DiffTrees compares two directory trees with `git diff --no-index <before>
// <after>` and returns the unified diff. This is the engine behind both the full
// run diff and per-subtask diffs; callers supply snapshot directories produced by
// CaptureBaseline / SnapshotPaths.
//
// `git diff --no-index` is read-only — it never touches the object store or index
// and works even outside a repository — and `diff` is on the read allowlist, so
// this is a legitimate gateway operation. Crucially, --no-index uses exit code 1
// to mean "the inputs differ"; we treat exit 1 as SUCCESS (HasChanges = true) and
// only exit codes other than 0/1 as real errors. Stderr is surfaced on genuine
// failures.
//
// Empty/missing directories are handled by substituting an empty temp dir, so a
// brand-new baseline (no "before" files) still diffs cleanly as all-additions.
func (g *Gateway) DiffTrees(ctx context.Context, beforeDir, afterDir string) (Diff, error) {
	before, cleanupBefore, err := ensureDir(beforeDir)
	if err != nil {
		return Diff{}, err
	}
	defer cleanupBefore()
	after, cleanupAfter, err := ensureDir(afterDir)
	if err != nil {
		return Diff{}, err
	}
	defer cleanupAfter()

	args := []string{"diff", "--no-index", "--", before, after}
	// Route through the same allowlist gate as read (defense in depth), but keep
	// our own exit-code handling because --no-index's "1 == differs" convention
	// is the opposite of read's "non-zero == error".
	if err := checkAllowed(args); err != nil {
		return Diff{}, err
	}
	stdout, stderr, runErr := g.run(ctx, g.repoRoot, args...)
	if runErr != nil {
		if code, ok := exitCode(runErr); ok {
			switch code {
			case 1:
				// Differences found — the normal, successful "there is a diff" case.
				return Diff{Patch: rewriteSnapshotPaths(string(stdout), before, after), HasChanges: true}, nil
			default:
				return Diff{}, fmt.Errorf("git diff --no-index: exit code %d%s", code, stderrTail(stderr))
			}
		}
		// Failed to start or a non-exit error (e.g. git not found, context
		// cancelled): a genuine failure.
		return Diff{}, fmt.Errorf("git diff --no-index: %w%s", runErr, stderrTail(stderr))
	}
	return Diff{Patch: string(stdout), HasChanges: false}, nil
}

// rewriteSnapshotPaths strips the snapshot temp-dir prefixes from a `git diff
// --no-index` patch so its headers read as repo-relative paths (`a/calc.go`)
// instead of leaking the absolute snapshot directory (`a/var/folders/…/aixecutor-st-after-123/calc.go`).
//
// `git diff --no-index` uses the literal operand paths as the `a/`…`b/` tokens,
// stripping only a leading slash. Since SnapshotPaths mirrors files at their
// repo-relative paths under each snapshot root, removing the operand prefix
// recovers exactly that repo-relative path wherever it appears (the `diff --git`,
// `---`, `+++`, and rename/copy lines).
func rewriteSnapshotPaths(patch, beforeDir, afterDir string) string {
	if patch == "" {
		return patch
	}
	for _, dir := range []string{beforeDir, afterDir} {
		if dir == "" {
			continue
		}
		prefix := strings.TrimPrefix(filepath.Clean(dir), "/") + "/"
		patch = strings.ReplaceAll(patch, prefix, "")
	}
	return patch
}

// FullDiff compares the current working tree against the run-start baseline and
// returns the unified diff (CLAUDE.md §4.4 "full diff" for senior review).
//
// It snapshots the current tree (tracked + untracked-non-ignored, .gitignore
// respected) into a temp dir, then diffs baseline -> current. The temp snapshot
// is removed before returning. No mutating git is performed.
//
// The current-tree snapshot is filtered through the SAME exclude prefixes as the
// baseline (g.filterExcluded), so the tool's own output dir (paths.runsDir) is
// absent from BOTH sides of the diff. This symmetry is essential: the baseline no
// longer contains runsDir, so if the current side still did, the entire runsDir
// would show up as "added" and pollute the senior-review diff. Filtering both
// sides keeps the full diff clean of the tool's artifacts.
func (g *Gateway) FullDiff(ctx context.Context, baseline Baseline, warn func(bytes int64)) (Diff, error) {
	curDir, err := os.MkdirTemp("", "aixecutor-current-*")
	if err != nil {
		return Diff{}, fmt.Errorf("git diff: creating temp dir for current tree: %w", err)
	}
	defer os.RemoveAll(curDir)

	tracked, err := g.TrackedFiles(ctx)
	if err != nil {
		return Diff{}, fmt.Errorf("git diff: enumerating tracked files: %w", err)
	}
	untracked, err := g.UntrackedFiles(ctx)
	if err != nil {
		return Diff{}, fmt.Errorf("git diff: enumerating untracked files: %w", err)
	}
	rels := g.filterExcluded(dedupePaths(append(tracked, untracked...)))
	if _, err := snapshotFiles(g.repoRoot, curDir, rels, warn); err != nil {
		return Diff{}, err
	}

	return g.DiffTrees(ctx, baseline.Dir(), curDir)
}

// SubtaskDiff computes the diff for a single subtask from two snapshots of its
// declared paths: before (taken prior to the subtask running) and after (taken
// once it finished). Only the subtask's declared paths are reflected, satisfying
// the per-subtask diff requirement (CLAUDE.md §4.4). Both snapshots are produced
// by SnapshotPaths; this helper just diffs their directories.
func (g *Gateway) SubtaskDiff(ctx context.Context, before, after Snapshot) (Diff, error) {
	return g.DiffTrees(ctx, before.Dir, after.Dir)
}

// ensureDir returns a directory usable as a `git diff --no-index` operand. If
// dir exists it is returned with a no-op cleanup. If dir is empty or missing, a
// fresh empty temp dir is created and returned with a cleanup that removes it, so
// "no before content" diffs as all-additions rather than failing on a missing
// path.
func ensureDir(dir string) (path string, cleanup func(), err error) {
	if dir != "" {
		if info, statErr := os.Stat(dir); statErr == nil && info.IsDir() {
			return dir, func() {}, nil
		}
	}
	tmp, err := os.MkdirTemp("", "aixecutor-empty-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("git diff: creating empty dir: %w", err)
	}
	return tmp, func() { os.RemoveAll(tmp) }, nil
}
