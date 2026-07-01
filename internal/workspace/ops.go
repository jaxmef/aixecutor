package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/jaxmef/aixecutor/internal/git"
)

// These methods present the workspace as a drop-in for the single-repo gateway the
// pipeline and run baseliner use: the run-start baseline, per-subtask snapshots,
// diffs, and the AIX-0016 revert — all over the unified workspace-relative file set,
// rooted at the workspace root, reusing the git package's filesystem engines. No
// mutating git is ever run; cross-repo enumeration is read-only, and snapshot/diff/
// restore are raw file I/O.

// TrackedFiles returns the workspace's current file set (workspace-relative), so a
// *Workspace satisfies the planner's read-only tracked-file lister exactly as a
// single-repo gateway does. It is the whole-workspace enumeration (every repo +
// the plain area), which is what the planner's orientation summary wants.
func (w *Workspace) TrackedFiles(ctx context.Context) ([]string, error) {
	return w.CurrentRels(ctx)
}

// CaptureBaseline snapshots the entire workspace (every git repo's tracked +
// untracked-non-ignored files plus the plain-area files) into dstDir, preserving
// workspace-relative layout, and returns the Baseline. It is the multi-root analogue
// of Gateway.CaptureBaseline; pre-existing uncommitted changes in every repo are
// captured, so they are restored byte-for-byte by a later RestoreTree.
func (w *Workspace) CaptureBaseline(ctx context.Context, dstDir string, warn func(bytes int64)) (git.Baseline, error) {
	rels, err := w.CurrentRels(ctx)
	if err != nil {
		return git.Baseline{}, err
	}
	snap, err := git.SnapshotFiles(w.root, dstDir, rels, warn)
	if err != nil {
		return git.Baseline{}, err
	}
	return git.Baseline{Snapshot: snap}, nil
}

// SnapshotPaths snapshots an explicit set of workspace-relative paths/globs into
// dstDir (the per-subtask before/after snapshots). Globs are matched against the
// workspace root and may span repos (e.g. repoA/internal/**, dirB/...). It delegates
// to the root gateway's filesystem glob engine (no git), which also drops excluded
// prefixes (runsDir).
func (w *Workspace) SnapshotPaths(dstDir string, patterns []string, warn func(bytes int64)) (git.Snapshot, error) {
	return w.snap.SnapshotPaths(dstDir, patterns, warn)
}

// DiffTrees computes `git diff --no-index` between two snapshot dirs (read-only;
// works without any repo) — used for per-subtask diffs.
func (w *Workspace) DiffTrees(ctx context.Context, beforeDir, afterDir string) (git.Diff, error) {
	return w.snap.DiffTrees(ctx, beforeDir, afterDir)
}

// FullDiff computes the whole-workspace diff from the run-start baseline (at
// baselineDir) to the CURRENT workspace: it snapshots the current workspace into a
// temp dir and diffs baseline → current, aggregating changes across all repos and
// the plain area. baselineDir comes straight from run.Baseline.Dir, so it works on
// resume.
func (w *Workspace) FullDiff(ctx context.Context, baselineDir string) (git.Diff, error) {
	tmp, err := os.MkdirTemp("", "aixecutor-ws-fulldiff-*")
	if err != nil {
		return git.Diff{}, fmt.Errorf("workspace full diff: temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	rels, err := w.CurrentRels(ctx)
	if err != nil {
		return git.Diff{}, err
	}
	if _, err := git.SnapshotFiles(w.root, tmp, rels, nil); err != nil {
		return git.Diff{}, err
	}
	return w.snap.DiffTrees(ctx, baselineDir, tmp)
}

// Manifest enumerates the current workspace file set and returns a workspace-relative
// path->FileMeta (mtime,size) listing — the multi-root analogue of Gateway.Manifest.
// It reuses CurrentRels, so its enumeration and exclusions are identical to
// CaptureBaseline / FullDiff by construction (tracked + untracked-non-ignored per repo
// honouring each repo's .gitignore, plus the plain area, minus excluded prefixes); no
// runsDir, editor-dir, or ignored path can appear. root lets the stats run under a
// directory other than the workspace root; an empty root defaults to the workspace root.
// Read-only: only ls-files enumeration and filesystem stats, never mutating git.
func (w *Workspace) Manifest(ctx context.Context, root string) (git.Manifest, error) {
	if root == "" {
		root = w.root
	}
	rels, err := w.CurrentRels(ctx)
	if err != nil {
		return nil, err
	}
	m := make(git.Manifest, len(rels))
	for _, rel := range rels {
		info, err := os.Stat(filepath.Join(root, rel))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // listed but gone from the tree; nothing to fingerprint.
			}
			return nil, fmt.Errorf("workspace manifest: stat %q: %w", rel, err)
		}
		m[rel] = git.FileMeta{ModTime: info.ModTime(), Size: info.Size()}
	}
	return m, nil
}

// RestoreTree reverts the entire workspace to the snapshot at snapshotDir (the
// AIX-0016 revert generalized to multiple roots): run-added files are deleted and
// baseline files copied back across every repo and the plain area, via raw file I/O
// — NO mutating git in any repo. extraExcludes augments the workspace excludes
// (e.g. a custom docs dir) so amended docs and run artifacts are never touched.
func (w *Workspace) RestoreTree(ctx context.Context, snapshotDir string, extraExcludes []string) (git.RestoreResult, error) {
	current, err := w.CurrentRels(ctx)
	if err != nil {
		return git.RestoreResult{}, err
	}
	excludes := append(append([]string{}, w.excludes...), cleanRelPrefixes(extraExcludes)...)
	return git.RestoreFromSnapshot(w.root, snapshotDir, current, excludes)
}
