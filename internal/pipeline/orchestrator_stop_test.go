package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jaxmef/aixecutor/internal/harness"
	"github.com/jaxmef/aixecutor/internal/run"
)

// TestOrchestratorStopAbortsAndResumes proves the end-to-end immediate-stop path
// through the orchestrator: a run whose second subtask's executor is blocking is sent
// a stop marker; the scheduler's watcher cancels in-flight work, the orchestrator
// persists `aborted` and returns ErrAborted (leaving the interrupted subtask
// re-runnable), and a later Resume completes the run WITHOUT re-running the already-done
// subtask.
func TestOrchestratorStopAbortsAndResumes(t *testing.T) {
	cfg := orchCfg() // serial execution: st-01 then st-02 (st-02 deps st-01).
	hs := fullPipelineHarnesses()
	w := hs.exec.(*writingHarness)
	// call-1 (st-01) returns at once; call-2 (st-02) blocks until the stop cancels the
	// run context (PushDelay honors cancellation). The default result covers the
	// resume re-run of st-02.
	w.mock.PushText("done")
	w.mock.PushDelay(30*time.Second, harness.Result{Text: "done"})

	o, store, _, _ := newOrchTest(t, cfg, hs)

	type startResult struct {
		r   *run.Run
		err error
	}
	resCh := make(chan startResult, 1)
	go func() {
		r, err := o.Start(context.Background(), "build the feature")
		resCh <- startResult{r, err}
	}()

	// Wait for st-02's executor to start blocking (call 2), then resolve the run id and
	// request the stop.
	waitUntil(t, func() bool { return w.callCount() >= 2 }, "st-02 executor to start")
	seed, err := store.Load(run.LatestSentinel)
	if err != nil {
		t.Fatalf("resolve latest run: %v", err)
	}
	if err := store.RequestStop(seed.ID); err != nil {
		t.Fatal(err)
	}

	var start startResult
	select {
	case start = <-resCh:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return promptly after stop")
	}
	if !errors.Is(start.err, ErrAborted) {
		t.Fatalf("Start err = %v, want ErrAborted", start.err)
	}

	aborted, err := store.Load(seed.ID)
	if err != nil {
		t.Fatalf("reload aborted run: %v", err)
	}
	if aborted.Status != run.StatusAborted {
		t.Errorf("status = %q, want aborted", aborted.Status)
	}
	st1, _ := aborted.SubtaskByID("st-01")
	st2, _ := aborted.SubtaskByID("st-02")
	if st1.Status != run.SubtaskDone {
		t.Errorf("st-01 = %q, want done (finished before the stop)", st1.Status)
	}
	if st2.Status.IsTerminal() {
		t.Errorf("st-02 = %q, want non-terminal (interrupted, re-runnable)", st2.Status)
	}
	if store.StopRequested(seed.ID) {
		t.Error("stop marker not cleared: the watcher must ack (ClearStop)")
	}

	callsBeforeResume := w.callCount()

	done, err := o.Resume(context.Background(), seed.ID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if done.Status != run.StatusCompleted {
		t.Errorf("status = %q, want completed", done.Status)
	}
	for _, st := range done.Subtasks {
		if st.Status != run.SubtaskDone {
			t.Errorf("subtask %s = %q, want done", st.ID, st.Status)
		}
	}
	// Resume re-ran only the interrupted st-02 (one more executor call), never the
	// already-done st-01.
	if got := w.callCount() - callsBeforeResume; got != 1 {
		t.Errorf("executor ran %d extra time(s) on resume; want 1 (only st-02 re-run)", got)
	}
}
