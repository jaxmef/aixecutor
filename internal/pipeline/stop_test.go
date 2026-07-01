package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jaxmef/aixecutor/internal/config"
	"github.com/jaxmef/aixecutor/internal/harness"
	"github.com/jaxmef/aixecutor/internal/prompt"
	"github.com/jaxmef/aixecutor/internal/run"
)

// TestStopCancelsInFlightExecutor proves the immediate-stop path: a stop requested
// while the executor is blocking mid-subtask cancels the run context (unwinding the
// blocking subprocess), Run returns promptly with a cancellation error, and the
// interrupted subtask is left non-terminal (implementing) — re-runnable, never failed.
func TestStopCancelsInFlightExecutor(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.Execution.Parallel = false

	store, r := newSchedRun(t, cfg, []run.Subtask{pending("st-01", nil, "a/x.go")})

	// The executor blocks until ctx is cancelled (PushDelay honors cancellation), so
	// the subtask is genuinely in-flight when the stop lands.
	h := harness.NewMock("executor")
	h.PushDelay(30*time.Second, harness.Result{Text: "done"})

	errCh := runSchedulerAsync(t, cfg, store, r, newFakeGit(t.TempDir()), h,
		WithStopCheck(func() bool { return store.StopRequested(r.ID) }),
		WithStopPollInterval(5*time.Millisecond),
	)

	// Wait for the executor to actually start blocking, then request the stop.
	waitUntil(t, func() bool { return h.CallCount() > 0 }, "executor to start")
	if err := store.RequestStop(r.ID); err != nil {
		t.Fatal(err)
	}

	err := awaitRun(t, errCh)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err = %v, want context.Canceled", err)
	}

	st, _ := r.SubtaskByID("st-01")
	if st.Status.IsTerminal() {
		t.Fatalf("subtask left terminal %q; want non-terminal (re-runnable)", st.Status)
	}
	if st.Status != run.SubtaskImplementing {
		t.Errorf("subtask status = %q, want implementing (interrupted mid-executor)", st.Status)
	}
	if store.StopRequested(r.ID) {
		t.Error("stop marker not cleared: the watcher must ack (ClearStop) on stop")
	}
}

// TestStopDuringReviewLeavesSubtaskNonTerminal proves the review-hook guard: a stop
// that lands while the review step is running does NOT mark the subtask failed. The
// review hook transitions the subtask to reviewing and blocks until cancellation;
// with the ctx.Err() guard in executeSubtask the subtask stays reviewing (re-runnable).
func TestStopDuringReviewLeavesSubtaskNonTerminal(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.Execution.Parallel = false

	store, r := newSchedRun(t, cfg, []run.Subtask{pending("st-01", nil, "a/x.go")})

	h := harness.NewMock("executor")
	h.DefaultResult = harness.Result{Text: "implemented"} // executor returns immediately.

	entered := make(chan struct{})
	reviewHook := func(ctx context.Context, _ run.Subtask, commit CommitFunc) error {
		if err := commit(func(st *run.Subtask) { st.Status = run.SubtaskReviewing }); err != nil {
			return err
		}
		close(entered)
		<-ctx.Done() // block in review until the stop cancels the run context.
		return context.Canceled
	}

	errCh := runSchedulerAsync(t, cfg, store, r, newFakeGit(t.TempDir()), h,
		WithReviewHook(reviewHook),
		WithStopCheck(func() bool { return store.StopRequested(r.ID) }),
		WithStopPollInterval(5*time.Millisecond),
	)

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("review hook was never entered")
	}
	if err := store.RequestStop(r.ID); err != nil {
		t.Fatal(err)
	}

	err := awaitRun(t, errCh)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err = %v, want context.Canceled", err)
	}

	st, _ := r.SubtaskByID("st-01")
	if st.Status == run.SubtaskFailed {
		t.Fatalf("subtask marked failed on a review-time stop; want non-terminal")
	}
	if st.Status != run.SubtaskReviewing {
		t.Errorf("subtask status = %q, want reviewing (interrupted mid-review)", st.Status)
	}
}

// runSchedulerAsync wires a Scheduler and runs it in a goroutine, returning a channel
// that receives Run's error. Unlike runScheduler it does not block, so the test can
// drive a stop while the run is in flight.
func runSchedulerAsync(t *testing.T, cfg config.Config, store *run.Store, r *run.Run, fg gitGateway, h harness.Harness, opts ...SchedulerOption) <-chan error {
	t.Helper()
	reg := registryWith(t, cfg, h)
	base := []SchedulerOption{WithSchedulerOutput(&strings.Builder{})}
	s, err := NewScheduler(r, cfg, reg, fg, store, prompt.NewRenderer(), append(base, opts...)...)
	if err != nil {
		t.Fatalf("new scheduler: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(context.Background()) }()
	return errCh
}

// awaitRun waits for the scheduler goroutine to return, failing if it does not stop
// promptly after a stop request (the whole point of the immediate-stop path).
func awaitRun(t *testing.T, errCh <-chan error) error {
	t.Helper()
	select {
	case err := <-errCh:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return promptly after stop")
		return nil
	}
}

// waitUntil polls cond until it holds or a short deadline elapses.
func waitUntil(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
