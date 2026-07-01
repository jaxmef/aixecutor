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
	mu    sync.Mutex
	w     io.Writer
	tty   bool
	color bool
	// live, when set, owns the single terminal writer: line() routes every
	// permanent line through live.Emit so it never tangles with the live status
	// region. nil keeps the original mutex-guarded direct write.
	live *LiveStatus
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

// WithColor enables or disables ANSI colour in the semantic output and returns the
// receiver so it chains off NewProgress. NewProgress stays plain (colour off) by
// default, so existing callers and tests see uncoloured output. No-op on nil.
func (p *Progress) WithColor(on bool) *Progress {
	if p == nil {
		return nil
	}
	p.color = on
	return p
}

// WithLive attaches a live status region. Once set, every permanent progress line
// is routed through it (so the live owner is the sole terminal writer) and
// PhaseStarted forwards the phase to it. Returns the receiver to chain off
// NewProgress; no-op on nil.
func (p *Progress) WithLive(l *LiveStatus) *Progress {
	if p == nil {
		return nil
	}
	p.live = l
	return p
}

// Live returns the attached live status region (nil if none). The orchestrator
// uses it to compose the live-timer harness decorator at its single wrap point.
func (p *Progress) Live() *LiveStatus {
	if p == nil {
		return nil
	}
	return p.live
}

// Close stops the live region (if any), wiping the live line so subsequent output
// (e.g. the run summary) starts on a clean line. Idempotent and nil-safe.
func (p *Progress) Close() {
	if p == nil {
		return
	}
	p.live.Stop()
}

// TTY reports whether the progress writer is an interactive terminal. Callers may
// use it to decide on optional embellishments; the plain fallback is always valid.
func (p *Progress) TTY() bool {
	if p == nil {
		return false
	}
	return p.tty
}

// colorOn reports whether coloured output is enabled, nil-safe so the semantic
// methods can wrap tokens with Colorize without each nil-checking the receiver.
func (p *Progress) colorOn() bool {
	return p != nil && p.color
}

// line writes a single progress line. It is the single output primitive every
// method funnels through. When a live region is attached it routes through
// live.Emit (the live owner is then the sole terminal writer); otherwise it falls
// back to the mutex-guarded direct write, keeping every existing test and the
// non-live path byte-identical.
func (p *Progress) line(format string, args ...any) {
	if p == nil {
		return
	}
	if p.live != nil {
		p.live.Emit(fmt.Sprintf(format, args...))
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
	if p != nil {
		p.live.SetPhase(name)
	}
	p.line("→ %s", Colorize(p.colorOn(), AnsiBold, name))
}

// PlanningDone reports planning finished, where the docs are, and how many
// subtasks were planned — the load-bearing "read the docs while execution
// proceeds" moment (CLAUDE.md §3.3).
func (p *Progress) PlanningDone(docsDir string, subtasks int) {
	p.line("Planning complete: %d subtask(s). Docs: %s", subtasks, docsDir)
}

// ResumeHint tells the user how to continue a run that stopped after planning.
func (p *Progress) ResumeHint(id string) {
	p.line("Resume execution with: aixecutor resume %s", id)
}

// SubtaskStarted reports an executor beginning work on a subtask.
func (p *Progress) SubtaskStarted(id, title string) {
	p.line("  [%s] implementing: %s", Colorize(p.colorOn(), AnsiCyan, id), title)
}

// SubtaskReviewVerdict reports one subtask review round's outcome: approved, or
// not-approved with a finding count. round is 1-based.
func (p *Progress) SubtaskReviewVerdict(id string, round int, approved bool, findings int) {
	c := p.colorOn()
	cid := Colorize(c, AnsiCyan, id)
	if approved {
		p.line("  [%s] review round %d: %s", cid, round, Colorize(c, AnsiGreen, "approved"))
		return
	}
	p.line("  [%s] review round %d: %s finding(s) — remediating", cid, round,
		Colorize(c, AnsiYellow, fmt.Sprintf("%d", findings)))
}

// SubtaskDone reports a subtask reaching done, with the executor↔reviewer loop
// count it took. flagged>0 means it proceeded with that many unresolved findings
// carried forward (the cap-reached, proceed-flagged outcome).
func (p *Progress) SubtaskDone(id string, loops, flagged int) {
	c := p.colorOn()
	cid := Colorize(c, AnsiCyan, id)
	done := Colorize(c, AnsiGreen, "done")
	if flagged > 0 {
		p.line("  [%s] %s (loops=%d, %d unresolved finding(s) carried forward)", cid, done, loops, flagged)
		return
	}
	p.line("  [%s] %s (loops=%d)", cid, done, loops)
}

// SubtaskFailed reports a subtask failing, with a short cause.
func (p *Progress) SubtaskFailed(id string, cause string) {
	c := p.colorOn()
	p.line("  [%s] %s: %s", Colorize(c, AnsiCyan, id), Colorize(c, AnsiRed, "FAILED"), cause)
}

// SeniorRound reports one senior-review round's outcome over the whole diff:
// approved (clean), or not-approved with a finding count. n is 1-based.
func (p *Progress) SeniorRound(n int, approved bool, findings int) {
	c := p.colorOn()
	if approved {
		p.line("  senior review round %d: %s", n, Colorize(c, AnsiGreen, "clean"))
		return
	}
	p.line("  senior review round %d: %s finding(s) — remediating", n,
		Colorize(c, AnsiYellow, fmt.Sprintf("%d", findings)))
}

// RunCompleted reports the run reaching a terminal status (completed / aborted /
// failed), as the final progress line before the summary.
func (p *Progress) RunCompleted(status string) {
	code := AnsiYellow
	switch status {
	case "completed":
		code = AnsiGreen
	case "failed", "aborted":
		code = AnsiRed
	}
	p.line("Run %s.", Colorize(p.colorOn(), code, status))
}
