package log

import (
	"fmt"
	"io"
	"os"
	"sync"
)

// Progress renders concise, incremental, line-oriented human progress to a single
// writer (normally stdout). It is the ONE place phase code goes for human output,
// so formatting and TTY behavior stay consistent and the phases emit SEMANTIC
// events ("subtask st-01 review approved on round 2") rather than ad-hoc prints.
//
// The event set is deliberately small but semantic so AIX-0015's TUI can consume
// the SAME events instead of re-deriving run state: a future TUI implements this
// surface (or wraps it) and renders the events its own way. Today's implementation
// formats plain text lines.
//
// TTY-awareness: on a non-TTY (a pipe, a file, CI) the output is plain and
// line-oriented — no spinners or cursor tricks (there are none here regardless).
// IsTTY is detected via the standard library (os.ModeCharDevice); the flag is
// exposed (TTY) so callers/tests can branch, but the current renderer's output is
// identical either way (plain lines), which is the required graceful degradation.
//
// All methods are safe for concurrent use: the scheduler runs subtasks in
// parallel, so writes are serialized behind a mutex to keep lines from
// interleaving (an io.Writer is not required to be concurrency-safe).
type Progress struct {
	mu  sync.Mutex
	w   io.Writer
	tty bool
}

// NewProgress builds a Progress writing to w (defaults to os.Stdout when nil) and
// detects whether w is a TTY. Tests pass a bytes.Buffer (never a TTY) to assert on
// the plain output.
func NewProgress(w io.Writer) *Progress {
	if w == nil {
		w = os.Stdout
	}
	return &Progress{w: w, tty: IsTTY(w)}
}

// TTY reports whether the progress writer is an interactive terminal. Callers may
// use it to decide on optional embellishments; the plain fallback is always valid.
func (p *Progress) TTY() bool {
	if p == nil {
		return false
	}
	return p.tty
}

// line writes a single progress line (adding the trailing newline) under the lock.
// It is the single output primitive every method funnels through.
func (p *Progress) line(format string, args ...any) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	w := p.w
	if w == nil {
		w = os.Stdout
	}
	fmt.Fprintf(w, format+"\n", args...)
}

// Logf emits a freeform progress line. It is the escape hatch for phase messages
// that are not (yet) a named semantic event; prefer a dedicated method when one
// fits. No-op on a nil Progress.
func (p *Progress) Logf(format string, args ...any) {
	p.line(format, args...)
}

// RunCreated announces a new run starting for a (already-excerpted) task.
func (p *Progress) RunCreated(id, taskExcerpt string) {
	p.line("Run %s created for task: %s", id, taskExcerpt)
}

// RunResuming announces a run being resumed from a persisted status.
func (p *Progress) RunResuming(id, status string) {
	p.line("Resuming run %s (status: %s)", id, status)
}

// PhaseStarted announces entering a named pipeline phase (planning / execution /
// senior review). It is the coarse banner a reader uses to follow the pipeline.
func (p *Progress) PhaseStarted(name string) {
	p.line("→ %s", name)
}

// PlanningDone reports planning finished, where the docs are, and how many
// subtasks were planned — the load-bearing "read the docs while execution
// proceeds" moment (CLAUDE.md §3.3).
func (p *Progress) PlanningDone(docsDir string, subtasks int) {
	p.line("Planning complete: %d subtask(s). Docs: %s", subtasks, docsDir)
}

// SubtaskStarted reports an executor beginning work on a subtask.
func (p *Progress) SubtaskStarted(id, title string) {
	p.line("  [%s] implementing: %s", id, title)
}

// SubtaskReviewVerdict reports one subtask review round's outcome: approved, or
// not-approved with a finding count. round is 1-based.
func (p *Progress) SubtaskReviewVerdict(id string, round int, approved bool, findings int) {
	if approved {
		p.line("  [%s] review round %d: approved", id, round)
		return
	}
	p.line("  [%s] review round %d: %d finding(s) — remediating", id, round, findings)
}

// SubtaskDone reports a subtask reaching done, with the executor↔reviewer loop
// count it took. flagged>0 means it proceeded with that many unresolved findings
// carried forward (the cap-reached, proceed-flagged outcome).
func (p *Progress) SubtaskDone(id string, loops, flagged int) {
	if flagged > 0 {
		p.line("  [%s] done (loops=%d, %d unresolved finding(s) carried forward)", id, loops, flagged)
		return
	}
	p.line("  [%s] done (loops=%d)", id, loops)
}

// SubtaskFailed reports a subtask failing, with a short cause.
func (p *Progress) SubtaskFailed(id string, cause string) {
	p.line("  [%s] FAILED: %s", id, cause)
}

// SeniorRound reports one senior-review round's outcome over the whole diff:
// approved (clean), or not-approved with a finding count. n is 1-based.
func (p *Progress) SeniorRound(n int, approved bool, findings int) {
	if approved {
		p.line("  senior review round %d: clean", n)
		return
	}
	p.line("  senior review round %d: %d finding(s) — remediating", n, findings)
}

// RunCompleted reports the run reaching a terminal status (completed / aborted /
// failed), as the final progress line before the summary.
func (p *Progress) RunCompleted(status string) {
	p.line("Run %s.", status)
}
