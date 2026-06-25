package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jaxmef/aixecutor/internal/config"
	"github.com/jaxmef/aixecutor/internal/harness"
	"github.com/jaxmef/aixecutor/internal/log"
	"github.com/jaxmef/aixecutor/internal/prompt"
	"github.com/jaxmef/aixecutor/internal/run"
)

// subtaskRunner is the narrow slice of owner-mediated engine state the review loop
// legitimately cannot own: it reads a subtask's snapshot via the run-state owner
// and reuses the single executor (snapshot+diff) entrypoint for remediation.
// *Scheduler satisfies it; the loop depends on this capability, not the runtime.
type subtaskRunner interface {
	subtaskSnapshot(id string) (run.Subtask, bool)
	runExecutor(ctx context.Context, id string, prior []Finding) (string, error)
}

// subtaskReviewLoop is the per-subtask executor↔reviewer loop (CLAUDE.md §3.3,
// AIX-0011). It is wired into the scheduler through the ReviewHook seam: after a
// subtask's diff is captured, the scheduler calls loop.Hook, which runs the
// subtaskReviewer on JUST that diff and, while the reviewer reports actionable
// findings, feeds them back to the executor and re-reviews — up to
// subtaskReview.maxLoops remediation cycles — then marks the subtask done.
//
// The loop never mutates shared run state directly; every subtask-state change
// goes through the commit func the scheduler hands the hook (serialized by the
// run-state owner goroutine), keeping the engine -race clean even though subtasks
// run in parallel.
type subtaskReviewLoop struct {
	// runner is the narrow owner-mediated engine capability (snapshot + the reusable
	// executor entrypoint), so remediation stays on the single snapshot+diff path.
	runner subtaskRunner

	// Construction-time collaborators copied from the scheduler at wiring time, so
	// the loop holds only what it needs rather than the whole runtime.
	cfg      config.ReviewLoop
	dryRun   bool
	progress *log.Progress
	renderer *prompt.Renderer
	repoRoot string
	layout   run.Layout

	// reviewer drives the subtaskReviewer agent (resolved from the registry by the
	// subtaskReviewer role's harness name).
	reviewer harness.Harness
	// role is the subtaskReviewer role config (model/permissionMode/template/
	// timeout), so the reviewer is fully configurable (invariant #4).
	role config.Role
}

// newSubtaskReviewLoop builds the loop from the narrow runner capability plus the
// explicit collaborators it needs (renderer, repo root, layout, review config,
// dry-run flag, progress) and the reviewer harness/role. The runtime's factory
// (NewSchedulerWithReview) binds these from a live scheduler.
func newSubtaskReviewLoop(
	runner subtaskRunner,
	reviewer harness.Harness,
	role config.Role,
	renderer *prompt.Renderer,
	progress *log.Progress,
	repoRoot string,
	layout run.Layout,
	cfg config.ReviewLoop,
	dryRun bool,
) *subtaskReviewLoop {
	return &subtaskReviewLoop{
		runner:   runner,
		reviewer: reviewer,
		role:     role,
		renderer: renderer,
		progress: progress,
		repoRoot: repoRoot,
		layout:   layout,
		cfg:      cfg,
		dryRun:   dryRun,
	}
}

// Hook has the ReviewHook signature so the scheduler is configured with
// WithReviewHook(loop.Hook). It runs the review loop for one subtask and drives it
// to a terminal state through commit (CommitFunc) — never by touching shared run
// state directly. A returned error fails the subtask's execution; the normal
// outcomes (approved, or cap-reached proceed-flagged) both mark the subtask done
// and return nil.
//
// snapshot is a value copy of the subtask produced by the run-state owner; the
// loop reads its id/spec freely and writes ONLY via commit.
func (l *subtaskReviewLoop) Hook(ctx context.Context, snapshot run.Subtask, commit CommitFunc) error {
	cfg := l.cfg

	// Review disabled: skip straight to done (CLAUDE.md §3.3 / schema
	// subtaskReview.enabled: false). No reviewer is invoked.
	if !cfg.Enabled {
		l.progress.Logf("  [%s] review disabled — marking done", snapshot.ID)
		return commit(func(st *run.Subtask) { st.Status = run.SubtaskDone })
	}

	// Dry-run: the reviewer harness is the placeholder wrapper, whose result is not
	// a parseable verdict. Treat it as an approval and mark done, so the whole
	// pipeline converges under --dry-run (mirrors the Planner's dry-run path). No
	// reviewer is invoked.
	if l.dryRun {
		l.progress.Logf("  [%s] [dry-run] skipping review — marking done", snapshot.ID)
		return commit(func(st *run.Subtask) {
			st.Status = run.SubtaskDone
			st.Unresolved = nil
		})
	}

	id := snapshot.ID
	if cfg.MaxLoops < 0 {
		l.progress.Logf("  [%s] reviewing (maxLoops unlimited — termination depends on the reviewer converging)", id)
	}

	for {
		if ctx.Err() != nil {
			return fmt.Errorf("subtask %q review canceled: %w", id, ctx.Err())
		}

		// Transition to reviewing and persist BEFORE invoking the reviewer, so an
		// interruption here is recoverable: resume rewinds reviewing → pending and
		// the scheduler re-enters the loop, re-reviewing the current diff at the
		// persisted Loops count (the resume contract on Store.Load).
		if err := commit(func(st *run.Subtask) { st.Status = run.SubtaskReviewing }); err != nil {
			return fmt.Errorf("marking subtask %q reviewing: %w", id, err)
		}

		// The round number is 1-based and tracks Loops: the first (free) review is
		// round 1; each remediation increments Loops, so the Nth review is round
		// Loops+1. Reading Loops off a fresh snapshot keeps resume correct.
		cur, ok := l.runner.subtaskSnapshot(id)
		if !ok {
			return fmt.Errorf("subtask %q vanished during review", id)
		}
		round := cur.Loops + 1

		verdict, err := l.review(ctx, cur, round)
		if err != nil {
			return err // a hard review error (unparseable after a re-ask, or I/O).
		}

		if verdict.Approved {
			l.progress.SubtaskReviewVerdict(id, round, true, 0)
			return commit(func(st *run.Subtask) {
				st.Status = run.SubtaskDone
				st.Unresolved = nil // converged cleanly: clear any prior flags.
			})
		}

		// Not approved. If the remediation budget is exhausted, proceed flagged:
		// mark done but carry the open findings forward for senior review / the
		// user to adjudicate (the spec'd default so the pipeline completes).
		if reachedMaxLoops(cur.Loops, cfg.MaxLoops) {
			l.progress.Logf(
				"  [%s] review did not converge after %d remediation cycle(s) (maxLoops=%d); "+
					"proceeding flagged with %d unresolved finding(s)",
				id, cur.Loops, cfg.MaxLoops, len(verdict.Findings))
			carried := toRunFindings(verdict.Findings)
			return commit(func(st *run.Subtask) {
				st.Status = run.SubtaskDone
				st.Unresolved = carried
			})
		}

		// Budget remains: record one more remediation cycle, then remediate.
		if err := commit(func(st *run.Subtask) { st.Status = run.SubtaskImplementing; st.Loops++ }); err != nil {
			return fmt.Errorf("recording remediation loop for subtask %q: %w", id, err)
		}
		l.progress.SubtaskReviewVerdict(id, round, false, len(verdict.Findings))

		if _, err := l.runner.runExecutor(ctx, id, verdict.Findings); err != nil {
			// A remediation executor failure is a context cancellation (fatal) or a
			// per-subtask failure; surface it so the scheduler records the subtask
			// failed rather than silently looping on a stale diff.
			if ctx.Err() != nil {
				return fmt.Errorf("subtask %q remediation canceled: %w", id, ctx.Err())
			}
			return fmt.Errorf("remediating subtask %q (round %d): %w", id, round, err)
		}
		// Loop: re-review the freshly-recaptured diff.
	}
}

// review renders the subtask-reviewer prompt for st against the current diff,
// invokes the reviewer harness, parses the verdict, and persists the round to
// subtasks/<id>/reviews/round-N.md. It applies the one-lenient-re-ask policy for
// malformed output (see invokeReviewer). It returns the parsed verdict or a hard
// error.
func (l *subtaskReviewLoop) review(ctx context.Context, st run.Subtask, round int) (Verdict, error) {
	diff, err := l.readDiff(st.ID)
	if err != nil {
		return Verdict{}, err
	}

	rawText, verdict, err := l.invokeReviewer(ctx, st, diff)
	// Persist the round regardless of parse outcome (when we have raw text), so a
	// failed/malformed review is inspectable on disk rather than lost.
	if rawText != "" {
		if perr := l.persistRound(st.ID, round, rawText, verdict, err); perr != nil {
			return Verdict{}, perr
		}
	}
	if err != nil {
		return Verdict{}, err
	}
	return verdict, nil
}

// invokeReviewer renders + runs the subtaskReviewer once and parses its verdict,
// applying the shared one-lenient-re-ask policy (invokeReviewerWithReask): on a
// PARSE failure it re-asks the reviewer EXACTLY ONCE and treats a second parse
// failure as a hard error; a harness/transport error is returned immediately. It
// returns the raw text of the LAST attempt (for persistence), the parsed verdict,
// and any error. The once-func is runReviewer bound to this subtask's diff.
func (l *subtaskReviewLoop) invokeReviewer(ctx context.Context, st run.Subtask, diff string) (raw string, v Verdict, err error) {
	return invokeReviewerWithReask(ctx, fmt.Sprintf("subtask %q", st.ID), l.progress,
		func(ctx context.Context) (string, error) {
			return l.runReviewer(ctx, st, diff)
		})
}

// runReviewer renders the subtask-reviewer prompt and runs the reviewer harness in
// the repo root with the reviewer role's model/permissionMode/timeout, returning
// the agent's final text. The reviewer is read-only on the repo; the git-safety
// preamble is guaranteed by the renderer.
func (l *subtaskReviewLoop) runReviewer(ctx context.Context, st run.Subtask, diff string) (string, error) {
	promptText, err := l.renderer.Render(l.role.PromptTemplate, prompt.SubtaskReviewerContext{
		Subtask: toPromptSubtask(st),
		Diff:    diff,
	})
	if err != nil {
		return "", fmt.Errorf("rendering subtask-reviewer prompt: %w", err)
	}
	res, err := l.reviewer.Run(ctx, harness.Request{
		Prompt:         promptText,
		Role:           "subtask-reviewer",
		Model:          l.role.Model,
		WorkDir:        l.repoRoot,
		PermissionMode: l.role.PermissionMode,
		Timeout:        l.role.Timeout.Std(),
	})
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

// readDiff reads the subtask's current diff.patch (the artifact runExecutor wrote
// / overwrote). A missing diff is an error: the reviewer needs something to judge,
// and by the time the hook runs the scheduler has already captured one.
func (l *subtaskReviewLoop) readDiff(id string) (string, error) {
	data, err := os.ReadFile(l.layout.SubtaskDiffFile(id))
	if err != nil {
		return "", fmt.Errorf("reading diff for subtask %q review: %w", id, err)
	}
	return string(data), nil
}

// persistRound writes a human-readable record of one review round to
// subtasks/<id>/reviews/round-N.md: the parsed verdict (or the parse error) plus
// the raw reviewer output. This is what makes the loop inspectable AND resumable —
// resume re-enters at Loops, and the round files show what each round decided. The
// write is plain file I/O.
func (l *subtaskReviewLoop) persistRound(id string, round int, raw string, v Verdict, parseErr error) error {
	dir := l.layout.SubtaskReviewsDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating reviews dir for subtask %q: %w", id, err)
	}
	path := l.layout.SubtaskReviewRoundFile(id, round)
	title := fmt.Sprintf("Subtask %s — review round %d", id, round)
	if err := os.WriteFile(path, []byte(renderReviewRound(title, raw, v, parseErr)), 0o644); err != nil {
		return fmt.Errorf("writing review round %d for subtask %q: %w", round, id, err)
	}
	return nil
}

// resolveSubtaskReviewer resolves the subtaskReviewer harness from the registry by
// the subtaskReviewer role's harness name, returning the harness and the role
// (model/permissionMode/template/timeout). An unknown reviewer harness is an
// actionable error rather than a nil-deref later. Used by NewSchedulerWithReview
// to build the loop.
func resolveSubtaskReviewer(cfg config.Config, reg *harness.Registry) (harness.Harness, config.Role, error) {
	role := cfg.Roles.SubtaskReviewer
	h, ok := reg.Get(role.Harness)
	if !ok {
		return nil, config.Role{}, fmt.Errorf(
			"pipeline: subtaskReviewer harness %q is not defined (known: %s)",
			role.Harness, strings.Join(reg.Names(), ", "))
	}
	return h, role, nil
}

// NewSchedulerWithReview builds a Scheduler whose per-subtask review step is the
// real executor↔reviewer loop (this ticket's deliverable), rather than the default
// no-op hook. It constructs the base scheduler, resolves the subtaskReviewer
// harness/role, binds the loop's narrow runner + collaborators from the live
// scheduler, and installs loop.Hook.
//
// This is the production entrypoint AIX-0013 will use to wire `run`/`resume`; until
// then it is exercised directly by tests. Callers that want the no-op hook (e.g.
// pure scheduling tests) keep using NewScheduler. Extra options are applied to the
// base scheduler before the hook is installed, so an explicit WithReviewHook can
// still override (last option wins) if a test wants to.
func NewSchedulerWithReview(
	r *run.Run,
	cfg config.Config,
	reg *harness.Registry,
	gw gitGateway,
	store *run.Store,
	renderer *prompt.Renderer,
	opts ...SchedulerOption,
) (*Scheduler, error) {
	if reg == nil {
		return nil, errors.New("pipeline: NewSchedulerWithReview requires a harness registry")
	}
	reviewer, role, err := resolveSubtaskReviewer(cfg, reg)
	if err != nil {
		return nil, err
	}

	s, err := NewScheduler(r, cfg, reg, gw, store, renderer, opts...)
	if err != nil {
		return nil, err
	}
	loop := newSubtaskReviewLoop(
		s, reviewer, role,
		s.renderer, s.progress, s.git.RepoRoot(), s.layout(),
		s.cfg.Pipeline.SubtaskReview, s.dryRun,
	)
	WithReviewHook(loop.Hook)(s)
	return s, nil
}
