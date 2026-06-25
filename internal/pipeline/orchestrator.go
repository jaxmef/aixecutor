package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jaxmef/aixecutor/internal/config"
	"github.com/jaxmef/aixecutor/internal/harness"
	"github.com/jaxmef/aixecutor/internal/log"
	"github.com/jaxmef/aixecutor/internal/prompt"
	"github.com/jaxmef/aixecutor/internal/run"
)

// ErrAborted is the sentinel returned by Start/Resume when a run is stopped by
// context cancellation (a SIGINT/SIGTERM the CLI translated into a canceled ctx)
// rather than by a genuine error. The run is persisted as StatusAborted (a
// resumable, non-failed terminal off-ramp), so the CLI can tell "the user
// interrupted me — tell them how to resume" apart from "the pipeline failed".
// Callers test for it with errors.Is.
var ErrAborted = errors.New("pipeline: run aborted")

// ErrPaused is the sentinel returned by Start/Resume when execution stopped at a
// safe subtask boundary in response to a review request (AIX-0016). The run is
// persisted as StatusPaused (resumable): the CLI tells the user how to inspect the
// docs and then either `resume` (clarify → continue) or `amend` (revert + restart).
// It is neither a failure nor an abort. Callers test for it with errors.Is.
var ErrPaused = errors.New("pipeline: run paused for review")

// Orchestrator drives a single run through the pipeline state machine
// (CLAUDE.md §3.3): created → planning → planned → executing → seniorReview →
// completed, with failed/aborted as terminal off-ramps. It is the thin sequencer
// that owns the run-level state transitions and persists run.yaml after every one
// of them; the per-phase work lives in the phase types (Planner, Scheduler +
// review loop, seniorReviewPhase) it composes.
//
// All collaborators are injected so the orchestrator is hermetically testable:
// the run Store (persistence + create), the resolved Config, the harness Registry
// (the planner/executor/reviewers are resolved from it by role), the git gateway
// seam (read-only + gated worktrees — the ONLY git the orchestrator ever touches,
// invariant #1), the prompt Renderer, and the repo summarizer for planning.
// Production wiring lives in internal/cli; tests construct it with mocks + fakes.
type Orchestrator struct {
	store      *run.Store
	cfg        config.Config
	reg        *harness.Registry
	git        gitGateway
	renderer   *prompt.Renderer
	summarizer RepoSummarizer

	// dryRun marks that the registry returns dry-run-wrapped harnesses, so the
	// planner writes placeholder docs instead of failing strict validation (the
	// rest of the pipeline already degrades gracefully under the dry-run wrapper:
	// the executor "edits" nothing, reviewers "approve", so it converges).
	dryRun bool
	// progress renders concise, semantic human progress (CLAUDE.md §7); it is
	// shared with the phases so all human output is consistent and TTY-aware.
	// Defaults to a stdout-backed Progress.
	progress *log.Progress
	// logger is the structured logger (slog). It is attached to the run's logs/ dir
	// once the run is created, and wraps the registry so every harness invocation is
	// logged with a pointer to its persisted raw output. nil is safe (no logging).
	logger *log.Logger
}

// OrchestratorOption configures an Orchestrator at construction.
type OrchestratorOption func(*Orchestrator)

// WithOrchestratorDryRun marks the orchestrator as running against dry-run-wrapped
// harnesses, so planning writes placeholder docs (matching how `plan` handles
// --dry-run) and no real agent is ever invoked.
func WithOrchestratorDryRun(dryRun bool) OrchestratorOption {
	return func(o *Orchestrator) { o.dryRun = dryRun }
}

// WithOrchestratorOutput sets where the orchestrator and its phases print human
// progress, by building a Progress over w. Defaults to os.Stdout; tests pass a
// buffer. Kept for backward compatibility with callers that have only a writer;
// WithOrchestratorProgress is preferred when a shared Progress already exists.
func WithOrchestratorOutput(w io.Writer) OrchestratorOption {
	return func(o *Orchestrator) {
		if w != nil {
			o.progress = log.NewProgress(w)
		}
	}
}

// WithOrchestratorProgress sets the shared Progress the orchestrator and its
// phases emit semantic events through. Defaults to a stdout-backed Progress.
func WithOrchestratorProgress(p *log.Progress) OrchestratorOption {
	return func(o *Orchestrator) {
		if p != nil {
			o.progress = p
		}
	}
}

// WithOrchestratorLogger sets the structured logger. The orchestrator attaches it
// to the run's logs/ dir at run start and wraps the registry so every harness
// invocation is logged with a pointer to its persisted raw output. nil disables
// structured logging.
func WithOrchestratorLogger(l *log.Logger) OrchestratorOption {
	return func(o *Orchestrator) { o.logger = l }
}

// NewOrchestrator constructs an Orchestrator from its collaborators. store, reg,
// gw, and renderer are required; summarizer feeds the planning phase (a
// GitRepoSummarizer in production, a fake in tests); cfg drives every knob.
func NewOrchestrator(
	store *run.Store,
	cfg config.Config,
	reg *harness.Registry,
	gw gitGateway,
	renderer *prompt.Renderer,
	summarizer RepoSummarizer,
	opts ...OrchestratorOption,
) (*Orchestrator, error) {
	if store == nil {
		return nil, errors.New("pipeline: NewOrchestrator requires a run store")
	}
	if reg == nil {
		return nil, errors.New("pipeline: NewOrchestrator requires a harness registry")
	}
	if gw == nil {
		return nil, errors.New("pipeline: NewOrchestrator requires a git gateway")
	}
	if renderer == nil {
		return nil, errors.New("pipeline: NewOrchestrator requires a renderer")
	}
	if summarizer == nil {
		return nil, errors.New("pipeline: NewOrchestrator requires a repo summarizer")
	}
	o := &Orchestrator{
		store:      store,
		cfg:        cfg,
		reg:        reg,
		git:        gw,
		renderer:   renderer,
		summarizer: summarizer,
		progress:   log.NewProgress(nil),
	}
	for _, opt := range opts {
		opt(o)
	}
	if o.progress == nil {
		o.progress = log.NewProgress(nil)
	}
	return o, nil
}

// Start creates a new run for task and drives it through the pipeline from the
// `created` state. It returns the run in whatever terminal-or-stopped state it
// reached and the driving error (nil on a completed run; ErrAborted on a
// cancellation; a wrapped phase error otherwise). The returned *run.Run is always
// non-nil once Create succeeds, so callers can render a summary even on failure.
func (o *Orchestrator) Start(ctx context.Context, task string) (*run.Run, error) {
	r, err := o.store.Create(task, o.cfg)
	if err != nil {
		return nil, fmt.Errorf("pipeline: creating run: %w", err)
	}
	o.attachRunLogging(r)
	o.progress.RunCreated(r.ID, excerptTask(task))
	// resuming=false: a fresh run honors the autostartExecution gate (stop after
	// planning when it is off).
	return r, o.drive(ctx, r, false)
}

// Resume loads the run identified by id (or the latest when id is "" / "latest")
// and re-enters the state machine at its persisted state, redoing no finished
// work (the resume contract on run.Store.Load). It returns the run and the same
// error contract as Start. A terminal run (completed/failed/aborted) is loaded and
// returned with a nil error from drive — except an aborted run, which drive
// re-enters and continues (abort is resumable), and completed/failed which are
// surfaced as "nothing to resume" by the caller via the returned run's Status.
//
// A resume always PROCEEDS past the planned→executing autostart gate: stopping
// after planning is the behavior of the INITIAL run with autostartExecution off
// (the user reads the docs, then runs `resume` to continue); an explicit resume is
// that continuation, so it must not stop at planned again.
func (o *Orchestrator) Resume(ctx context.Context, id string) (*run.Run, error) {
	r, err := o.store.Load(id)
	if err != nil {
		return nil, err
	}
	o.attachRunLogging(r)
	o.progress.RunResuming(r.ID, r.Status.String())
	return r, o.drive(ctx, r, true)
}

// Amend implements the AIX-0016 amend path from a paused run: it reverts the
// working tree to the run-start baseline (Option B — raw file restore, NO mutating
// git), re-reads the user's edited docs/subtasks.yaml, re-validates the DAG, resets
// subtask + senior-review state, and restarts execution from the clean baseline. It
// returns the run and the same error contract as Start (nil on completion,
// ErrPaused/ErrAborted on a stop, a wrapped phase error on failure).
//
// The amendments themselves survive the revert: the docs live under runsDir (which
// the gateway already excludes from the baseline) and the docs dir is additionally
// passed as a restore exclusion to cover a custom --docs-path. Pre-existing
// uncommitted changes are restored byte-for-byte (the baseline captured them).
func (o *Orchestrator) Amend(ctx context.Context, id string) (*run.Run, error) {
	r, err := o.store.Load(id)
	if err != nil {
		return nil, err
	}
	if r.Status != run.StatusPaused {
		return r, fmt.Errorf(
			"pipeline: run %q is %s, not paused; pause it with `aixecutor review %s` before amending",
			r.ID, r.Status, r.ID)
	}
	o.attachRunLogging(r)

	// 1. Revert the working tree to the pre-execution baseline.
	res, err := o.git.RestoreTree(ctx, r.Baseline.Dir, o.docsExcludes(r))
	if err != nil {
		return r, fmt.Errorf("pipeline: reverting run %q to its baseline: %w", r.ID, err)
	}
	o.progress.Logf("Reverted to the pre-execution state (%d file(s) restored, %d removed).",
		res.Restored, res.Deleted)

	// 2. Re-derive the subtask DAG from the amended docs/subtasks.yaml.
	subtasks, err := o.reloadSubtasks(r)
	if err != nil {
		return r, fmt.Errorf("pipeline: re-reading amended subtasks for run %q: %w", r.ID, err)
	}

	// 3. Reset run state for a clean restart, persist, then re-drive from executing.
	if err := o.save(r, func() {
		r.Subtasks = subtasks
		r.SeniorReview.Status = run.SeniorReviewPending
		r.SeniorReview.Rounds = 0
		r.SeniorReview.Unresolved = nil
		r.Status = run.StatusExecuting
	}); err != nil {
		return r, err
	}
	_ = o.store.ClearPause(r.ID)

	o.progress.Logf("Restarting execution from the amended plan (%d subtask(s)).", len(subtasks))
	return r, o.driveFrom(ctx, r, run.StatusExecuting, true)
}

// reloadSubtasks reads and DAG-validates the (possibly amended) docs/subtasks.yaml
// for a run, returning fresh pending subtasks.
func (o *Orchestrator) reloadSubtasks(r *run.Run) ([]run.Subtask, error) {
	path := filepath.Join(o.store.DocsDir(r.ID), subtasksDocName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %q: %w", path, err)
	}
	return ParseSubtasks(data)
}

// docsExcludes returns the docs directory as a repo-root-relative restore exclusion
// (so amended docs are never reverted), or nil when the docs dir is not under the
// repo root (already outside the baseline, so no exclusion is needed).
func (o *Orchestrator) docsExcludes(r *run.Run) []string {
	rel, err := filepath.Rel(o.git.RepoRoot(), o.store.DocsDir(r.ID))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	return []string{rel}
}

// attachRunLogging points the structured logger at the run's logs/ dir and wraps
// the registry so every harness invocation is logged with a pointer to its
// persisted raw output (<run>/logs/<role>-<seq>.out). It is called once per run
// (from Start/Resume) before any phase runs. A nil logger is a no-op; a failure to
// open the run log file is reported on the logger itself (best-effort) and never
// fails the run.
func (o *Orchestrator) attachRunLogging(r *run.Run) {
	if o.logger == nil {
		return
	}
	logsDir := run.Layout{RunsDir: o.store.RunsDir(), ID: r.ID, DocsSubdir: o.cfg.Paths.DocsSubdir}.LogsDir()
	if err := o.logger.AttachRunFile(logsDir); err != nil {
		o.logger.Warn("could not attach run log file; continuing console-only", "error", err.Error())
	}
	o.logger.Info("run starting", "id", r.ID, "status", r.Status.String(), "dryRun", o.dryRun)
	// Verbose-only: dump the resolved per-role harness/model wiring so a `-v` run is
	// observably more detailed without cluttering the default level.
	o.logger.Debug("resolved roles",
		"planner", roleDesc(o.cfg.Roles.Planner),
		"executor", roleDesc(o.cfg.Roles.Executor),
		"subtaskReviewer", roleDesc(o.cfg.Roles.SubtaskReviewer),
		"seniorReviewer", roleDesc(o.cfg.Roles.SeniorReviewer),
		"isolation", o.cfg.Pipeline.Execution.Isolation,
		"maxParallel", o.cfg.Pipeline.Execution.MaxParallel,
	)
	// Wrap the registry so each role's harness logs its invocations + persists raw
	// output. Done here (not at construction) because the logs dir is per-run.
	o.reg = o.reg.Wrap(func(h harness.Harness) harness.Harness {
		return log.WrapHarness(h, o.logger)
	})
}

// drive is the shared state machine that both Start and Resume run, so a fresh run
// and a resumed run follow the EXACT same path. It is idempotent per state: each
// case re-derives what it needs from the persisted run/artifacts (it does not
// re-plan when subtasks already exist; the scheduler skips done subtasks; the
// senior phase re-enters at its persisted round), and run.yaml is Saved BETWEEN
// every transition so a crash anywhere leaves a resumable checkpoint.
//
// Cancellation vs. failure: if ctx is canceled at a phase boundary or surfaces out
// of a phase as a cancellation, drive persists the run as `aborted` (resumable)
// and returns ErrAborted — it does NOT mark the run failed. Any other phase error
// persists `failed` and is returned wrapped. The fallthrough pattern means a run
// that enters at an earlier state flows forward through the later ones in one call.
func (o *Orchestrator) drive(ctx context.Context, r *run.Run, resuming bool) error {
	// A run that already finished (or failed) is not driven further; the caller
	// reports it. An aborted run is NOT terminal here — it is resumable, so it
	// falls into the switch below and re-enters at whatever phase it was in
	// (its run-level Status was left at the phase it was aborted in... except we
	// persist Status=aborted on abort, so handle that explicitly first).
	switch r.Status {
	case run.StatusCompleted, run.StatusFailed:
		return nil
	case run.StatusPaused:
		// A paused run resumes execution (clarify → continue). Clear any lingering
		// pause request so it does not immediately re-pause, then re-enter execution:
		// the scheduler re-derives the ready set from persisted statuses, so done
		// subtasks are not re-run and the rest continue.
		_ = o.store.ClearPause(r.ID)
		return o.driveFrom(ctx, r, run.StatusExecuting, resuming)
	case run.StatusAborted:
		// Resuming an aborted run: re-derive the phase to re-enter from the
		// persisted sub-state (subtasks / senior-review progress), since the
		// run-level Status no longer names a phase.
		return o.driveFrom(ctx, r, o.phaseAfterAbort(r), resuming)
	}
	return o.driveFrom(ctx, r, r.Status, resuming)
}

// driveFrom runs the state machine starting at `from`. It is separated from drive
// so the aborted-run path can inject the re-derived phase. Each case persists
// before proceeding and falls through to the next phase. resuming bypasses the
// planned→executing autostart gate (an explicit resume always continues).
func (o *Orchestrator) driveFrom(ctx context.Context, r *run.Run, from run.Status, resuming bool) error {
	switch from {
	case run.StatusCreated, run.StatusPlanning:
		if err := o.runPlanning(ctx, r); err != nil {
			return o.classify(ctx, r, err)
		}
		fallthrough

	case run.StatusPlanned:
		// Stop after planning when autostartExecution is off — UNLESS this is an
		// explicit resume, which is the user's signal to continue past planning
		// (the stop-after-planning workflow is: initial run stops here, user reads
		// the docs, then `resume` proceeds). Mirrors `plan` for the initial run.
		if !resuming && !o.cfg.Pipeline.AutostartExecution {
			o.progress.Logf("Planning complete; autostartExecution is off — stopping after planning.")
			o.progress.Logf("Resume execution with: aixecutor resume %s", r.ID)
			return nil
		}
		// Transition planned → executing and persist BEFORE the scheduler runs, so
		// an interruption between planning and execution is resumable at executing.
		if err := o.save(r, func() { r.Status = run.StatusExecuting }); err != nil {
			return o.classify(ctx, r, err)
		}
		fallthrough

	case run.StatusExecuting:
		if err := o.runExecution(ctx, r); err != nil {
			// A pause-to-review is a clean, resumable stop: the scheduler already
			// persisted StatusPaused. Surface ErrPaused without marking the run
			// failed/aborted so the CLI prints the review options.
			if errors.Is(err, ErrPaused) {
				return err
			}
			return o.classify(ctx, r, err)
		}
		// Execution succeeded (every subtask done). Advance to senior review and
		// persist before the phase runs.
		if err := o.save(r, func() { r.Status = run.StatusSeniorReview }); err != nil {
			return o.classify(ctx, r, err)
		}
		fallthrough

	case run.StatusSeniorReview:
		if err := o.runSeniorReview(ctx, r); err != nil {
			return o.classify(ctx, r, err)
		}
		// Senior review completed (clean or report-and-proceed). Finish the run.
		if err := o.save(r, func() { r.Status = run.StatusCompleted }); err != nil {
			return o.classify(ctx, r, err)
		}
		fallthrough

	case run.StatusCompleted:
		o.progress.RunCompleted("completed")
		return nil

	default:
		// Unknown/foreign status — should be caught by Load's validation, but never
		// silently no-op: surface it.
		return fmt.Errorf("pipeline: run %q has unexpected status %q; cannot drive it", r.ID, r.Status)
	}
}

// runPlanning runs the planning phase, idempotently. On resume into
// created/planning it simply re-plans (planning is a single idempotent step that
// rewrites the docs and re-seeds the subtask DAG), EXCEPT it short-circuits when a
// prior run already produced subtasks AND reached at least `planned`. That guard
// keeps re-planning from clobbering the subtask DAG (and any per-subtask progress)
// of a run that, on disk, already advanced past planning — e.g. an interrupted
// planning that wrote subtasks but crashed before flipping the status, or a
// hand-edited run.yaml. The planner persists the planned transition itself.
func (o *Orchestrator) runPlanning(ctx context.Context, r *run.Run) error {
	// If the run already has a planned DAG (subtasks present and the run got at
	// least as far as planned), do not re-plan: re-deriving from persisted state
	// keeps resume from redoing finished planning and from discarding subtask
	// progress. This matters when drive is entered at created/planning on a run
	// that, on disk, actually has subtasks (e.g. an interrupted planning that DID
	// write them but crashed before flipping the status).
	if len(r.Subtasks) > 0 && statusAtLeastPlanned(r.Status) {
		o.progress.Logf("Planning already complete (%d subtask(s)); skipping.", len(r.Subtasks))
		return nil
	}

	plannerRole := o.cfg.Roles.Planner
	h, ok := o.reg.Get(plannerRole.Harness)
	if !ok {
		return fmt.Errorf("pipeline: planner harness %q is not defined (known: %s)",
			plannerRole.Harness, strings.Join(o.reg.Names(), ", "))
	}

	planner := NewPlanner(
		h,
		o.renderer,
		o.store,
		o.summarizer,
		plannerRole,
		o.git.RepoRoot(),
		WithDryRun(o.dryRun),
		WithProgress(o.progress),
	)
	return planner.Plan(ctx, r)
}

// runExecution runs the subtask scheduler with the real per-subtask review loop
// (AIX-0010 + AIX-0011). The scheduler re-derives the ready set purely from
// persisted subtask statuses, so resume re-enters correctly: done subtasks are
// never re-run, and interrupted ones are rewound to pending and re-executed. A
// clean run leaves every subtask done; the orchestrator advances the run-level
// status afterward.
func (o *Orchestrator) runExecution(ctx context.Context, r *run.Run) error {
	sched, err := NewSchedulerWithReview(r, o.cfg, o.reg, o.git, o.store, o.renderer,
		WithSchedulerProgress(o.progress),
		WithSchedulerDryRun(o.dryRun),
		// Honor a pause-to-review request (AIX-0016) at each subtask boundary by
		// polling the run's control channel. Safe when no request exists (false).
		WithPauseCheck(func() bool { return o.store.PauseRequested(r.ID) }))
	if err != nil {
		return err
	}
	return sched.Run(ctx)
}

// runSeniorReview runs the senior-review phase (AIX-0012). The phase re-enters at
// its persisted SeniorReview.Status/Rounds, so a resumed run continues the loop
// rather than restarting it; a disabled phase records itself skipped. The phase
// leaves SeniorReview.Status terminal on a normal outcome (clean OR cap-reached),
// and only returns an error for a fatal condition.
func (o *Orchestrator) runSeniorReview(ctx context.Context, r *run.Run) error {
	phase, err := NewSeniorReviewPhase(o.cfg, o.reg, o.git, o.store, o.renderer,
		WithSeniorReviewProgress(o.progress),
		WithSeniorReviewDryRun(o.dryRun))
	if err != nil {
		return err
	}
	return phase.Run(ctx, r)
}

// classify turns a phase error into the right terminal state + return value. A
// cancellation (ctx canceled, or the error wraps context.Canceled /
// DeadlineExceeded) is NOT a failure: the run is persisted `aborted` (resumable)
// and ErrAborted is returned. Any other error persists `failed` and is returned
// wrapped with the run id. Worktree cleanup already happened inside the scheduler
// (its defer fires on ctx-cancel); the orchestrator only records the run state.
func (o *Orchestrator) classify(ctx context.Context, r *run.Run, phaseErr error) error {
	if isCancellation(ctx, phaseErr) {
		// Persist aborted (best-effort; the returned ErrAborted is the primary
		// signal). Do not clobber an already-terminal status.
		if !r.Status.IsTerminal() {
			if serr := o.save(r, func() { r.Status = run.StatusAborted }); serr != nil {
				// Persisting the abort failed; report both, but still signal abort so
				// the CLI prints the resume hint rather than a failure.
				o.progress.Logf("warning: persisting aborted state for run %s failed: %v", r.ID, serr)
			}
		}
		o.progress.Logf("Run %s aborted; resume with: aixecutor resume %s", r.ID, r.ID)
		return fmt.Errorf("%w (run %s): %v", ErrAborted, r.ID, phaseErr)
	}

	// A genuine phase error. Mark the run failed if a phase did not already do so
	// (the scheduler marks failed on subtask failure; senior-review failures leave
	// the run in seniorReview). Saving failed here is idempotent.
	if !r.Status.IsTerminal() {
		if serr := o.save(r, func() { r.Status = run.StatusFailed }); serr != nil {
			return fmt.Errorf("pipeline: run %q failed (%v) and persisting the failed state also failed: %w",
				r.ID, phaseErr, serr)
		}
	}
	return fmt.Errorf("pipeline: run %q failed: %w", r.ID, phaseErr)
}

// phaseAfterAbort re-derives which phase a previously-aborted run should re-enter,
// from its persisted sub-state (since Status==aborted no longer names a phase).
// The order mirrors the forward pipeline: if the senior review has started, resume
// there; else if any subtasks exist, resume execution; else (re)plan.
func (o *Orchestrator) phaseAfterAbort(r *run.Run) run.Status {
	switch {
	case r.SeniorReview.Status == run.SeniorReviewRunning || r.SeniorReview.Status == run.SeniorReviewDone:
		// Senior review had started — re-enter there; it resumes at its persisted
		// round (or no-ops if already done).
		return run.StatusSeniorReview
	case len(r.Subtasks) > 0:
		// Planning produced the DAG, so re-enter execution. The scheduler re-derives
		// the ready set from persisted statuses: it re-runs only the unfinished
		// subtasks, and if every subtask is already done it no-ops and the
		// fallthrough advances to senior review.
		return run.StatusExecuting
	default:
		// No subtasks yet — planning never finished; re-plan.
		return run.StatusPlanning
	}
}

// save applies mutate to the run and persists it through the store, wrapping the
// error with the run id. The orchestrator drives a single run on one goroutine
// (the scheduler owns the parallel fan-out internally and serializes its own
// saves), so no lock is needed here.
func (o *Orchestrator) save(r *run.Run, mutate func()) error {
	if mutate != nil {
		mutate()
	}
	if err := o.store.Save(r); err != nil {
		return fmt.Errorf("pipeline: persisting run %q state: %w", r.ID, err)
	}
	return nil
}

// roleDesc renders a role's harness+model for the verbose "resolved roles" debug
// line, e.g. "claude/opus".
func roleDesc(r config.Role) string { return r.Harness + "/" + r.Model }

// statusAtLeastPlanned reports whether a run-level status is planned-or-later
// (i.e. planning is done). Used by runPlanning's idempotence guard.
func statusAtLeastPlanned(s run.Status) bool {
	switch s {
	case run.StatusPlanned, run.StatusExecuting, run.StatusSeniorReview, run.StatusCompleted:
		return true
	default:
		return false
	}
}

// isCancellation reports whether a phase error represents a context cancellation
// (the user interrupted the run) rather than a genuine failure. It checks both the
// live ctx and the error chain, because a phase may return a wrapped ctx error
// after the ctx was canceled.
func isCancellation(ctx context.Context, err error) bool {
	if ctx != nil && ctx.Err() != nil {
		return true
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// excerptTask trims a task description to a single readable line for the run
// banner, collapsing internal newlines and bounding the length. It counts runes
// (not bytes) so a multibyte task is never split mid-character.
func excerptTask(task string) string {
	t := strings.TrimSpace(strings.ReplaceAll(task, "\n", " "))
	const max = 80
	r := []rune(t)
	if len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return t
}
