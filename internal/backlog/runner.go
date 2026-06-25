package backlog

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// GateMode decides whether the runner advances to the next ticket after each run.
type GateMode string

const (
	// GateManual processes one ready ticket per invocation, then pauses: the human
	// inspects the working tree and re-runs to continue. The safest default and the
	// natural pairing point for the AIX-0016 review checkpoint.
	GateManual GateMode = "manual"
	// GateStopOnFinding advances through cleanly-reviewed tickets automatically but
	// stops the moment a ticket completes with unresolved senior-review findings.
	GateStopOnFinding GateMode = "stop-on-finding"
	// GateAuto runs the whole backlog unattended, advancing on any completed run
	// (even one with unresolved findings). Only a non-completing run stops it.
	GateAuto GateMode = "auto"
)

// ValidGate reports whether g is a recognized gating mode.
func ValidGate(g GateMode) bool {
	switch g {
	case GateManual, GateStopOnFinding, GateAuto:
		return true
	default:
		return false
	}
}

// ErrAborted signals a user-initiated, resumable stop (Ctrl-C) mid-backlog. The
// in-flight ticket is left Ready so a subsequent run re-runs it from scratch.
var ErrAborted = errors.New("backlog run aborted")

// Outcome is the structured result of running one ticket through the pipeline,
// reported by a TicketRunner. It is intentionally pipeline-agnostic so the backlog
// package needs no dependency on internal/run or internal/pipeline.
type Outcome struct {
	// RunID is the pipeline run id created for this ticket (for traceability).
	RunID string
	// Completed is true when the run reached terminal success (run status
	// completed); false for a failed run.
	Completed bool
	// Clean is true when the senior review left no unresolved findings.
	Clean bool
	// Unresolved is the count of unresolved senior-review findings.
	Unresolved int
}

// TicketRunner drives a single ticket's task end-to-end (plan → execute → review)
// and reports its Outcome. A non-nil error is a hard run failure; a completed run
// with findings is a successful Outcome (Completed true, Clean false), not an error.
type TicketRunner func(ctx context.Context, t Ticket) (Outcome, error)

// Runner sequences tickets over the DAG, runs each ready ticket via the injected
// TicketRunner, gates advancement, and persists per-ticket state after every
// transition so the multi-ticket run is resumable.
type Runner struct {
	graph     *Graph
	state     *State
	statePath string
	gate      GateMode
	run       TicketRunner
	out       io.Writer
}

// NewRunner builds a Runner. statePath is where per-ticket state is persisted; out
// receives human-facing progress (nil discards it).
func NewRunner(graph *Graph, state *State, statePath string, gate GateMode, run TicketRunner, out io.Writer) *Runner {
	if out == nil {
		out = io.Discard
	}
	return &Runner{graph: graph, state: state, statePath: statePath, gate: gate, run: run, out: out}
}

// Summary reports what a Run did, for the command to render.
type Summary struct {
	// Completed are the ticket ids marked done during this invocation.
	Completed []string
	// Failed is the ticket that did not complete and stopped the run ("" if none).
	Failed string
	// NeedsReview is the ticket that completed with unresolved findings and stopped
	// the run under a pausing gate ("" if none).
	NeedsReview string
	// Paused is true when a manual-gate run stopped after a clean ticket to await
	// the human (more ready tickets may remain).
	Paused bool
	// Exhausted is true when no ticket remains to run (every ticket is done).
	Exhausted bool
	// Remaining are ticket ids not yet done when the run stopped (for reporting a
	// blocked/paused backlog).
	Remaining []string
}

// Run drives the backlog until it pauses, stops on a gate, is aborted, or is
// exhausted. It is safe to call repeatedly (resume): already-done tickets are
// skipped via the persisted state.
func (r *Runner) Run(ctx context.Context) (Summary, error) {
	var sum Summary
	for {
		if ctx.Err() != nil {
			return r.finish(sum), ErrAborted
		}

		t, ok := r.graph.NextReady(r.doneSet())
		if !ok {
			return r.finish(sum), nil
		}

		ts := r.state.get(t.ID)
		ts.Status = TicketInProgress
		if err := r.save(); err != nil {
			return r.finish(sum), err
		}
		r.logf("▶ running ticket %s", t.ID)

		outcome, runErr := r.run(ctx, t)
		ts.RunID = outcome.RunID
		ts.Unresolved = outcome.Unresolved

		// A cancellation mid-run is an abort, not a failure: leave the ticket Ready
		// so the next invocation re-runs it.
		if ctx.Err() != nil {
			ts.Status = TicketReady
			_ = r.save()
			return r.finish(sum), ErrAborted
		}
		if runErr != nil {
			ts.Status = TicketFailed
			_ = r.save()
			sum.Failed = t.ID
			return r.finish(sum), fmt.Errorf("ticket %s failed: %w", t.ID, runErr)
		}

		switch {
		case !outcome.Completed:
			ts.Status = TicketFailed
			_ = r.save()
			sum.Failed = t.ID
			return r.finish(sum), fmt.Errorf("ticket %s did not complete (run %s)", t.ID, outcome.RunID)

		case !outcome.Clean && r.gate != GateAuto:
			ts.Status = TicketReview
			_ = r.save()
			sum.NeedsReview = t.ID
			r.logf("⏸ ticket %s completed with %d unresolved finding(s) — stopping for review", t.ID, outcome.Unresolved)
			return r.finish(sum), nil

		default:
			ts.Status = TicketDone
			if err := r.save(); err != nil {
				return r.finish(sum), err
			}
			sum.Completed = append(sum.Completed, t.ID)
			r.logf("✓ ticket %s done", t.ID)
			if r.gate == GateManual {
				sum.Paused = true
				r.logf("⏸ manual gate: pausing after %s — re-run to continue", t.ID)
				return r.finish(sum), nil
			}
		}
	}
}

// doneSet is the set of ticket ids considered complete: author-declared done plus
// runner-recorded done. It drives dependency satisfaction and selection.
func (r *Runner) doneSet() map[string]bool {
	done := make(map[string]bool)
	for _, t := range r.graph.Tickets() {
		if t.Status == StatusDone || r.state.isDone(t.ID) {
			done[t.ID] = true
		}
	}
	return done
}

// finish fills Exhausted/Remaining on the summary from the final done set.
func (r *Runner) finish(sum Summary) Summary {
	done := r.doneSet()
	for _, t := range r.graph.Tickets() {
		if !done[t.ID] {
			sum.Remaining = append(sum.Remaining, t.ID)
		}
	}
	sum.Exhausted = len(sum.Remaining) == 0
	return sum
}

func (r *Runner) save() error {
	return SaveState(r.statePath, r.state)
}

func (r *Runner) logf(format string, args ...any) {
	fmt.Fprintf(r.out, format+"\n", args...)
}
