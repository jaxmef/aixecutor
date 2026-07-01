package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jaxmef/aixecutor/internal/git"
	"github.com/jaxmef/aixecutor/internal/harness"
	"github.com/jaxmef/aixecutor/internal/prompt"
	"github.com/jaxmef/aixecutor/internal/run"
)

// executeSubtask runs a single subtask end-to-end (CLAUDE.md §3.3): it provisions
// isolation if configured, snapshots the subtask's declared paths, renders and
// invokes the executor harness in the right working directory, captures the
// per-subtask diff to subtasks/<id>/diff.patch, reconciles worktree changes back to
// the main tree, and drives the subtask through implementing → [review] → done.
// State is persisted (Store.Save, serialized by the run-state owner) after each transition.
//
// The returned error is non-nil only for a FATAL condition (context cancellation, a
// persistence failure). A failure of the subtask itself (executor errored, snapshot
// or diff failed) is recorded by marking the subtask SubtaskFailed and persisting
// it, then returning nil, so one subtask's failure does not abort the batch — the
// scheduler's deadlock/finalize logic surfaces it. The only exception is a
// persistence failure while recording the failure, which is returned as fatal.
func (s *Scheduler) executeSubtask(ctx context.Context, id string) error {
	st, ok := s.subtaskSnapshot(id)
	if !ok {
		return fmt.Errorf("pipeline: subtask %q vanished from the run", id)
	}
	// Skip already-done subtasks defensively (resume should keep them out of the
	// ready set, but never re-invoke the executor on a finished subtask).
	if st.Status == run.SubtaskDone {
		return nil
	}

	// Transition pending → implementing and persist before doing any work, so an
	// interruption here is recoverable (resume rewinds implementing → pending).
	if err := s.commitSubtask(id, func(st *run.Subtask) {
		st.Status = run.SubtaskImplementing
	}); err != nil {
		return fmt.Errorf("pipeline: marking subtask %q implementing: %w", id, err)
	}
	s.progress.SubtaskStarted(id, st.Title)

	// Initial execution: no prior findings (the remediation loop passes findings
	// in via runExecutor from the review hook).
	if _, err := s.runExecutor(ctx, id, nil); err != nil {
		// A per-subtask failure: record it and surface it through the run, not as a
		// fatal scheduler error. A context cancellation IS fatal — propagate it so
		// the run aborts rather than masquerading as a subtask failure.
		if ctx.Err() != nil {
			return fmt.Errorf("pipeline: subtask %q canceled: %w", id, ctx.Err())
		}
		if ferr := s.failSubtask(id, err); ferr != nil {
			return ferr // persisting the failure itself failed: fatal.
		}
		return nil
	}

	// Diff captured; hand off to the review step (no-op default in this ticket),
	// which drives the subtask to done. The hook gets a read-only snapshot and a
	// commit func that routes its mutation + persist through the state owner, so the
	// transition cannot race a concurrent marshal (-race clean).
	snap, ok := s.subtaskSnapshot(id)
	if !ok {
		return fmt.Errorf("pipeline: subtask %q vanished before review", id)
	}
	commit := func(mutate func(st *run.Subtask)) error {
		return s.commitSubtask(id, mutate)
	}
	if err := s.reviewHook(ctx, snap, commit); err != nil {
		// A cancellation during review is fatal, not a subtask failure: propagate it so
		// the subtask stays reviewing/implementing (re-runnable), never marked failed —
		// matching the executor-pass guard above.
		if ctx.Err() != nil {
			return fmt.Errorf("pipeline: subtask %q canceled: %w", id, ctx.Err())
		}
		if ferr := s.failSubtask(id, fmt.Errorf("review step: %w", err)); ferr != nil {
			return ferr
		}
		return nil
	}
	// Report the terminal subtask outcome (loops spent + any unresolved findings
	// carried forward) by reading the committed state, so the line is accurate
	// regardless of which review hook ran (the no-op hook or the real loop).
	if fin, ok := s.subtaskSnapshot(id); ok && fin.Status == run.SubtaskDone {
		s.progress.SubtaskDone(id, fin.Loops, len(fin.Unresolved))
	}
	return nil
}

// runExecutor performs one executor pass over a subtask and (re)captures its
// diff: provision a worktree (if isolation requires), snapshot-before, invoke the
// executor (with priorFindings injected into its prompt on a remediation pass),
// snapshot-after, compute and persist the diff to subtasks/<id>/diff.patch, and
// reconcile worktree changes back into the main tree. It returns the path of the
// persisted diff and a non-fatal error describing what went wrong; the caller
// decides how to record it.
//
// This is the single reusable executor entrypoint shared by BOTH the initial
// execution (executeSubtask, priorFindings == nil) and the subtask review loop's
// remediation step (review_subtask.go, priorFindings == the reviewer's open
// findings), so snapshot/diff logic lives in exactly one place. On remediation it
// OVERWRITES diff.patch with the new diff, which is what the next review round
// reads.
func (s *Scheduler) runExecutor(ctx context.Context, id string, priorFindings []Finding) (diffPath string, err error) {
	st, ok := s.subtaskSnapshot(id)
	if !ok {
		return "", fmt.Errorf("subtask %q not found", id)
	}

	workDir := s.git.RepoRoot()
	var cleanup func()
	if s.isolation() == isolationWorktree {
		wd, done, werr := s.provisionWorktree(ctx, st)
		if werr != nil {
			return "", werr
		}
		workDir = wd
		cleanup = done
		// Defer worktree teardown so it happens on EVERY exit path (success or any
		// error below), never leaking a worktree into the user's repo.
		defer cleanup()
	}

	// Expand the declared `files` globs (with `**`) against the working tree to the
	// concrete set of paths to snapshot. This is the AIX-0006 carry-forward: the
	// gateway's filepath.Glob cannot expand `**`, so we do it here and hand it
	// literal paths.
	paths, err := expandFiles(workDir, st.Files)
	if err != nil {
		return "", fmt.Errorf("expanding declared files for subtask %q: %w", id, err)
	}

	// SNAPSHOT-BEFORE: capture the declared paths' current content (the per-subtask
	// baseline) into a temp dir we own and clean up.
	beforeDir, err := os.MkdirTemp("", "aixecutor-st-before-*")
	if err != nil {
		return "", fmt.Errorf("creating before-snapshot dir for subtask %q: %w", id, err)
	}
	defer os.RemoveAll(beforeDir)
	beforeGW := s.gatewayFor(workDir)
	if _, err := beforeGW.SnapshotPaths(beforeDir, paths, nil); err != nil {
		return "", fmt.Errorf("snapshotting subtask %q before-state: %w", id, err)
	}

	// fingerprint the working tree immediately before/after the executor
	// runs so we can flag paths it changed that NO subtask declared (planner
	// under-declaration). This is a strict side-channel: best-effort, never alters the
	// per-subtask diff scope (still declared-globs only) below, never fails the subtask.
	// A before-manifest error disables the check for this pass (nil sentinel).
	beforeManifest, mErr := s.git.Manifest(ctx, workDir)
	if mErr != nil {
		s.progress.Logf("subtask %s: skipping undeclared-edit detection (before-manifest failed): %v", id, mErr)
		beforeManifest = nil
	}

	// Invoke the executor harness in the working directory, injecting any prior
	// reviewer findings so a remediation pass renders the "address these findings"
	// prompt (empty on the initial pass).
	res, err := s.invokeExecutor(ctx, st, workDir, priorFindings)
	if err != nil {
		return "", fmt.Errorf("executor failed for subtask %q: %w", id, err)
	}

	if beforeManifest != nil {
		s.recordUndeclaredEdits(ctx, id, workDir, beforeManifest)
	}

	// SNAPSHOT-AFTER: re-expand (the executor may have created new declared files)
	// and snapshot the post-execution content.
	afterPaths, err := expandFiles(workDir, st.Files)
	if err != nil {
		return "", fmt.Errorf("re-expanding declared files for subtask %q: %w", id, err)
	}
	afterDir, err := os.MkdirTemp("", "aixecutor-st-after-*")
	if err != nil {
		return "", fmt.Errorf("creating after-snapshot dir for subtask %q: %w", id, err)
	}
	defer os.RemoveAll(afterDir)
	afterGW := s.gatewayFor(workDir)
	if _, err := afterGW.SnapshotPaths(afterDir, afterPaths, nil); err != nil {
		return "", fmt.Errorf("snapshotting subtask %q after-state: %w", id, err)
	}

	// Compute the per-subtask diff (read-only `git diff --no-index`) and persist it.
	diff, err := s.git.DiffTrees(ctx, beforeDir, afterDir)
	if err != nil {
		return "", fmt.Errorf("diffing subtask %q: %w", id, err)
	}
	if err := s.persistDiff(id, diff.Patch); err != nil {
		return "", err
	}

	// Round numbering mirrors the review loop: the snapshot taken at the top of
	// runExecutor holds Loops BEFORE this pass, so round == st.Loops+1 makes
	// execution/round-N pair with reviews/round-N.
	if err := s.persistExecution(id, st.Loops+1, res, diff.Patch); err != nil {
		return "", err
	}

	// Reconcile: copy the files the executor changed in the worktree back into the
	// main tree (raw file I/O — no commits), so the main working tree reflects the
	// subtask's edits. No-op outside worktree isolation (workDir is the repo root).
	if s.isolation() == isolationWorktree {
		if err := reconcileChangedFiles(beforeDir, afterDir, s.git.RepoRoot()); err != nil {
			return "", fmt.Errorf("reconciling worktree changes for subtask %q: %w", id, err)
		}
	}
	return s.layout().SubtaskDiffFile(id), nil
}

// gatewayFor returns a gitGateway whose snapshot operations are rooted at dir. For
// the main tree (dir == repo root) it returns the scheduler's gateway unchanged.
// For a worktree it returns a thin wrapper that re-roots SnapshotPaths at the
// worktree path, since the gateway's RepoRoot is the main repo. Diffs and worktree
// management still go through the original gateway.
func (s *Scheduler) gatewayFor(dir string) gitGateway {
	if dir == s.git.RepoRoot() {
		return s.git
	}
	return rerootedGateway{gitGateway: s.git, root: dir}
}

// rerootedGateway re-roots SnapshotPaths at a different directory (a worktree)
// while delegating everything else to the wrapped gateway. It exists because the
// per-subtask before/after snapshots must read from the worktree, not the main
// repo, when worktree isolation is active.
type rerootedGateway struct {
	gitGateway
	root string
}

// RepoRoot reports the re-rooted directory so expandFiles and snapshotting resolve
// relative paths against the worktree.
func (g rerootedGateway) RepoRoot() string { return g.root }

// SnapshotPaths copies the (literal) paths relative to the re-rooted directory. It
// reimplements the gateway's literal-path copy against g.root rather than the main
// repo, using the same raw file I/O (no git), so the worktree's content is what gets
// snapshotted. Glob expansion has already happened in expandFiles, so every path
// here is literal. The returned git.Snapshot carries only Dir (the scheduler uses
// the snapshot dirs, not the file list).
func (g rerootedGateway) SnapshotPaths(dstDir string, paths []string, _ func(bytes int64)) (git.Snapshot, error) {
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return git.Snapshot{}, fmt.Errorf("snapshot: creating %q: %w", dstDir, err)
	}
	for _, rel := range paths {
		clean := filepath.Clean(rel)
		if clean == "." || clean == "" || filepath.IsAbs(clean) {
			continue
		}
		src := filepath.Join(g.root, clean)
		info, err := os.Lstat(src)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // declared-but-not-created: no before content.
			}
			return git.Snapshot{}, fmt.Errorf("snapshot: stat %q: %w", src, err)
		}
		if info.IsDir() {
			if err := copyTree(src, filepath.Join(dstDir, clean)); err != nil {
				return git.Snapshot{}, err
			}
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if err := copyFile(src, filepath.Join(dstDir, clean)); err != nil {
			return git.Snapshot{}, err
		}
	}
	return git.Snapshot{Dir: dstDir}, nil
}

// invokeExecutor renders the executor prompt for st and runs the executor harness
// in workDir with the executor role's model/permissionMode/timeout. priorFindings
// is empty on the initial pass and carries the reviewer's open findings on a
// remediation pass (driven by the subtask review loop, AIX-0011), which switches
// the executor prompt into its "address these findings" mode. A harness error is
// returned to the caller, which records the subtask as failed.
func (s *Scheduler) invokeExecutor(ctx context.Context, st run.Subtask, workDir string, priorFindings []Finding) (harness.Result, error) {
	promptText, err := s.renderer.Render(s.role.PromptTemplate, prompt.ExecutorContext{
		Task:           s.run.Task,
		Subtask:        toPromptSubtask(st),
		ContextExcerpt: s.ctxProv.ContextExcerpt(st),
		PriorFindings:  toPromptFindings(priorFindings),
		Baseline:       prompt.BaselineInfo{Description: baselineDescription},
	})
	if err != nil {
		return harness.Result{}, fmt.Errorf("rendering executor prompt: %w", err)
	}

	return s.executor.Run(ctx, harness.Request{
		Prompt:         promptText,
		Role:           "executor",
		Model:          s.role.Model,
		WorkDir:        workDir,
		PermissionMode: s.role.PermissionMode,
		Timeout:        s.role.Timeout.Std(),
	})
}

// provisionWorktree obtains a worktree manager (gated on git.policy: allow-worktree)
// and adds a worktree for the subtask, returning its path and a cleanup func that
// removes ALL worktrees the manager created. The cleanup is safe to defer
// immediately so a worktree is torn down on every exit path, including the executor
// erroring. A refusal (policy not allow-worktree) is surfaced with a clear error.
func (s *Scheduler) provisionWorktree(ctx context.Context, st run.Subtask) (workDir string, cleanup func(), err error) {
	wm, err := s.git.Worktree(s.cfg.Git.Policy)
	if err != nil {
		// The gateway already produces an actionable "set git.policy: allow-worktree"
		// message; wrap it with the isolation context.
		return "", func() {}, fmt.Errorf("worktree isolation for subtask %q: %w", st.ID, err)
	}
	cleanup = func() {
		if rerr := wm.RemoveAll(ctx); rerr != nil {
			s.progress.Logf("warning: cleaning up worktree(s) for subtask %s: %v", st.ID, rerr)
		}
	}

	path, err := wm.Add(ctx, worktreeName(st.ID))
	if err != nil {
		// Add records the path before running git, so cleanup still targets a partial
		// worktree; run it here since the caller has not yet deferred ours.
		cleanup()
		return "", func() {}, fmt.Errorf("provisioning worktree for subtask %q: %w", st.ID, err)
	}
	return path, cleanup, nil
}

// persistDiff writes the per-subtask diff to subtasks/<id>/diff.patch, creating the
// subtask artifact dir. It uses the run layout via the store so the path matches
// exactly where resume and the reviewer (AIX-0011) look. The write is plain file
// I/O; the diff was produced read-only by the gateway.
func (s *Scheduler) persistDiff(id, patch string) error {
	layout := s.layout()
	dir := layout.SubtaskDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating subtask dir for %q: %w", id, err)
	}
	if err := os.WriteFile(layout.SubtaskDiffFile(id), []byte(patch), 0o644); err != nil {
		return fmt.Errorf("writing diff.patch for subtask %q: %w", id, err)
	}
	return nil
}

// persistExecution writes a human-readable execution summary for one executor pass
// to subtasks/<id>/execution/round-N.md — the counterpart to the review loop's
// round files (execution/round-N pairs with reviews/round-N). It records the
// executor's summary text plus the role's harness/model/timeout, the pass's
// duration/exit code, a link to that round's diff.patch, and the files it touched.
// diff.patch stays the machine artifact; this is the readable one. The write
// overwrites the round file (like diff.patch) so resume stays idempotent.
func (s *Scheduler) persistExecution(id string, round int, res harness.Result, patch string) error {
	layout := s.layout()
	dir := layout.SubtaskExecutionsDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating execution dir for subtask %q: %w", id, err)
	}
	var title string
	if st, ok := s.subtaskSnapshot(id); ok {
		title = st.Title
	}
	md := renderExecutionRound(
		id, title, round,
		s.role.Harness, s.role.Model, s.role.PermissionMode, s.role.Timeout.Std(),
		res.Duration, res.ExitCode,
		changedFilesFromPatch(patch), res.Text,
	)
	if err := os.WriteFile(layout.SubtaskExecutionRoundFile(id, round), []byte(md), 0o644); err != nil {
		return fmt.Errorf("writing execution round %d for subtask %q: %w", round, id, err)
	}
	return nil
}

// changedFilesFromPatch extracts the repo-relative paths touched by a diff by
// scanning its `diff --git a/<path> b/<path>` headers and taking the `b/` path
// (already repo-relative). Order is preserved and duplicates dropped.
func changedFilesFromPatch(patch string) []string {
	const header = "diff --git "
	seen := map[string]struct{}{}
	var out []string
	for _, line := range strings.Split(patch, "\n") {
		if !strings.HasPrefix(line, header) {
			continue
		}
		fields := strings.Fields(line[len(header):])
		if len(fields) < 2 {
			continue
		}
		path := strings.TrimPrefix(fields[len(fields)-1], "b/")
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	return out
}

// failSubtask records a subtask's failure: it marks the subtask SubtaskFailed and
// records the cause, both folded into a single owner-side transition (failReq). The
// failure reason is printed for the user; it is kept out of run.yaml's schema (which
// has no per-subtask error field) but surfaced in the run-level error the scheduler
// returns. A persistence failure here is returned as fatal.
func (s *Scheduler) failSubtask(id string, cause error) error {
	s.progress.SubtaskFailed(id, cause.Error())
	reply := make(chan error, 1)
	err, ok := ask(s.actor, reply, failReq{id: id, cause: cause, reply: reply})
	if !ok {
		return errActorStopped
	}
	if err != nil {
		return fmt.Errorf("pipeline: recording subtask %q failure: %w", id, err)
	}
	return nil
}

// isolation returns the effective isolation policy, defaulting an empty value to
// non-overlapping (the schema default) so a hand-built config never yields an
// unknown mode.
func (s *Scheduler) isolation() string {
	iso := s.cfg.Pipeline.Execution.Isolation
	if iso == "" {
		return isolationNonOverlapping
	}
	return iso
}

// layout returns the run's artifact layout via the store, so artifact paths match
// Create/resume exactly.
func (s *Scheduler) layout() run.Layout {
	return run.Layout{RunsDir: s.store.RunsDir(), ID: s.run.ID, DocsSubdir: s.docsSubdir()}
}

// docsSubdir returns the configured docs subdir name (defaulting to "docs"), so the
// layout the scheduler builds resolves the same docs dir the store does.
func (s *Scheduler) docsSubdir() string {
	if s.cfg.Paths.DocsSubdir != "" {
		return s.cfg.Paths.DocsSubdir
	}
	return "docs"
}

// baselineDescription is the phrase the executor prompt uses to tell the agent how
// its change is judged. The per-subtask diff is measured against the subtask's
// declared paths as they were before the subtask ran.
const baselineDescription = "the working tree as it was before this subtask started"

// worktreeName derives a safe, single-segment worktree name from a subtask id. The
// gateway validates the name too, but normalizing here keeps the worktree directory
// readable.
func worktreeName(id string) string {
	return filepath.Base(filepath.Clean("/" + id))
}

// toPromptSubtask maps a run.Subtask onto the prompt package's SubtaskSpec (the
// fields a worker prompt needs), splitting the joined Acceptance string back into a
// list for the template. It keeps the prompt package free of any run/pipeline
// import (CLAUDE.md §3.1).
func toPromptSubtask(st run.Subtask) prompt.SubtaskSpec {
	return prompt.SubtaskSpec{
		ID:          st.ID,
		Title:       st.Title,
		Description: st.Description,
		Files:       st.Files,
		Acceptance:  splitAcceptance(st.Acceptance),
		ManualTest:  "",
	}
}

// splitAcceptance turns the run model's newline-bulleted Acceptance string back into
// individual criteria for the prompt, stripping the leading "- " bullet that
// joinAcceptance added. An empty string yields nil.
func splitAcceptance(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, line := range bytes.Split([]byte(s), []byte("\n")) {
		t := string(bytes.TrimSpace(line))
		t = trimBullet(t)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// trimBullet removes a leading "- " or "-" bullet from an acceptance line.
func trimBullet(s string) string {
	if len(s) >= 2 && s[0] == '-' && s[1] == ' ' {
		return s[2:]
	}
	if len(s) >= 1 && s[0] == '-' {
		return s[1:]
	}
	return s
}

// expandFiles expands a subtask's declared file globs against the tree rooted at
// root, returning the de-duplicated, sorted set of repo-relative paths to snapshot.
// It is the `**`-aware counterpart to the gateway's filepath.Glob expansion:
//
//   - A literal pattern (no `*`/`?`) is returned as-is, EVEN IF it does not yet
//     exist, so a file the subtask will create has an empty before-snapshot and
//     shows as an addition in the after-snapshot (matching the gateway's contract).
//   - A glob pattern (containing `*`/`**`/`?`) is matched against existing files by
//     walking the tree once; only existing matches contribute.
//
// The walk skips the .git directory (and any nested .git) so a worktree's or repo's
// VCS metadata is never snapshotted. Patterns that escape the root are rejected.
func expandFiles(root string, patterns []string) ([]string, error) {
	seen := map[string]struct{}{}
	var out []string
	add := func(p string) {
		p = filepath.Clean(p)
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}

	var globs []string
	for _, p := range patterns {
		clean := filepath.Clean(normSlash(p))
		if clean == "." || clean == "" {
			continue
		}
		if filepath.IsAbs(clean) || clean == ".." || hasDotDotPrefix(clean) {
			return nil, fmt.Errorf("declared file %q escapes the repository root", p)
		}
		if !hasGlobMeta(clean) {
			add(filepath.FromSlash(clean)) // literal: keep even if missing.
			continue
		}
		globs = append(globs, clean)
	}

	// Walk once for all glob patterns (only if there are any), matching each file.
	if len(globs) > 0 {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == ".git" {
					return filepath.SkipDir
				}
				return nil
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return rerr
			}
			relSlash := normSlash(rel)
			for _, g := range globs {
				if matchGlob(g, relSlash) {
					add(rel)
					break
				}
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walking %q to expand globs: %w", root, err)
		}
	}

	sort.Strings(out)
	return out, nil
}

// hasDotDotPrefix reports whether a cleaned, slash path begins with a ".." segment
// (would escape its root). Mirrors the gateway's defensive check so declared globs
// cannot read outside the tree.
func hasDotDotPrefix(clean string) bool {
	return clean == ".." || (len(clean) >= 3 && clean[0] == '.' && clean[1] == '.' && (clean[2] == '/' || clean[2] == filepath.Separator))
}

// reconcileChangedFiles copies files that the executor changed in a worktree back
// into the main tree. It compares the before-snapshot and after-snapshot of the
// subtask's declared paths and, for every file present after whose content differs
// from before (or that is new), copies it to the corresponding path under destRoot
// with raw file I/O — NO git, NO commits (CLAUDE.md §2 invariant 1; §4.3 worktree
// reconcile-by-copy).
//
// Conflict/deletion policy (documented caveat): a file DELETED in the worktree is
// NOT removed from the main tree, and concurrent edits to the same path are out of
// scope under the default non-overlapping isolation (which guarantees disjoint file
// sets). Worktree isolation with overlapping declared paths is the advanced case the
// user opts into; last-writer-wins by copy is the accepted v1 semantics.
func reconcileChangedFiles(beforeDir, afterDir, destRoot string) error {
	return filepath.WalkDir(afterDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(afterDir, path)
		if rerr != nil {
			return rerr
		}
		beforePath := filepath.Join(beforeDir, rel)
		changed, cerr := filesDiffer(beforePath, path)
		if cerr != nil {
			return cerr
		}
		if !changed {
			return nil
		}
		return copyFile(path, filepath.Join(destRoot, rel))
	})
}

// filesDiffer reports whether the files at a and b have different content. A
// missing "a" (the file is new in the after-snapshot) counts as different. It reads
// both files; per-subtask file sets are small, so this is fine.
func filesDiffer(a, b string) (bool, error) {
	ab, aerr := os.ReadFile(a)
	if aerr != nil {
		if errors.Is(aerr, fs.ErrNotExist) {
			return true, nil // new file in the worktree.
		}
		return false, fmt.Errorf("reading %q: %w", a, aerr)
	}
	bb, berr := os.ReadFile(b)
	if berr != nil {
		return false, fmt.Errorf("reading %q: %w", b, berr)
	}
	return !bytes.Equal(ab, bb), nil
}

// copyFile copies the regular file at src to dst, creating parent directories, with
// raw file I/O. Used for snapshotting (worktree re-root) and worktree reconcile.
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("creating dir for %q: %w", dst, err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("reading %q: %w", src, err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return fmt.Errorf("writing %q: %w", dst, err)
	}
	return nil
}

// copyTree recursively copies the regular files under srcDir into dstDir, preserving
// structure. Used by the re-rooted gateway when a declared path is a directory.
func copyTree(srcDir, dstDir string) error {
	return filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(srcDir, path)
		if rerr != nil {
			return rerr
		}
		return copyFile(path, filepath.Join(dstDir, rel))
	})
}
