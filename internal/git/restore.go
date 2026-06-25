package git

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// RestoreResult reports what a RestoreTree did, for logging and tests.
type RestoreResult struct {
	// Restored is the number of files copied back from the snapshot (the run's
	// modifications and deletions undone).
	Restored int
	// Deleted is the number of files removed (the run's additions undone).
	Deleted int
}

// RestoreTree syncs the working tree back to the snapshot at snapshotDir — the
// AIX-0016 "Option B" clean revert. It is the inverse of CaptureBaseline:
//
//   - every file captured in the snapshot is copied back over the working tree
//     (undoing the run's modifications, and re-creating files the run deleted);
//   - every file that exists in the working tree now but is ABSENT from the
//     snapshot is deleted (undoing the run's additions);
//   - directories emptied by those deletions are pruned.
//
// Because the snapshot is the run-start baseline — which already captured the
// user's pre-existing uncommitted changes — the restore returns the tree to its
// exact pre-execution state, including those uncommitted changes, byte for byte.
//
// Scope & safety: enumeration of the current tree is read-only git
// (tracked + untracked-non-ignored); the actual changes are raw file I/O, so NO
// mutating git runs (invariant #1). Paths under any exclude prefix — the gateway's
// configured excludes (paths.runsDir) UNION extraExcludes (e.g. a custom docs dir)
// — are never read for deletion and never written, so run artifacts and the
// planning docs the user just amended are preserved. The exclude set is also why
// this is "scope-aware": AIX-0020 reuses it over a multi-root workspace by passing
// the per-workspace exclusions.
func (g *Gateway) RestoreTree(ctx context.Context, snapshotDir string, extraExcludes []string) (RestoreResult, error) {
	// Current tree (read-only git enumeration), minus the gateway's excludes.
	current, err := g.currentTreeRels(ctx)
	if err != nil {
		return RestoreResult{}, err
	}
	excludes := mergePrefixes(g.excludePrefixes, cleanPrefixes(extraExcludes))
	return RestoreFromSnapshot(g.repoRoot, snapshotDir, current, excludes)
}

// RestoreFromSnapshot is the engine behind Gateway.RestoreTree, with the current
// file enumeration (currentRels, root-relative) injected rather than read from git.
// It syncs root to the snapshot at snapshotDir using only raw file I/O: it deletes
// regular files present in currentRels but absent from the snapshot (the additions),
// copies every snapshot file back (undoing modifies + re-adding deletes), and prunes
// emptied dirs. Paths under excludePrefixes are never touched. NO git runs here — it
// is the multi-root reuse seam for the workspace revert (AIX-0020), which enumerates
// currentRels across repos + plain dirs and calls this with the workspace root.
func RestoreFromSnapshot(root, snapshotDir string, currentRels, excludePrefixes []string) (RestoreResult, error) {
	excludes := cleanPrefixes(excludePrefixes)

	baselineRels, err := snapshotRels(snapshotDir)
	if err != nil {
		return RestoreResult{}, err
	}
	baselineSet := make(map[string]struct{}, len(baselineRels))
	for _, rel := range baselineRels {
		baselineSet[filepath.Clean(rel)] = struct{}{}
	}

	var res RestoreResult

	// Delete files the run added: present now, absent from the snapshot. Excluded
	// paths are never deleted (defense in depth on top of the caller's filter).
	for _, rel := range currentRels {
		rel = filepath.Clean(rel)
		// Defense in depth on this exported, multi-root seam: never act on a path
		// that escapes the root (callers produce in-tree paths, but a crafted rel
		// must not let a delete reach outside the workspace).
		if rel == ".." || hasDotDotPrefix(rel) || filepath.IsAbs(rel) {
			continue
		}
		if _, kept := baselineSet[rel]; kept {
			continue
		}
		if isUnderAnyPrefix(rel, excludes) {
			continue
		}
		abs := filepath.Join(root, rel)
		// Only delete REGULAR files. The baseline snapshot captures regular files
		// only (snapshotFiles skips symlinks/devices, and ls-files also surfaces
		// submodule gitlinks), so a non-regular path is never a run "addition" we
		// could restore — deleting it would lose a pre-existing symlink or break a
		// submodule. Leaving it untouched is the correct "preserve" behavior.
		info, err := os.Lstat(abs)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return res, fmt.Errorf("git restore: inspecting %q: %w", rel, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return res, fmt.Errorf("git restore: removing added file %q: %w", rel, err)
		}
		res.Deleted++
		pruneEmptyParents(root, filepath.Dir(abs))
	}

	// Copy every snapshot file back over the working tree (undo modifies + re-add
	// deletes). copyFileInto recreates parent dirs as needed.
	for _, rel := range baselineRels {
		if isUnderAnyPrefix(rel, excludes) {
			continue // never write into an excluded area
		}
		src := filepath.Join(snapshotDir, rel)
		if _, err := copyFileInto(src, rel, root); err != nil {
			return res, fmt.Errorf("git restore: %w", err)
		}
		res.Restored++
	}

	return res, nil
}

// currentTreeRels enumerates the working tree's tracked + untracked-non-ignored
// files (read-only git), de-duplicated and filtered by the gateway's exclude
// prefixes — the same enumeration CaptureBaseline uses, so "current minus baseline"
// is a like-for-like set difference.
func (g *Gateway) currentTreeRels(ctx context.Context) ([]string, error) {
	tracked, err := g.TrackedFiles(ctx)
	if err != nil {
		return nil, fmt.Errorf("git restore: enumerating tracked files: %w", err)
	}
	untracked, err := g.UntrackedFiles(ctx)
	if err != nil {
		return nil, fmt.Errorf("git restore: enumerating untracked files: %w", err)
	}
	return g.filterExcluded(dedupePaths(append(tracked, untracked...))), nil
}

// snapshotRels lists the regular files under snapshotDir as cleaned,
// snapshot-relative paths (the same layout CaptureBaseline wrote: <dir>/<rel>). A
// missing snapshot dir is an error — a revert with no baseline to restore from is a
// programming bug, not a silent no-op.
func snapshotRels(snapshotDir string) ([]string, error) {
	info, err := os.Stat(snapshotDir)
	if err != nil {
		return nil, fmt.Errorf("git restore: reading snapshot dir %q: %w", snapshotDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("git restore: snapshot path %q is not a directory", snapshotDir)
	}

	var rels []string
	err = filepath.WalkDir(snapshotDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("git restore: walking snapshot %q: %w", path, err)
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(snapshotDir, path)
		if err != nil {
			return fmt.Errorf("git restore: relativizing %q: %w", path, err)
		}
		rels = append(rels, filepath.Clean(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rels, nil
}

// pruneEmptyParents removes dir and its ancestors up to (but not including)
// repoRoot while they are empty, cleaning up directories a deletion emptied. It is
// best-effort: a non-empty dir (or any error) stops the walk. It never removes
// repoRoot itself.
func pruneEmptyParents(repoRoot, dir string) {
	for {
		if dir == repoRoot || !isUnder(repoRoot, dir) {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// isUnder reports whether path is repoRoot or nested beneath it.
func isUnder(repoRoot, path string) bool {
	rel, err := filepath.Rel(repoRoot, path)
	if err != nil {
		return false
	}
	return rel != ".." && !hasDotDotPrefix(rel)
}

// isUnderAnyPrefix reports whether the cleaned relative path lies at or under any
// of the prefixes (segment-boundary match), mirroring Gateway.isExcluded but over
// an explicit prefix list.
func isUnderAnyPrefix(rel string, prefixes []string) bool {
	clean := filepath.Clean(rel)
	for _, pre := range prefixes {
		if clean == pre || hasPathPrefix(clean, pre) {
			return true
		}
	}
	return false
}

// hasPathPrefix reports whether clean is nested under pre (pre + separator + ...).
func hasPathPrefix(clean, pre string) bool {
	p := pre + string(filepath.Separator)
	return len(clean) > len(p) && clean[:len(p)] == p
}

// mergePrefixes unions two already-cleaned prefix lists, de-duplicating.
func mergePrefixes(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, p := range append(append([]string{}, a...), b...) {
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}
