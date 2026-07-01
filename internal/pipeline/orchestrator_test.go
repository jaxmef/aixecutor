package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jaxmef/aixecutor/internal/config"
	"github.com/jaxmef/aixecutor/internal/harness"
	"github.com/jaxmef/aixecutor/internal/prompt"
	"github.com/jaxmef/aixecutor/internal/run"
)

// ---------------------------------------------------------------------------
// Orchestrator test helpers. Everything is hermetic: mock harnesses for every
// role, a fake git gateway, a fake baseliner, a canned repo summarizer. No real
// agent, network, or git, and (asserted separately) no mutating git.
// ---------------------------------------------------------------------------

// orchRoles is the set of role names used in orchestrator tests; each role is
// bound to its OWN harness name so the four agents can be scripted and asserted
// independently (the default config points them all at "claude").
const (
	plannerHarness   = "h-planner"
	execHarness      = "h-exec"
	subRevHarness    = "h-subrev"
	seniorRevHarness = "h-senior"
)

// orchCfg returns a config wired so each pipeline role uses a distinct harness
// name, with serial execution for deterministic call ordering. The four harness
// names are added to cfg.Harnesses (the registry only builds harnesses that exist
// there) so the registry resolves each role to its own scripted mock. Every other
// knob is the default (autostart on, both review loops enabled, maxLoops 3).
func orchCfg() config.Config {
	cfg := config.Default()
	cfg.Pipeline.Execution.Parallel = false
	cfg.Roles.Planner.Harness = plannerHarness
	cfg.Roles.Executor.Harness = execHarness
	cfg.Roles.SubtaskReviewer.Harness = subRevHarness
	cfg.Roles.SeniorReviewer.Harness = seniorRevHarness

	// Register a harness entry per role name. The value is irrelevant (the test
	// registry's factories return mocks regardless of the config.Harness), but the
	// KEY must be present or NewRegistry won't build it.
	base := cfg.Harnesses["claude"]
	for _, name := range []string{plannerHarness, execHarness, subRevHarness, seniorRevHarness} {
		cfg.Harnesses[name] = base
	}
	return cfg
}

// orchHarnesses bundles the four role harnesses for a test, so a single registry
// can resolve each role to its own mock.
type orchHarnesses struct {
	planner *harness.Mock
	exec    harness.Harness // usually a *writingHarness so subtask diffs are non-empty
	subRev  *harness.Mock
	senior  *harness.Mock
}

// orchRegistry builds a registry resolving each role's harness name to the
// matching mock in hs.
func orchRegistry(t *testing.T, cfg config.Config, hs orchHarnesses) *harness.Registry {
	t.Helper()
	reg, err := harness.NewRegistry(cfg, harness.Options{
		Factories: map[string]harness.Factory{
			cfg.Roles.Planner.Harness:         func(string, config.Harness) (harness.Harness, error) { return hs.planner, nil },
			cfg.Roles.Executor.Harness:        func(string, config.Harness) (harness.Harness, error) { return hs.exec, nil },
			cfg.Roles.SubtaskReviewer.Harness: func(string, config.Harness) (harness.Harness, error) { return hs.subRev, nil },
			cfg.Roles.SeniorReviewer.Harness:  func(string, config.Harness) (harness.Harness, error) { return hs.senior, nil },
		},
	})
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	return reg
}

// newOrchTest wires an Orchestrator over a hermetic store (fake baseliner, fixed
// clock) and fake git gateway, with quiet output captured into the returned
// builder. It returns the orchestrator, the store, the fake git, and the output
// buffer so a test can drive it and assert on persisted state + summary.
func newOrchTest(t *testing.T, cfg config.Config, hs orchHarnesses) (*Orchestrator, *run.Store, *fakeGit, *strings.Builder) {
	t.Helper()
	root := t.TempDir()
	store, err := run.NewStoreFromConfig(cfg, root,
		run.WithBaseliner(fakeBaseliner{}),
		run.WithClock(fixedClock()),
	)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	fg := newFakeGit(root)
	reg := orchRegistry(t, cfg, hs)
	out := &strings.Builder{}
	o, err := NewOrchestrator(store, cfg, reg, fg, prompt.NewRenderer(), fakeSummarizer{summary: "internal/example/*.go"},
		WithOrchestratorOutput(out))
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}
	return o, store, fg, out
}

// fullPipelineHarnesses builds the four mocks for a converging full pipeline: the
// planner returns the standard two-subtask bundle; the executor writes each
// subtask's declared file (so its diff is non-empty); both reviewers approve.
func fullPipelineHarnesses() orchHarnesses {
	planner := harness.NewMock("planner")
	planner.PushText(goodBundle)

	exec := newWritingHarness("executor")
	// st-01 declares internal/example/type.go; st-02 declares .../use.go. Serial
	// execution runs them in dependency order, so write each on its call.
	exec.filesByCall = []map[string]string{
		{"internal/example/type.go": "package example\n\ntype T struct{}\n"},
		{"internal/example/use.go": "package example\n\nvar _ = T{}\n"},
	}
	exec.mock.DefaultResult = harness.Result{Text: "done"}

	subRev := harness.NewMock("subreviewer")
	subRev.DefaultResult = harness.Result{Text: approvedYAML()} // approve every subtask review

	senior := harness.NewMock("senior")
	senior.PushText(approvedYAML()) // approve the whole change on the first review

	return orchHarnesses{planner: planner, exec: exec, subRev: subRev, senior: senior}
}

// cancelingHarness is a Harness that, on its FIRST Run, invokes a cancel func
// (simulating a SIGINT landing mid-phase) and returns a context-canceled error,
// then behaves inertly on any later call. It drives the orchestrator's
// cancellation→aborted path deterministically without timing races. It is its own
// type (not a writingHarness) because it must return the context error rather than
// write files.
type cancelingHarness struct {
	name   string
	cancel context.CancelFunc
	calls  int
}

func newCancelingHarness(name string, cancel context.CancelFunc) *cancelingHarness {
	return &cancelingHarness{name: name, cancel: cancel}
}

func (h *cancelingHarness) Name() string { return h.name }

func (h *cancelingHarness) Run(ctx context.Context, _ harness.Request) (harness.Result, error) {
	h.calls++
	if h.calls == 1 {
		h.cancel()
		// Return a context error so the executor surfaces a cancellation (mirrors a
		// real harness whose subprocess was killed by the canceled context).
		return harness.Result{}, context.Canceled
	}
	return harness.Result{Text: "noop"}, nil
}

// ---------------------------------------------------------------------------
// Tests.
// ---------------------------------------------------------------------------

// TestOrchestratorFullPipeline is the headline end-to-end test: a fresh run drives
// planning → execution → per-subtask reviews → senior review → completed, all with
// mock harnesses. It asserts the run reaches completed, every subtask is done, the
// senior review is clean, and each role's agent was invoked.
func TestOrchestratorFullPipeline(t *testing.T) {
	cfg := orchCfg()
	hs := fullPipelineHarnesses()
	o, store, _, out := newOrchTest(t, cfg, hs)

	r, err := o.Start(context.Background(), "add the example feature")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if r.Status != run.StatusCompleted {
		t.Errorf("run status = %q; want completed", r.Status)
	}
	for _, st := range r.Subtasks {
		if st.Status != run.SubtaskDone {
			t.Errorf("subtask %s = %q; want done", st.ID, st.Status)
		}
	}
	if r.SeniorReview.Status != run.SeniorReviewDone {
		t.Errorf("SeniorReview.Status = %q; want done", r.SeniorReview.Status)
	}
	if len(r.SeniorReview.Unresolved) != 0 {
		t.Errorf("SeniorReview.Unresolved = %+v; want empty (clean)", r.SeniorReview.Unresolved)
	}

	// Each role's agent ran.
	if hs.planner.CallCount() != 1 {
		t.Errorf("planner invoked %d times; want 1", hs.planner.CallCount())
	}
	if hs.exec.(*writingHarness).callCount() != 2 {
		t.Errorf("executor invoked %d times; want 2 (one per subtask)", hs.exec.(*writingHarness).callCount())
	}
	if hs.subRev.CallCount() != 2 {
		t.Errorf("subtask reviewer invoked %d times; want 2 (one per subtask)", hs.subRev.CallCount())
	}
	if hs.senior.CallCount() != 1 {
		t.Errorf("senior reviewer invoked %d times; want 1", hs.senior.CallCount())
	}

	// Persisted state reflects completion.
	reloaded, err := store.Load(r.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Status != run.StatusCompleted {
		t.Errorf("reloaded status = %q; want completed", reloaded.Status)
	}

	// A summary is produced and ends with the no-commit reminder.
	var sum strings.Builder
	WriteSummary(&sum, r, store.DocsDir(r.ID), false)
	if !strings.Contains(sum.String(), "Nothing was committed") {
		t.Errorf("summary missing the no-commit reminder:\n%s", sum.String())
	}
	_ = out // progress output captured but not asserted here.
}

// TestOrchestratorAutostartOff proves autostartExecution:false stops after
// planning: the run reaches planned, NO execution happens (executor/reviewers
// never invoked), and the docs exist.
func TestOrchestratorAutostartOff(t *testing.T) {
	cfg := orchCfg()
	cfg.Pipeline.AutostartExecution = false
	hs := fullPipelineHarnesses()
	o, store, _, out := newOrchTest(t, cfg, hs)

	r, err := o.Start(context.Background(), "plan only")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if r.Status != run.StatusPlanned {
		t.Errorf("run status = %q; want planned (autostart off stops after planning)", r.Status)
	}
	if hs.exec.(*writingHarness).callCount() != 0 {
		t.Errorf("executor invoked %d times; want 0 (no execution)", hs.exec.(*writingHarness).callCount())
	}
	if hs.senior.CallCount() != 0 {
		t.Errorf("senior reviewer invoked %d times; want 0 (no execution)", hs.senior.CallCount())
	}
	// The planner ran and the subtasks are pending.
	if hs.planner.CallCount() != 1 {
		t.Errorf("planner invoked %d times; want 1", hs.planner.CallCount())
	}
	if len(r.Subtasks) != 2 {
		t.Fatalf("got %d subtasks; want 2", len(r.Subtasks))
	}
	for _, st := range r.Subtasks {
		if st.Status != run.SubtaskPending {
			t.Errorf("subtask %s = %q; want pending (not executed)", st.ID, st.Status)
		}
	}
	// The stop message names how to resume.
	if !strings.Contains(out.String(), "resume "+r.ID) {
		t.Errorf("autostart-off output should tell the user to resume %s:\n%s", r.ID, out.String())
	}

	reloaded, err := store.Load(r.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Status != run.StatusPlanned {
		t.Errorf("reloaded status = %q; want planned", reloaded.Status)
	}
}

// TestOrchestratorAutostartOnProceeds proves autostartExecution:true proceeds past
// planning automatically (no extra call), reaching completed in one Start.
func TestOrchestratorAutostartOnProceeds(t *testing.T) {
	cfg := orchCfg() // autostart is true by default.
	hs := fullPipelineHarnesses()
	o, _, _, _ := newOrchTest(t, cfg, hs)

	r, err := o.Start(context.Background(), "do it all")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if r.Status != run.StatusCompleted {
		t.Errorf("run status = %q; want completed (autostart proceeded through execution)", r.Status)
	}
	if hs.exec.(*writingHarness).callCount() == 0 {
		t.Error("executor never ran; autostart should have begun execution")
	}
}

// TestOrchestratorResumeAfterExecution proves resume continues from the persisted
// state WITHOUT redoing done work: a run is interrupted at the senior-review
// boundary (every subtask already done), then a fresh orchestrator (fresh mocks)
// resumes it. The new executor/subtask-reviewer mocks must NOT be invoked (done
// subtasks are skipped); only the senior reviewer runs, and the run reaches
// completed.
func TestOrchestratorResumeAfterExecution(t *testing.T) {
	cfg := orchCfg()

	// --- Phase 1: drive to the senior-review boundary, then get interrupted. ---
	// A canceling senior reviewer simulates a SIGINT landing right as senior review
	// begins: execution is fully done and persisted, the senior phase has entered
	// (Status=running), and the ctx cancellation aborts the run resumably. The
	// senior reviewer is not a *harness.Mock, so wire the registry directly.
	ctx1, cancel1 := context.WithCancel(context.Background())
	root := t.TempDir()
	store, err := run.NewStoreFromConfig(cfg, root,
		run.WithBaseliner(fakeBaseliner{}), run.WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	fg := newFakeGit(root)
	hs1 := fullPipelineHarnesses()
	reg1, err := harness.NewRegistry(cfg, harness.Options{
		Factories: map[string]harness.Factory{
			cfg.Roles.Planner.Harness:         func(string, config.Harness) (harness.Harness, error) { return hs1.planner, nil },
			cfg.Roles.Executor.Harness:        func(string, config.Harness) (harness.Harness, error) { return hs1.exec, nil },
			cfg.Roles.SubtaskReviewer.Harness: func(string, config.Harness) (harness.Harness, error) { return hs1.subRev, nil },
			cfg.Roles.SeniorReviewer.Harness: func(string, config.Harness) (harness.Harness, error) {
				return newCancelingHarness("senior-cancel", cancel1), nil
			},
		},
	})
	if err != nil {
		t.Fatalf("registry 1: %v", err)
	}
	o1, err := NewOrchestrator(store, cfg, reg1, fg, prompt.NewRenderer(),
		fakeSummarizer{summary: "x"}, WithOrchestratorOutput(&strings.Builder{}))
	if err != nil {
		t.Fatalf("new orchestrator 1: %v", err)
	}

	r1, err := o1.Start(ctx1, "resume me")
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("phase 1 should abort at senior review; got: %v", err)
	}
	// Every subtask is done and persisted; the run is aborted (resumable).
	reloaded, err := store.Load(r1.ID)
	if err != nil {
		t.Fatalf("reload after phase 1: %v", err)
	}
	if reloaded.Status != run.StatusAborted {
		t.Fatalf("after phase 1, run status = %q; want aborted", reloaded.Status)
	}
	for _, st := range reloaded.Subtasks {
		if st.Status != run.SubtaskDone {
			t.Fatalf("after phase 1, subtask %s = %q; want done", st.ID, st.Status)
		}
	}

	// --- Phase 2: resume with FRESH mocks; done work must not be redone. ---
	hs2 := orchHarnesses{
		planner: harness.NewMock("planner2"), // must NOT run (planning is done)
		exec:    newWritingHarness("exec2"),  // must NOT run (subtasks done)
		subRev:  harness.NewMock("subrev2"),  // must NOT run (subtasks done)
		senior:  harness.NewMock("senior2"),
	}
	hs2.senior.PushText(approvedYAML()) // this time the senior review approves.
	reg2 := orchRegistry(t, cfg, hs2)
	o2, err := NewOrchestrator(store, cfg, reg2, fg, prompt.NewRenderer(),
		fakeSummarizer{summary: "x"}, WithOrchestratorOutput(&strings.Builder{}))
	if err != nil {
		t.Fatalf("new orchestrator 2: %v", err)
	}

	r2, err := o2.Resume(context.Background(), reloaded.ID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if r2.Status != run.StatusCompleted {
		t.Errorf("resumed run status = %q; want completed", r2.Status)
	}
	// Done work was NOT redone.
	if hs2.planner.CallCount() != 0 {
		t.Errorf("planner re-invoked on resume %d times; want 0 (planning was done)", hs2.planner.CallCount())
	}
	if hs2.exec.(*writingHarness).callCount() != 0 {
		t.Errorf("executor re-invoked on resume %d times; want 0 (subtasks were done)", hs2.exec.(*writingHarness).callCount())
	}
	if hs2.subRev.CallCount() != 0 {
		t.Errorf("subtask reviewer re-invoked on resume %d times; want 0 (subtasks were done)", hs2.subRev.CallCount())
	}
	// Only the senior reviewer ran on resume.
	if hs2.senior.CallCount() != 1 {
		t.Errorf("senior reviewer invoked %d times on resume; want 1", hs2.senior.CallCount())
	}
}

// TestOrchestratorResumeAfterPlanningAutostartOff proves the plan→resume workflow:
// with autostartExecution OFF, Start stops at planned; an explicit resume then
// PROCEEDS past planning (resume always continues — it does not re-honor the
// stop-after-planning gate) to run execution + senior review to completion WITHOUT
// re-planning, even though autostart is STILL off.
func TestOrchestratorResumeAfterPlanningAutostartOff(t *testing.T) {
	cfgOff := orchCfg()
	cfgOff.Pipeline.AutostartExecution = false
	hs1 := fullPipelineHarnesses()
	o1, store, fg, _ := newOrchTest(t, cfgOff, hs1)

	r1, err := o1.Start(context.Background(), "plan then resume")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if r1.Status != run.StatusPlanned {
		t.Fatalf("run status = %q; want planned", r1.Status)
	}

	// Resume with autostart STILL OFF and fresh mocks; planning must not re-run, but
	// execution + senior review must proceed (resume continues past the gate).
	hs2 := fullPipelineHarnesses()
	reg2 := orchRegistry(t, cfgOff, hs2)
	o2, err := NewOrchestrator(store, cfgOff, reg2, fg, prompt.NewRenderer(),
		fakeSummarizer{summary: "x"}, WithOrchestratorOutput(&strings.Builder{}))
	if err != nil {
		t.Fatalf("new orchestrator 2: %v", err)
	}

	r2, err := o2.Resume(context.Background(), r1.ID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if r2.Status != run.StatusCompleted {
		t.Errorf("resumed run status = %q; want completed (resume proceeds past planning)", r2.Status)
	}
	// Planning was NOT redone (subtasks already existed + status >= planned).
	if hs2.planner.CallCount() != 0 {
		t.Errorf("planner re-invoked on resume %d times; want 0", hs2.planner.CallCount())
	}
	// Execution + senior review DID run on resume.
	if hs2.exec.(*writingHarness).callCount() != 2 {
		t.Errorf("executor invoked %d times on resume; want 2", hs2.exec.(*writingHarness).callCount())
	}
	if hs2.senior.CallCount() != 1 {
		t.Errorf("senior reviewer invoked %d times on resume; want 1", hs2.senior.CallCount())
	}
}

// TestOrchestratorCancellationAborts proves a context cancellation mid-run persists
// the run as `aborted` (resumable, NOT failed) and returns ErrAborted. A canceling
// executor drives the cancellation during execution; the run must end aborted and a
// follow-up resume must be able to complete it.
func TestOrchestratorCancellationAborts(t *testing.T) {
	cfg := orchCfg()

	ctx, cancel := context.WithCancel(context.Background())

	hs1 := fullPipelineHarnesses()
	// Replace the executor with one that cancels the context on its first call and
	// returns a context error, simulating a SIGINT landing mid-execution.
	cancelExec := newCancelingHarness("exec-cancel", cancel)
	hs1.exec = cancelExec
	o1, store, fg, _ := newOrchTest(t, cfg, hs1)

	r1, err := o1.Start(ctx, "cancel me")
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("Start error = %v; want ErrAborted", err)
	}
	// Persisted as aborted (resumable), not failed.
	reloaded, lerr := store.Load(r1.ID)
	if lerr != nil {
		t.Fatalf("reload: %v", lerr)
	}
	if reloaded.Status != run.StatusAborted {
		t.Errorf("reloaded status = %q; want aborted", reloaded.Status)
	}

	// --- Resume to completion with a fresh, non-canceling set of mocks. ---
	hs2 := fullPipelineHarnesses()
	reg2 := orchRegistry(t, cfg, hs2)
	o2, err := NewOrchestrator(store, cfg, reg2, fg, prompt.NewRenderer(),
		fakeSummarizer{summary: "x"}, WithOrchestratorOutput(&strings.Builder{}))
	if err != nil {
		t.Fatalf("new orchestrator 2: %v", err)
	}
	r2, err := o2.Resume(context.Background(), reloaded.ID)
	if err != nil {
		t.Fatalf("Resume after abort: %v", err)
	}
	if r2.Status != run.StatusCompleted {
		t.Errorf("resumed run status = %q; want completed (abort is resumable)", r2.Status)
	}
	// Planning was not redone on the abort-resume.
	if hs2.planner.CallCount() != 0 {
		t.Errorf("planner re-invoked after abort-resume %d times; want 0", hs2.planner.CallCount())
	}
}

// TestOrchestratorPhaseErrorFails proves a genuine (non-cancellation) phase error
// marks the run failed and is returned (NOT ErrAborted). The planner harness errors
// at transport, which is a hard failure of the very first phase.
func TestOrchestratorPhaseErrorFails(t *testing.T) {
	cfg := orchCfg()
	hs := fullPipelineHarnesses()
	hs.planner = harness.NewMock("planner-fail")
	hs.planner.PushError(harness.Result{}, errors.New("planner transport died"))
	o, store, _, _ := newOrchTest(t, cfg, hs)

	r, err := o.Start(context.Background(), "will fail")
	if err == nil {
		t.Fatal("Start should fail when the planner errors")
	}
	if errors.Is(err, ErrAborted) {
		t.Errorf("a planner transport error must not be classified as an abort: %v", err)
	}
	// The run is marked failed and persisted.
	reloaded, lerr := store.Load(r.ID)
	if lerr != nil {
		t.Fatalf("reload: %v", lerr)
	}
	if reloaded.Status != run.StatusFailed {
		t.Errorf("reloaded status = %q; want failed", reloaded.Status)
	}
}

// TestOrchestratorSummaryContent asserts the end-of-run summary includes the
// per-subtask outcomes, the senior verdict, the docs path, and the no-commit
// reminder — and, on a cap-reached senior review, the unresolved finding count.
func TestOrchestratorSummaryContent(t *testing.T) {
	cfg := orchCfg()
	cfg.Pipeline.SeniorReview.MaxLoops = 1
	hs := fullPipelineHarnesses()
	// Senior reviewer NEVER approves → cap reached at maxLoops=1 → report-and-proceed
	// with one unresolved finding.
	hs.senior = harness.NewMock("senior-stuck")
	hs.senior.DefaultResult = harness.Result{Text: findingsYAML("whole-change problem")}
	o, store, fg, _ := newOrchTest(t, cfg, hs)
	fg.fullDiffs = []string{"+++ internal/example/type.go\nx\n"}

	r, err := o.Start(context.Background(), "summary test")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if r.Status != run.StatusCompleted {
		t.Fatalf("run status = %q; want completed (report-and-proceed still completes)", r.Status)
	}

	var sum strings.Builder
	WriteSummary(&sum, r, store.DocsDir(r.ID), false)
	s := sum.String()

	for _, want := range []string{
		"Run summary",
		"Subtasks (2/2 done)",
		"st-01", "st-02", // per-subtask outcomes
		"done",                  // subtask status
		"Senior review:",        // verdict line
		"unresolved finding",    // cap-reached count phrasing
		"whole-change problem",  // the open finding listed
		store.DocsDir(r.ID),     // docs path
		"Nothing was committed", // the reminder
	} {
		if !strings.Contains(s, want) {
			t.Errorf("summary missing %q:\n%s", want, s)
		}
	}
	// The structured signal backs the summary: cap reached → Unresolved populated.
	if len(r.SeniorReview.Unresolved) == 0 {
		t.Error("SeniorReview.Unresolved should be populated at the cap")
	}
}

// TestOrchestratorDryRunCompletes proves the WHOLE pipeline converges under
// --dry-run with NO real agents: the registry wraps every harness in the dry-run
// wrapper (which returns a role-agnostic placeholder, never executing anything),
// and the orchestrator's dryRun flag makes planning write placeholder docs and
// both review phases short-circuit to approved. The run must reach completed. This
// is the hermetic counterpart to the `./bin/aixecutor --dry-run run` sanity check.
func TestOrchestratorDryRunCompletes(t *testing.T) {
	cfg := config.Default() // all roles → the default "claude" harness.
	cfg.Pipeline.Execution.Parallel = false

	// A dry-run registry over the default harnesses: every harness is wrapped so
	// Run never executes a real command (no binary needed).
	reg, err := harness.NewRegistry(cfg, harness.Options{DryRun: true})
	if err != nil {
		t.Fatalf("dry-run registry: %v", err)
	}

	root := t.TempDir()
	store, err := run.NewStoreFromConfig(cfg, root,
		run.WithBaseliner(fakeBaseliner{}), run.WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	fg := newFakeGit(root)

	o, err := NewOrchestrator(store, cfg, reg, fg, prompt.NewRenderer(),
		fakeSummarizer{summary: "x"},
		WithOrchestratorDryRun(true),
		WithOrchestratorOutput(&strings.Builder{}),
	)
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}

	r, err := o.Start(context.Background(), "add a hello flag")
	if err != nil {
		t.Fatalf("dry-run Start: %v", err)
	}
	if r.Status != run.StatusCompleted {
		t.Errorf("dry-run run status = %q; want completed", r.Status)
	}
	for _, st := range r.Subtasks {
		if st.Status != run.SubtaskDone {
			t.Errorf("dry-run subtask %s = %q; want done", st.ID, st.Status)
		}
	}
	if r.SeniorReview.Status != run.SeniorReviewDone {
		t.Errorf("dry-run SeniorReview.Status = %q; want done", r.SeniorReview.Status)
	}

	// The summary still ends with the no-commit reminder under dry-run.
	var sum strings.Builder
	WriteSummary(&sum, r, store.DocsDir(r.ID), false)
	if !strings.Contains(sum.String(), "Nothing was committed") {
		t.Errorf("dry-run summary missing the no-commit reminder:\n%s", sum.String())
	}
}

// TestOrchestratorUnknownPlannerHarness proves a missing planner harness is an
// actionable error (not a nil-deref panic): the planner role points at an
// unregistered harness, so driving fails clearly naming it.
func TestOrchestratorUnknownPlannerHarness(t *testing.T) {
	cfg := orchCfg()
	cfg.Roles.Planner.Harness = "ghost"
	// Do NOT register "ghost" in cfg.Harnesses, so the registry never builds it.
	delete(cfg.Harnesses, "ghost")
	hs := fullPipelineHarnesses()
	o, store, _, _ := newOrchTest(t, cfg, hs)

	r, err := o.Start(context.Background(), "no planner")
	if err == nil {
		t.Fatal("Start should fail when the planner harness is undefined")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error should name the missing planner harness; got: %v", err)
	}
	// The run is marked failed and persisted (not left mid-flight).
	if reloaded, lerr := store.Load(r.ID); lerr != nil {
		t.Fatalf("reload: %v", lerr)
	} else if reloaded.Status != run.StatusFailed {
		t.Errorf("reloaded status = %q; want failed", reloaded.Status)
	}
}

// TestOrchestratorRequiresCollaborators proves the constructor rejects missing
// required collaborators with clear errors.
func TestOrchestratorRequiresCollaborators(t *testing.T) {
	cfg := orchCfg()
	store := newTestStore(t)
	reg, err := harness.NewRegistry(cfg, harness.Options{})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	fg := newFakeGit(t.TempDir())
	rndr := prompt.NewRenderer()
	sum := fakeSummarizer{}

	cases := []struct {
		name string
		call func() (*Orchestrator, error)
	}{
		{"nil store", func() (*Orchestrator, error) { return NewOrchestrator(nil, cfg, reg, fg, rndr, sum) }},
		{"nil registry", func() (*Orchestrator, error) { return NewOrchestrator(store, cfg, nil, fg, rndr, sum) }},
		{"nil gateway", func() (*Orchestrator, error) { return NewOrchestrator(store, cfg, reg, nil, rndr, sum) }},
		{"nil renderer", func() (*Orchestrator, error) { return NewOrchestrator(store, cfg, reg, fg, nil, sum) }},
		{"nil summarizer", func() (*Orchestrator, error) { return NewOrchestrator(store, cfg, reg, fg, rndr, nil) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.call(); err == nil {
				t.Errorf("NewOrchestrator(%s) should error", tc.name)
			}
		})
	}
}
