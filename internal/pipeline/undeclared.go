package pipeline

import (
	"context"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jaxmef/aixecutor/internal/git"
	"github.com/jaxmef/aixecutor/internal/run"
)

// collectDeclaredGlobs returns the de-duplicated union of every subtask's declared
// Files globs. It is computed once in NewScheduler because Files are immutable after
// planning, so the union is a static, race-free input to undeclared-edit detection.
func collectDeclaredGlobs(subtasks []run.Subtask) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, st := range subtasks {
		for _, f := range st.Files {
			p := normSlash(filepath.ToSlash(f))
			if p == "" || p == "." {
				continue
			}
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// pathDeclared reports whether the (repo-relative) path is owned by any of the
// declared ownership patterns. A glob pattern (with `*`/`**`/`?`) matches via the
// project glob engine; a literal pattern matches the path exactly OR as a directory
// prefix — a declared directory owns everything beneath it, mirroring how expandFiles
// / SnapshotPaths treat a declared directory as its whole subtree.
func pathDeclared(path string, patterns []string) bool {
	p := normSlash(filepath.ToSlash(path))
	for _, pat := range patterns {
		if hasGlobMeta(pat) {
			if matchGlob(pat, p) {
				return true
			}
			continue
		}
		if p == pat || strings.HasPrefix(p, pat+"/") {
			return true
		}
	}
	return false
}

// undeclaredChanges returns the subset of changed paths owned by NONE of patterns,
// preserving changed's order (already sorted by Manifest.Changed).
func undeclaredChanges(changed, patterns []string) []string {
	var out []string
	for _, c := range changed {
		if !pathDeclared(c, patterns) {
			out = append(out, c)
		}
	}
	return out
}

// mergeUndeclared unions prior and next into a de-duplicated, sorted slice, so
// repeated executor passes (initial + remediation) accumulate rather than clobber the
// recorded set.
func mergeUndeclared(prior, next []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range append(append([]string(nil), prior...), next...) {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// recordUndeclaredEdits is the after-half of the best-effort undeclared-edit check
// wrapped around invokeExecutor. Given the before-manifest captured pre-invocation, it
// takes an after-manifest, diffs them, subtracts every subtask's declared globs, and —
// if anything remains — warns the user and records the paths on the subtask via the
// commit seam (serialized by the run-state owner). Every failure is logged and
// swallowed: it never fails the subtask, never touches diff.patch scope or persistDiff.
func (s *Scheduler) recordUndeclaredEdits(ctx context.Context, id, workDir string, before git.Manifest) {
	after, err := s.git.Manifest(ctx, workDir)
	if err != nil {
		s.progress.Logf("subtask %s: skipping undeclared-edit detection (after-manifest failed): %v", id, err)
		return
	}
	undeclared := undeclaredChanges(before.Changed(after), s.declaredGlobs)
	if len(undeclared) == 0 {
		return
	}
	s.progress.Logf("subtask %s: executor changed %d path(s) outside its declared files (planner may have under-declared): %s",
		id, len(undeclared), strings.Join(undeclared, ", "))
	if err := s.commitSubtask(id, func(st *run.Subtask) {
		st.Undeclared = mergeUndeclared(st.Undeclared, undeclared)
	}); err != nil {
		s.progress.Logf("subtask %s: recording undeclared edits failed: %v", id, err)
	}
}
