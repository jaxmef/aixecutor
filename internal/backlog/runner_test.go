package backlog

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// recorder captures the order tickets are run, and serves scripted outcomes.
type recorder struct {
	ran      []string
	outcomes map[string]Outcome // per-id; absent → clean completed
	errs     map[string]error   // per-id hard run error
}

func (rec *recorder) fn() TicketRunner {
	return func(_ context.Context, t Ticket) (Outcome, error) {
		rec.ran = append(rec.ran, t.ID)
		if e, ok := rec.errs[t.ID]; ok {
			return Outcome{RunID: "run-" + t.ID}, e
		}
		o, ok := rec.outcomes[t.ID]
		if !ok {
			o = Outcome{Completed: true, Clean: true}
		}
		o.RunID = "run-" + t.ID
		return o, nil
	}
}

// fanGraph builds A → {B, C}: B and C both depend on A.
func fanGraph(t *testing.T) *Graph {
	t.Helper()
	g, err := BuildGraph([]Ticket{tk("A"), tk("B", "A"), tk("C", "A")})
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	return g
}

func newState() *State { return &State{Tickets: map[string]*TicketState{}} }

func TestRunnerAutoRunsAllInOrder(t *testing.T) {
	rec := &recorder{}
	statePath := filepath.Join(t.TempDir(), backlogStatePathName())
	r := NewRunner(fanGraph(t), newState(), statePath, GateAuto, rec.fn(), nil)

	sum, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !sum.Exhausted {
		t.Errorf("expected exhausted backlog, remaining=%v", sum.Remaining)
	}
	if got := rec.ran; !equal(got, []string{"A", "B", "C"}) {
		t.Errorf("ran order = %v, want [A B C]", got)
	}
	if !equal(sum.Completed, []string{"A", "B", "C"}) {
		t.Errorf("completed = %v", sum.Completed)
	}
}

func TestRunnerStopOnFinding(t *testing.T) {
	rec := &recorder{outcomes: map[string]Outcome{
		"B": {Completed: true, Clean: false, Unresolved: 2},
	}}
	state := newState()
	r := NewRunner(fanGraph(t), state, filepath.Join(t.TempDir(), "s.yaml"), GateStopOnFinding, rec.fn(), nil)

	sum, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.NeedsReview != "B" {
		t.Errorf("NeedsReview = %q, want B", sum.NeedsReview)
	}
	// A ran and is done; B ran and stopped; C (dep A done, but run stopped) never ran.
	if !equal(rec.ran, []string{"A", "B"}) {
		t.Errorf("ran = %v, want [A B]", rec.ran)
	}
	if state.Tickets["B"].Status != TicketReview {
		t.Errorf("B status = %q, want review", state.Tickets["B"].Status)
	}
	if state.Tickets["B"].Unresolved != 2 {
		t.Errorf("B unresolved = %d, want 2", state.Tickets["B"].Unresolved)
	}
}

func TestRunnerStopsOnFailedTicketAndBlocksDependents(t *testing.T) {
	rec := &recorder{outcomes: map[string]Outcome{
		"A": {Completed: false}, // did not complete
	}}
	state := newState()
	r := NewRunner(fanGraph(t), state, filepath.Join(t.TempDir(), "s.yaml"), GateAuto, rec.fn(), nil)

	sum, err := r.Run(context.Background())
	if err == nil {
		t.Fatal("expected an error when a ticket fails")
	}
	if sum.Failed != "A" {
		t.Errorf("Failed = %q, want A", sum.Failed)
	}
	// A's dependents must not run.
	if !equal(rec.ran, []string{"A"}) {
		t.Errorf("ran = %v, want [A] (dependents blocked)", rec.ran)
	}
	if state.Tickets["A"].Status != TicketFailed {
		t.Errorf("A status = %q, want failed", state.Tickets["A"].Status)
	}
}

func TestRunnerHardErrorStops(t *testing.T) {
	rec := &recorder{errs: map[string]error{"A": errors.New("agent exploded")}}
	state := newState()
	r := NewRunner(fanGraph(t), state, filepath.Join(t.TempDir(), "s.yaml"), GateAuto, rec.fn(), nil)

	_, err := r.Run(context.Background())
	if err == nil {
		t.Fatal("expected the hard error to stop the run")
	}
	if state.Tickets["A"].Status != TicketFailed {
		t.Errorf("A status = %q, want failed", state.Tickets["A"].Status)
	}
}

func TestRunnerManualPausesEachTicketAndResumes(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "s.yaml")
	g := fanGraph(t)

	// First invocation: runs A, then pauses (manual gate).
	rec1 := &recorder{}
	state1, err := LoadState(statePath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	r1 := NewRunner(g, state1, statePath, GateManual, rec1.fn(), nil)
	sum1, err := r1.Run(context.Background())
	if err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	if !sum1.Paused || !equal(rec1.ran, []string{"A"}) {
		t.Fatalf("first run: paused=%v ran=%v, want paused after [A]", sum1.Paused, rec1.ran)
	}

	// Second invocation: fresh state loaded from disk — A is done and must NOT
	// re-run; B runs next, then pauses again.
	rec2 := &recorder{}
	state2, err := LoadState(statePath)
	if err != nil {
		t.Fatalf("LoadState 2: %v", err)
	}
	if !state2.isDone("A") {
		t.Fatal("A should be persisted as done after the first run")
	}
	r2 := NewRunner(g, state2, statePath, GateManual, rec2.fn(), nil)
	sum2, err := r2.Run(context.Background())
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if !equal(rec2.ran, []string{"B"}) {
		t.Errorf("second run ran = %v, want [B] (A not re-run)", rec2.ran)
	}
	if !sum2.Paused {
		t.Errorf("second run should pause after B")
	}
}

func TestRunnerResumeSkipsDoneAcrossReload(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "s.yaml")
	g := fanGraph(t)

	// Run everything under auto, persisting to disk.
	rec1 := &recorder{}
	s1, _ := LoadState(statePath)
	if _, err := NewRunner(g, s1, statePath, GateAuto, rec1.fn(), nil).Run(context.Background()); err != nil {
		t.Fatalf("Run 1: %v", err)
	}

	// Re-run from reloaded state: nothing left, nothing re-run.
	rec2 := &recorder{}
	s2, _ := LoadState(statePath)
	sum, err := NewRunner(g, s2, statePath, GateAuto, rec2.fn(), nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if len(rec2.ran) != 0 {
		t.Errorf("resume re-ran %v, want nothing", rec2.ran)
	}
	if !sum.Exhausted {
		t.Errorf("expected exhausted on resume")
	}
}

func TestRunnerAutoAdvancesThroughUncleanTicket(t *testing.T) {
	rec := &recorder{outcomes: map[string]Outcome{
		"B": {Completed: true, Clean: false, Unresolved: 1},
	}}
	r := NewRunner(fanGraph(t), newState(), filepath.Join(t.TempDir(), "s.yaml"), GateAuto, rec.fn(), nil)
	sum, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// auto advances even through B's unresolved findings → all three run.
	if !equal(rec.ran, []string{"A", "B", "C"}) {
		t.Errorf("ran = %v, want [A B C] (auto ignores findings)", rec.ran)
	}
	if !sum.Exhausted {
		t.Errorf("expected exhausted under auto")
	}
}

func TestRunnerAbortLeavesTicketResumable(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "s.yaml")
	ctx, cancel := context.WithCancel(context.Background())
	// The runner func cancels mid-run to simulate Ctrl-C during ticket A.
	rec := &recorder{}
	run := func(c context.Context, tk Ticket) (Outcome, error) {
		rec.ran = append(rec.ran, tk.ID)
		cancel()
		return Outcome{RunID: "run-" + tk.ID}, context.Canceled
	}
	state := newState()
	r := NewRunner(fanGraph(t), state, statePath, GateAuto, run, nil)

	_, err := r.Run(ctx)
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("Run err = %v, want ErrAborted", err)
	}
	// The interrupted ticket is left Ready (not failed) so it re-runs on resume.
	if state.Tickets["A"].Status != TicketReady {
		t.Errorf("A status = %q, want ready (resumable)", state.Tickets["A"].Status)
	}
}

// TestRunnerReviewTicketRerunsOnResume locks the "fix the tree, re-run" contract:
// a ticket stopped in the review state (stop-on-finding) is NOT done, so the next
// invocation re-runs it; once it comes back clean the backlog advances past it.
func TestRunnerReviewTicketRerunsOnResume(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "s.yaml")
	g := fanGraph(t)

	// Run #1: A clean, B leaves findings → stop-on-finding parks B in review.
	rec1 := &recorder{outcomes: map[string]Outcome{"B": {Completed: true, Clean: false, Unresolved: 1}}}
	s1, _ := LoadState(statePath)
	r1 := NewRunner(g, s1, statePath, GateStopOnFinding, rec1.fn(), nil)
	sum1, err := r1.Run(context.Background())
	if err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	if sum1.NeedsReview != "B" {
		t.Fatalf("run 1 NeedsReview = %q, want B", sum1.NeedsReview)
	}

	// Run #2 (resume): B is now clean. A must NOT re-run (done); B re-runs and then
	// C runs to exhaustion.
	rec2 := &recorder{}
	s2, _ := LoadState(statePath)
	if s2.isDone("A") != true {
		t.Fatal("A should be done after run 1")
	}
	r2 := NewRunner(g, s2, statePath, GateAuto, rec2.fn(), nil)
	sum2, err := r2.Run(context.Background())
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if !equal(rec2.ran, []string{"B", "C"}) {
		t.Errorf("resume ran = %v, want [B C] (B re-runs, A skipped)", rec2.ran)
	}
	if !sum2.Exhausted {
		t.Errorf("expected exhausted after B re-runs clean")
	}
}

func backlogStatePathName() string { return "backlog-state.yaml" }

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
