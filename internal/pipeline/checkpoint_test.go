package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jaxmef/aixecutor/internal/config"
	"github.com/jaxmef/aixecutor/internal/harness"
	"github.com/jaxmef/aixecutor/internal/run"
)

// TestSchedulerPausesAtBoundaryPreservingDone proves the AIX-0016 pause is honored
// at a SAFE subtask boundary: a pause requested after the first subtask completes
// stops the scheduler with run.yaml consistent — st-01 done, st-02 still pending —
// and returns ErrPaused, never mid-subtask-write.
func TestSchedulerPausesAtBoundaryPreservingDone(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.Execution.Parallel = false // serial: one subtask per batch.

	subs := []run.Subtask{
		pending("st-01", nil, "a/x.go"),
		pending("st-02", nil, "b/y.go"),
	}
	store, r := newSchedRun(t, cfg, subs)

	h := harness.NewMock("executor")
	h.DefaultResult = harness.Result{Text: "done"}

	// Pause check: false on the first boundary (let st-01 run), true on the second
	// (pause before st-02).
	calls := 0
	pause := func() bool { calls++; return calls >= 2 }

	err := runScheduler(t, cfg, store, r, newFakeGit(t.TempDir()), h, WithPauseCheck(pause))
	if !errors.Is(err, ErrPaused) {
		t.Fatalf("scheduler err = %v, want ErrPaused", err)
	}
	if r.Status != run.StatusPaused {
		t.Errorf("run status = %q, want paused", r.Status)
	}
	st1, _ := r.SubtaskByID("st-01")
	st2, _ := r.SubtaskByID("st-02")
	if st1.Status != run.SubtaskDone {
		t.Errorf("st-01 status = %q, want done (it finished before the pause)", st1.Status)
	}
	if st2.Status != run.SubtaskPending {
		t.Errorf("st-02 status = %q, want pending (never started)", st2.Status)
	}
	// Persisted state matches (consistent run.yaml at the pause).
	reloaded, err := store.Load(r.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Status != run.StatusPaused {
		t.Errorf("persisted status = %q, want paused", reloaded.Status)
	}
}

// TestSchedulerAllDoneFinalizesDespitePauseRequest proves the terminal check wins
// over the pause check: a pause request that lands when every subtask is already
// done finalizes the run (→ ready for senior review) rather than stranding it paused.
func TestSchedulerAllDoneFinalizesDespitePauseRequest(t *testing.T) {
	cfg := config.Default()
	subs := []run.Subtask{
		{ID: "st-01", Title: "a", Status: run.SubtaskDone},
		{ID: "st-02", Title: "b", Status: run.SubtaskDone},
	}
	store, r := newSchedRun(t, cfg, subs)

	h := harness.NewMock("executor")
	h.DefaultResult = harness.Result{Text: "done"}

	// pauseCheck is always true, but every subtask is already terminal.
	err := runScheduler(t, cfg, store, r, newFakeGit(t.TempDir()), h, WithPauseCheck(func() bool { return true }))
	if err != nil {
		t.Fatalf("scheduler err = %v, want nil (finalized, not paused)", err)
	}
	if r.Status == run.StatusPaused {
		t.Error("an all-done run must not be marked paused")
	}
}

// TestPauseThenResumeContinues proves the clarify-only path: a paused run resumes
// and runs the remaining subtasks to completion, NOT re-running finished work.
func TestPauseThenResumeContinues(t *testing.T) {
	cfg := orchCfg()
	cfg.Pipeline.AutostartExecution = false // stop after planning so we know the id.
	hs := fullPipelineHarnesses()
	o, store, _, _ := newOrchTest(t, cfg, hs)
	ctx := context.Background()

	r, err := o.Start(ctx, "build the feature")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if r.Status != run.StatusPlanned {
		t.Fatalf("after planning, status = %q, want planned", r.Status)
	}

	// Request a pause, then resume: execution pauses at the first boundary (before
	// any subtask runs), so the executor is never invoked.
	if err := store.RequestPause(r.ID); err != nil {
		t.Fatal(err)
	}
	_, err = o.Resume(ctx, r.ID)
	if !errors.Is(err, ErrPaused) {
		t.Fatalf("resume into a pending pause: err = %v, want ErrPaused", err)
	}
	paused, _ := store.Load(r.ID)
	if paused.Status != run.StatusPaused {
		t.Errorf("status = %q, want paused", paused.Status)
	}
	if hs.exec.(*writingHarness).callCount() != 0 {
		t.Errorf("executor ran %d times before the pause; want 0", hs.exec.(*writingHarness).callCount())
	}

	// Resume again (clarify → continue): the pause request was cleared, so execution
	// runs both subtasks to completion.
	done, err := o.Resume(ctx, r.ID)
	if err != nil {
		t.Fatalf("second Resume: %v", err)
	}
	if done.Status != run.StatusCompleted {
		t.Errorf("status = %q, want completed", done.Status)
	}
	if hs.exec.(*writingHarness).callCount() != 2 {
		t.Errorf("executor ran %d times; want 2 (both subtasks, none re-run)", hs.exec.(*writingHarness).callCount())
	}
}

// TestAmendRevertsAndRestarts proves the amend path: from a paused run, Amend
// reverts the working tree to the run baseline (RestoreTree against the persisted
// baseline dir) and restarts execution from the amended docs/subtasks.yaml.
func TestAmendRevertsAndRestarts(t *testing.T) {
	cfg := orchCfg()
	cfg.Pipeline.AutostartExecution = false
	hs := fullPipelineHarnesses()
	o, store, fg, _ := newOrchTest(t, cfg, hs)
	ctx := context.Background()

	r, err := o.Start(ctx, "build the feature")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := store.RequestPause(r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Resume(ctx, r.ID); !errors.Is(err, ErrPaused) {
		t.Fatalf("expected ErrPaused, got %v", err)
	}

	// The user amends the plan to a single subtask. Overwrite docs/subtasks.yaml.
	amended := "subtasks:\n" +
		"  - id: st-amended\n" +
		"    title: \"Amended step\"\n" +
		"    description: \"Do the amended work.\"\n" +
		"    deps: []\n" +
		"    files: [\"internal/example/type.go\"]\n" +
		"    acceptance:\n" +
		"      - \"It works.\"\n"
	if err := os.WriteFile(filepath.Join(store.DocsDir(r.ID), "subtasks.yaml"), []byte(amended), 0o644); err != nil {
		t.Fatal(err)
	}

	done, err := o.Amend(ctx, r.ID)
	if err != nil {
		t.Fatalf("Amend: %v", err)
	}

	// The revert ran against the persisted run baseline.
	calls := fg.restoreTreeCalls()
	if len(calls) != 1 {
		t.Fatalf("RestoreTree called %d times, want 1", len(calls))
	}
	if calls[0].snapshotDir != r.Baseline.Dir {
		t.Errorf("RestoreTree snapshotDir = %q, want the run baseline %q", calls[0].snapshotDir, r.Baseline.Dir)
	}
	// The run restarted from the amended plan and completed.
	if done.Status != run.StatusCompleted {
		t.Errorf("status = %q, want completed", done.Status)
	}
	if len(done.Subtasks) != 1 || done.Subtasks[0].ID != "st-amended" {
		t.Errorf("subtasks not re-derived from the amended plan: %+v", done.Subtasks)
	}
	if done.Subtasks[0].Status != run.SubtaskDone {
		t.Errorf("amended subtask status = %q, want done", done.Subtasks[0].Status)
	}
}

// TestAmendRequiresPausedRun proves Amend refuses a run that is not paused.
func TestAmendRequiresPausedRun(t *testing.T) {
	cfg := orchCfg()
	hs := fullPipelineHarnesses()
	o, _, _, _ := newOrchTest(t, cfg, hs)
	ctx := context.Background()

	r, err := o.Start(ctx, "build the feature") // autostart on → runs to completion.
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if r.Status != run.StatusCompleted {
		t.Fatalf("precondition: status = %q, want completed", r.Status)
	}
	if _, err := o.Amend(ctx, r.ID); err == nil {
		t.Error("Amend on a completed run should error (not paused)")
	}
}
