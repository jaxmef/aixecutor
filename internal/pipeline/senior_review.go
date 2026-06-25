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

// seniorReviewPhase is the final pipeline phase (CLAUDE.md §3.3 step 3, AIX-0012):
// after every subtask is done, it reviews the WHOLE change — the full diff from
// the run-start baseline to the current working tree — and, while the senior
// reviewer reports findings, runs one executor remediation pass addressing all of
// them, recomputes the full diff, and re-reviews, up to seniorReview.maxLoops
// remediation cycles. The pipeline completes either way: a clean verdict ends it
// resolved, and exhausting the budget ends it with the remaining findings reported
// (NEVER committing — the user adjudicates; invariant #1).
//
// It is a sibling of the per-subtask review loop (review_subtask.go) and shares
// the same loop-control primitives (loop.go): the reachedMaxLoops budget rule, the
// one-lenient-re-ask policy for malformed output, and the round-rendering. The
// differences it owns are the diff SCOPE (the full baseline→current diff, not a
// single subtask's diff) and the persistence target (senior-review/round-N.md and
// run.SeniorReview, not a subtask's reviews dir).
type seniorReviewPhase struct {
	// reviewer drives the seniorReviewer agent; reviewerRole is its config
	// (model/permissionMode/template/timeout), so the role is fully configurable.
	reviewer     harness.Harness
	reviewerRole config.Role
	// executor drives the remediation pass; executorRole is the executor role's
	// config. Remediation reuses the EXECUTOR role (not a bespoke one) so the same
	// agent that built the change fixes it, with the senior findings injected.
	executor     harness.Harness
	executorRole config.Role

	cfg      config.Config
	git      gitGateway
	store    *run.Store
	renderer *prompt.Renderer

	// progress renders concise, semantic human progress (defaults to a stdout-backed
	// Progress). The phase runs after execution, single-threaded, but Progress is
	// concurrency-safe regardless.
	progress *log.Progress

	// dryRun marks that the reviewer harness is the dry-run wrapper (placeholder
	// result, not a parseable verdict). The phase then records a clean verdict
	// without invoking/parsing, so the whole pipeline completes under --dry-run
	// (mirrors the Planner and the subtask review loop).
	dryRun bool
}

// SeniorReviewOption configures a seniorReviewPhase at construction.
type SeniorReviewOption func(*seniorReviewPhase)

// WithSeniorReviewOutput sets where the phase prints human progress, by building a
// Progress over w. Defaults to os.Stdout. Tests pass a buffer to keep output quiet
// and assertable; WithSeniorReviewProgress is preferred when a shared Progress
// already exists.
func WithSeniorReviewOutput(w io.Writer) SeniorReviewOption {
	return func(p *seniorReviewPhase) {
		if w != nil {
			p.progress = log.NewProgress(w)
		}
	}
}

// WithSeniorReviewProgress sets the shared Progress the phase emits semantic events
// through. Defaults to a stdout-backed Progress.
func WithSeniorReviewProgress(pr *log.Progress) SeniorReviewOption {
	return func(p *seniorReviewPhase) {
		if pr != nil {
			p.progress = pr
		}
	}
}

// WithSeniorReviewDryRun marks the phase as running against the dry-run reviewer
// wrapper, so it records a clean verdict without invoking/parsing — letting the
// pipeline complete under --dry-run (symmetric with the Planner and subtask loop).
func WithSeniorReviewDryRun(dryRun bool) SeniorReviewOption {
	return func(p *seniorReviewPhase) { p.dryRun = dryRun }
}

// NewSeniorReviewPhase builds the senior-review phase, resolving the seniorReviewer
// and executor harnesses from the registry by their roles' harness names. An
// unknown harness for either role is an actionable error rather than a nil-deref
// later. This is the production entrypoint AIX-0013 will wire after the scheduler
// reports every subtask done; tests construct it directly via this same function.
func NewSeniorReviewPhase(
	cfg config.Config,
	reg *harness.Registry,
	gw gitGateway,
	store *run.Store,
	renderer *prompt.Renderer,
	opts ...SeniorReviewOption,
) (*seniorReviewPhase, error) {
	if reg == nil {
		return nil, errors.New("pipeline: NewSeniorReviewPhase requires a harness registry")
	}
	if store == nil {
		return nil, errors.New("pipeline: NewSeniorReviewPhase requires a run store")
	}
	if renderer == nil {
		return nil, errors.New("pipeline: NewSeniorReviewPhase requires a renderer")
	}

	reviewerRole := cfg.Roles.SeniorReviewer
	reviewer, ok := reg.Get(reviewerRole.Harness)
	if !ok {
		return nil, fmt.Errorf(
			"pipeline: seniorReviewer harness %q is not defined (known: %s)",
			reviewerRole.Harness, strings.Join(reg.Names(), ", "))
	}
	executorRole := cfg.Roles.Executor
	executor, ok := reg.Get(executorRole.Harness)
	if !ok {
		return nil, fmt.Errorf(
			"pipeline: executor harness %q (used for senior-review remediation) is not defined (known: %s)",
			executorRole.Harness, strings.Join(reg.Names(), ", "))
	}

	p := &seniorReviewPhase{
		reviewer:     reviewer,
		reviewerRole: reviewerRole,
		executor:     executor,
		executorRole: executorRole,
		cfg:          cfg,
		git:          gw,
		store:        store,
		renderer:     renderer,
		progress:     log.NewProgress(nil),
	}
	for _, o := range opts {
		o(p)
	}
	if p.progress == nil {
		p.progress = log.NewProgress(nil)
	}
	return p, nil
}

// Run executes the senior-review phase for r. The precondition (every subtask
// done) is the orchestrator's responsibility (AIX-0013); this method just operates
// on a run in that state. It transitions the run to seniorReview, runs the review/
// remediation loop, and on a normal outcome leaves SeniorReview.Status terminal
// (SeniorReviewDone) so the orchestrator can advance the run to completed. It
// returns an error only for a FATAL condition (context cancellation, an
// unparseable reviewer after the lenient re-ask, a persistence/diff/executor
// failure) — NOT for "the diff was not clean at the cap", which is a normal,
// reported outcome that still completes the phase.
func (p *seniorReviewPhase) Run(ctx context.Context, r *run.Run) error {
	if r == nil {
		return errors.New("pipeline: seniorReviewPhase.Run(nil run)")
	}

	cfg := p.cfg.Pipeline.SeniorReview

	// Phase disabled: record it skipped and return without invoking the reviewer
	// (CLAUDE.md §3.3 / schema seniorReview.enabled: false). The run is left ready
	// for the orchestrator to complete.
	if !cfg.Enabled {
		p.progress.Logf("senior review: disabled — skipping")
		return p.save(r, func() {
			r.SeniorReview.Enabled = false
			r.SeniorReview.Status = run.SeniorReviewSkipped
		})
	}

	p.progress.PhaseStarted("Senior review")

	// Already finished (e.g. resume after the phase completed): nothing to do.
	if r.SeniorReview.Status == run.SeniorReviewDone {
		p.progress.Logf("senior review: already complete — nothing to do")
		return nil
	}

	// Enter the phase. Persist seniorReview + the running senior-review status
	// BEFORE invoking the reviewer, so an interruption here is recoverable: resume
	// re-enters with Status=running at the persisted Rounds and re-reviews the
	// current full diff (see the resume contract on Store.Load).
	if err := p.save(r, func() {
		r.Status = run.StatusSeniorReview
		r.SeniorReview.Enabled = true
		r.SeniorReview.Status = run.SeniorReviewRunning
	}); err != nil {
		return fmt.Errorf("pipeline: entering senior review for run %q: %w", r.ID, err)
	}

	// Dry-run: the reviewer is the placeholder wrapper, so record a clean verdict
	// and finish the phase without invoking/parsing it. The whole pipeline thus
	// completes under --dry-run (mirrors the Planner and the subtask review loop).
	if p.dryRun {
		p.progress.Logf("senior review: [dry-run] skipping review — completing clean")
		return p.save(r, func() {
			r.SeniorReview.Status = run.SeniorReviewDone
			r.SeniorReview.Unresolved = nil
		})
	}

	// Carried-forward findings: the unresolved findings from every subtask whose
	// review loop ended flagged (AIX-0011, Subtask.Unresolved). They are gathered
	// ONCE and passed to the senior reviewer every round so it re-judges them
	// against the whole change.
	carried := p.carriedFindings(r)
	if len(carried) > 0 {
		p.progress.Logf("senior review: carrying %d unresolved finding(s) forward from subtask review", len(carried))
	}
	planSummary := p.planSummary(r)

	if cfg.MaxLoops < 0 {
		p.progress.Logf("senior review: reviewing the full diff (maxLoops unlimited — termination depends on the reviewer converging)")
	} else {
		p.progress.Logf("senior review: reviewing the full diff (up to %d remediation cycle(s))", cfg.MaxLoops)
	}

	for {
		if ctx.Err() != nil {
			return fmt.Errorf("pipeline: senior review canceled: %w", ctx.Err())
		}

		// round is 1-based and tracks Rounds: the first (free) review is round 1;
		// each remediation increments Rounds, so the Nth review is round Rounds+1.
		// Reading Rounds off the live run keeps resume correct (it re-enters here at
		// the persisted Rounds).
		round := r.SeniorReview.Rounds + 1

		// 1+2. Recompute the FULL diff and review it. The diff is recomputed every
		// round (not incremental) so the reviewer always judges the whole change as
		// it currently stands — including any fixes a prior remediation made.
		verdict, err := p.review(ctx, r, round, planSummary, carried)
		if err != nil {
			return err // a hard review error (unparseable after a re-ask, diff, or I/O).
		}

		// 3. Clean → done. The whole change is acceptable; end the phase resolved.
		// Clear any Unresolved so a clean convergence is unambiguously clean in the
		// persisted state (defends against a resume that re-entered after a prior
		// cap-reached attempt).
		if verdict.Approved {
			p.progress.SeniorRound(round, true, 0)
			return p.save(r, func() {
				r.SeniorReview.Status = run.SeniorReviewDone
				r.SeniorReview.Unresolved = nil
			})
		}

		// 4. Not clean AND the remediation budget is exhausted: end the phase with
		// the remaining findings reported. The pipeline STILL completes — no git
		// writes; the user adjudicates the open findings (the spec'd outcome).
		if reachedMaxLoops(r.SeniorReview.Rounds, cfg.MaxLoops) {
			p.progress.Logf(
				"senior review: did not converge after %d remediation cycle(s) (maxLoops=%d); "+
					"completing with %d unresolved finding(s) reported",
				r.SeniorReview.Rounds, cfg.MaxLoops, len(verdict.Findings))
			if err := p.reportRemaining(r, round, verdict.Findings); err != nil {
				return err
			}
			// Record the open findings on the run (the structured counterpart to
			// unresolved.md) so the end-of-run summary reads state, not a filename:
			// Status=done + non-empty Unresolved == "completed report-and-proceed".
			unresolved := toRunFindings(verdict.Findings)
			return p.save(r, func() {
				r.SeniorReview.Status = run.SeniorReviewDone
				r.SeniorReview.Unresolved = unresolved
			})
		}

		// 5. Budget remains: remediate once (an executor pass addressing ALL current
		// findings), record the round, then loop to recompute the full diff and
		// re-review the fixes.
		if err := p.save(r, func() { r.SeniorReview.Rounds++ }); err != nil {
			return fmt.Errorf("pipeline: recording senior-review remediation round for run %q: %w", r.ID, err)
		}
		p.progress.SeniorRound(round, false, len(verdict.Findings))

		if err := p.remediate(ctx, r, round, verdict.Findings); err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("pipeline: senior-review remediation canceled: %w", ctx.Err())
			}
			return fmt.Errorf("pipeline: senior-review remediation (round %d) for run %q: %w", round, r.ID, err)
		}
		// Loop: recompute the full diff so the next review sees the remediation.
	}
}

// review computes the FULL diff (baseline → current tree), renders + runs the
// senior-reviewer prompt, parses the verdict (with the shared one-lenient-re-ask
// policy), and persists the round to senior-review/round-N.md. It returns the
// parsed verdict or a hard error. The round file is written whenever there is raw
// output, so a malformed/failed review is still inspectable on disk.
func (p *seniorReviewPhase) review(ctx context.Context, r *run.Run, round int, planSummary string, carried []Finding) (Verdict, error) {
	diff, err := p.fullDiff(ctx, r)
	if err != nil {
		return Verdict{}, err
	}

	rawText, verdict, err := invokeReviewerWithReask(ctx, "senior review", p.progress,
		func(ctx context.Context) (string, error) {
			return p.runReviewer(ctx, r, diff, planSummary, carried)
		})
	// Persist the round regardless of parse outcome (when we have raw text).
	if rawText != "" {
		if perr := p.persistRound(r, round, rawText, verdict, err); perr != nil {
			return Verdict{}, perr
		}
	}
	if err != nil {
		return Verdict{}, err
	}
	return verdict, nil
}

// fullDiff computes the full diff from the run-start baseline to the current
// working tree via the gateway (read-only). The baseline directory comes straight
// from the persisted run.Baseline.Dir, so this works identically on resume.
func (p *seniorReviewPhase) fullDiff(ctx context.Context, r *run.Run) (string, error) {
	diff, err := p.git.FullDiff(ctx, r.Baseline.Dir)
	if err != nil {
		return "", fmt.Errorf("computing the full diff for senior review: %w", err)
	}
	return diff.Patch, nil
}

// runReviewer renders the senior-reviewer prompt and runs the reviewer harness in
// the repo root with the seniorReviewer role's model/permissionMode/timeout,
// returning the agent's final text. The reviewer is read-only on the repo; the
// git-safety preamble is guaranteed by the renderer.
func (p *seniorReviewPhase) runReviewer(ctx context.Context, r *run.Run, diff, planSummary string, carried []Finding) (string, error) {
	promptText, err := p.renderer.Render(p.reviewerRole.PromptTemplate, prompt.SeniorReviewerContext{
		Task:            r.Task,
		PlanSummary:     planSummary,
		FullDiff:        diff,
		CarriedFindings: toPromptFindings(carried),
	})
	if err != nil {
		return "", fmt.Errorf("rendering senior-reviewer prompt: %w", err)
	}
	res, err := p.reviewer.Run(ctx, harness.Request{
		Prompt:         promptText,
		Role:           "senior-reviewer",
		Model:          p.reviewerRole.Model,
		WorkDir:        p.git.RepoRoot(),
		PermissionMode: p.reviewerRole.PermissionMode,
		Timeout:        p.reviewerRole.Timeout.Std(),
	})
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

// remediate runs ONE executor pass over the whole change, addressing all of the
// current senior-review findings (CLAUDE.md §3.3 senior-review remediation;
// AIX-0012 keeps it to a single pass per round — see the doc note below). It is
// represented NOT as a synthetic subtask in run.Subtasks (which would distort the
// done/total counts the scheduler and `status` derive from the DAG) but as a
// senior-review round: the run carries SeniorReview.Rounds and a remediate-*.md
// artifact per round for traceability, while the actual edits are made by the
// executor sub-agent in the repo (no app-level git writes).
//
// The executor prompt is rendered through the SAME path the subtask remediation
// uses (ExecutorContext.PriorFindings), so the agent sees the standard "address
// these findings" prompt; a synthetic whole-change "subtask" supplies the task
// framing. The executor runs in the repo root in acceptEdits and edits the tree;
// the next loop iteration recomputes the full diff so the re-review sees the fixes.
func (p *seniorReviewPhase) remediate(ctx context.Context, r *run.Run, round int, findings []Finding) error {
	st := p.remediationSubtask(round)
	promptText, err := p.renderer.Render(p.executorRole.PromptTemplate, prompt.ExecutorContext{
		Task:           r.Task,
		Subtask:        st,
		ContextExcerpt: "",
		PriorFindings:  toPromptFindings(findings),
		Baseline:       prompt.BaselineInfo{Description: seniorBaselineDescription},
	})
	if err != nil {
		return fmt.Errorf("rendering senior-review remediation prompt: %w", err)
	}

	// Record what we asked the remediation pass to fix, for traceability, before
	// invoking it (so an interrupted remediation still left a record of the ask).
	if perr := p.persistRemediation(r, round, findings); perr != nil {
		return perr
	}

	_, err = p.executor.Run(ctx, harness.Request{
		Prompt:         promptText,
		Role:           "senior-remediation",
		Model:          p.executorRole.Model,
		WorkDir:        p.git.RepoRoot(),
		PermissionMode: p.executorRole.PermissionMode,
		Timeout:        p.executorRole.Timeout.Std(),
	})
	if err != nil {
		return fmt.Errorf("senior-review remediation executor failed: %w", err)
	}
	return nil
}

// remediationSubtask builds the synthetic whole-change "subtask" spec that frames
// the senior-review remediation for the executor prompt. It is NOT persisted to
// run.Subtasks; it exists only to populate ExecutorContext. The id (sr-round-N)
// matches the ticket's suggested naming and shows up in the rendered prompt so the
// agent understands it is fixing whole-change review findings, not a single
// planned subtask. No file ownership is declared: the executor may touch any path
// the findings implicate.
func (p *seniorReviewPhase) remediationSubtask(round int) prompt.SubtaskSpec {
	return prompt.SubtaskSpec{
		ID:    fmt.Sprintf("sr-round-%d", round),
		Title: "Senior review remediation",
		Description: "The senior reviewer examined the entire change (all subtasks together) and " +
			"raised the findings below. Resolve every finding across the whole change, keeping all " +
			"subtasks consistent. Do not regress anything that is already correct.",
		Files:      nil,
		Acceptance: []string{"Every senior-review finding below is resolved.", "The change as a whole remains correct and consistent."},
		ManualTest: "",
	}
}

// carriedFindings collects the unresolved findings from every subtask whose review
// loop ended flagged (Subtask.Unresolved, AIX-0011) into the pipeline Finding
// shape, so they can be fed to the senior reviewer as CarriedFindings. Returns nil
// when no subtask carried any, so the prompt renders without the carried-findings
// section.
func (p *seniorReviewPhase) carriedFindings(r *run.Run) []Finding {
	var out []Finding
	for i := range r.Subtasks {
		for _, f := range r.Subtasks[i].Unresolved {
			out = append(out, fromRunFinding(f))
		}
	}
	return out
}

// planSummary reads the planner's docs/plan.md (resolved through the store layout,
// so it matches where planning wrote it) to summarize the plan for the reviewer.
// The read is best-effort: a missing or unreadable plan.md yields an empty summary
// rather than an error, since the senior reviewer can still judge the diff and a
// run should not fail because the optional doc is absent (e.g. a hand-seeded test
// run).
func (p *seniorReviewPhase) planSummary(r *run.Run) string {
	data, err := os.ReadFile(filepath.Join(p.store.DocsDir(r.ID), planDocName))
	if err != nil {
		return ""
	}
	return string(data)
}

// persistRound writes a human-readable record of one senior-review round to
// senior-review/round-N.md: the parsed verdict (or the parse error) plus the raw
// reviewer output. This is what makes the phase inspectable AND resumable — resume
// re-enters at Rounds, and the round files show what each round decided. The write
// is plain file I/O; the round rendering is shared with the subtask loop (loop.go).
func (p *seniorReviewPhase) persistRound(r *run.Run, round int, raw string, v Verdict, parseErr error) error {
	dir := p.layout(r).SeniorReviewDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating senior-review dir: %w", err)
	}
	title := fmt.Sprintf("Senior review — round %d", round)
	path := p.seniorRoundFile(r, round)
	if err := os.WriteFile(path, []byte(renderReviewRound(title, raw, v, parseErr)), 0o644); err != nil {
		return fmt.Errorf("writing senior-review round %d: %w", round, err)
	}
	return nil
}

// persistRemediation records the findings handed to a remediation round in
// senior-review/remediation-N.md, so the artifacts show what each remediation was
// asked to fix (the edits themselves land in the working tree and the next round's
// diff). Plain file I/O.
func (p *seniorReviewPhase) persistRemediation(r *run.Run, round int, findings []Finding) error {
	dir := p.layout(r).SeniorReviewDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating senior-review dir: %w", err)
	}
	path := filepath.Join(dir, fmt.Sprintf("remediation-%d.md", round))
	if err := os.WriteFile(path, []byte(renderRemediation(round, findings)), 0o644); err != nil {
		return fmt.Errorf("writing senior-review remediation %d: %w", round, err)
	}
	return nil
}

// reportRemaining persists the findings that were still open when the loop hit its
// maxLoops bound to senior-review/unresolved.md, so the user has a single, durable
// list of what the senior review could not get resolved. The pipeline still
// completes; this is the report side of the proceed-anyway outcome.
func (p *seniorReviewPhase) reportRemaining(r *run.Run, round int, findings []Finding) error {
	dir := p.layout(r).SeniorReviewDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating senior-review dir: %w", err)
	}
	path := filepath.Join(dir, "unresolved.md")
	if err := os.WriteFile(path, []byte(renderUnresolved(round, findings)), 0o644); err != nil {
		return fmt.Errorf("writing senior-review unresolved report: %w", err)
	}
	return nil
}

// save applies mutate to the run and persists it through the store. The phase is
// single-threaded (it runs after execution finishes), so no lock is needed; this
// helper centralizes the "mutate then Save, wrap the error" pattern used at every
// transition so persistence is consistent and resume always reads a coherent state.
func (p *seniorReviewPhase) save(r *run.Run, mutate func()) error {
	if mutate != nil {
		mutate()
	}
	if err := p.store.Save(r); err != nil {
		return fmt.Errorf("pipeline: persisting senior-review state for run %q: %w", r.ID, err)
	}
	return nil
}

// layout returns the run's artifact layout via the store, so artifact paths match
// Create/resume exactly (same pattern as the scheduler).
func (p *seniorReviewPhase) layout(r *run.Run) run.Layout {
	return run.Layout{RunsDir: p.store.RunsDir(), ID: r.ID, DocsSubdir: p.docsSubdir()}
}

// seniorRoundFile returns the senior-review/round-N.md path (1-based), reusing the
// same round-file naming as the subtask reviews so resume can find prior rounds
// deterministically.
func (p *seniorReviewPhase) seniorRoundFile(r *run.Run, round int) string {
	return filepath.Join(p.layout(r).SeniorReviewDir(), fmt.Sprintf("round-%d.md", round))
}

// docsSubdir returns the configured docs subdir name (defaulting to "docs"), so
// the layout the phase builds resolves the same docs dir the store does.
func (p *seniorReviewPhase) docsSubdir() string {
	if p.cfg.Paths.DocsSubdir != "" {
		return p.cfg.Paths.DocsSubdir
	}
	return "docs"
}

// seniorBaselineDescription is the phrase the executor remediation prompt uses to
// tell the agent how its change is judged in the senior phase: against the whole
// run-start working tree (the full diff), not a single subtask's slice.
const seniorBaselineDescription = "the working tree as it was when this run started (the whole change is reviewed together)"

// fromRunFinding maps a persisted run.Finding back onto the pipeline Finding shape,
// so unresolved subtask findings (Subtask.Unresolved) can be carried into the
// senior reviewer's context. It is the inverse of Finding.toRun (findings.go).
func fromRunFinding(f run.Finding) Finding {
	return Finding{
		Severity:   f.Severity,
		File:       f.File,
		Line:       f.Line,
		Message:    f.Message,
		Suggestion: f.Suggestion,
	}
}

// renderRemediation formats the findings a remediation round was asked to fix as
// Markdown, for the senior-review/remediation-N.md artifact.
func renderRemediation(round int, findings []Finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Senior review — remediation %d\n\n", round)
	fmt.Fprintf(&b, "The executor was asked to resolve %d finding(s) across the whole change:\n\n", len(findings))
	writeFindingList(&b, findings)
	return b.String()
}

// renderUnresolved formats the findings left open at the maxLoops cap as Markdown,
// for the senior-review/unresolved.md report.
func renderUnresolved(round int, findings []Finding) string {
	var b strings.Builder
	b.WriteString("# Senior review — unresolved findings\n\n")
	fmt.Fprintf(&b,
		"The senior review reached its remediation bound after round %d without the change becoming "+
			"clean. The following %d finding(s) remain open for you to adjudicate; the pipeline completed "+
			"WITHOUT committing — the working tree is left for your review.\n\n",
		round, len(findings))
	writeFindingList(&b, findings)
	return b.String()
}

// writeFindingList writes a Markdown bullet list of findings (severity, location,
// message, optional suggestion) to b. Shared by the remediation and unresolved
// reports so they render findings identically.
func writeFindingList(b *strings.Builder, findings []Finding) {
	for i, f := range findings {
		loc := f.File
		if f.File != "" && f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.File, f.Line)
		}
		fmt.Fprintf(b, "%d. [%s]", i+1, f.Severity)
		if loc != "" {
			fmt.Fprintf(b, " `%s`", loc)
		}
		fmt.Fprintf(b, " — %s\n", f.Message)
		if f.Suggestion != "" {
			fmt.Fprintf(b, "   - Suggestion: %s\n", f.Suggestion)
		}
	}
}
