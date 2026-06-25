package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jaxmef/aixecutor/internal/config"
	"github.com/jaxmef/aixecutor/internal/harness"
	"github.com/jaxmef/aixecutor/internal/prompt"
	"github.com/jaxmef/aixecutor/internal/run"
)

// ---------------------------------------------------------------------------
// Senior-review test helpers. All hermetic: a recording Mock senior reviewer, a
// recording Mock executor, a fake git gateway returning canned full diffs. No
// real agent, network, or git — and no git WRITES anywhere (the full diff is the
// fake's read-only FullDiff; remediation edits would be the executor sub-agent's,
// here a no-op mock).
// ---------------------------------------------------------------------------

// seniorCfg returns a default config tuned for the senior-review phase: the
// executor and seniorReviewer bound to DISTINCT harness names ("claude" for the
// executor, "pi" for the senior reviewer — both exist in the default harnesses) so
// the two agents can be scripted and asserted independently.
func seniorCfg() config.Config {
	cfg := config.Default()
	cfg.Roles.Executor.Harness = "claude"
	cfg.Roles.SeniorReviewer.Harness = "pi"
	return cfg
}

// seniorRegistry builds a registry whose executor-harness name resolves to exec and
// whose seniorReviewer-harness name resolves to rev.
func seniorRegistry(t *testing.T, cfg config.Config, exec, rev harness.Harness) *harness.Registry {
	t.Helper()
	reg, err := harness.NewRegistry(cfg, harness.Options{
		Factories: map[string]harness.Factory{
			cfg.Roles.Executor.Harness: func(string, config.Harness) (harness.Harness, error) {
				return exec, nil
			},
			cfg.Roles.SeniorReviewer.Harness: func(string, config.Harness) (harness.Harness, error) {
				return rev, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	return reg
}

// newSeniorPhase wires a seniorReviewPhase over the given collaborators with quiet
// output. It mirrors newReviewLoopScheduler for the senior phase.
func newSeniorPhase(t *testing.T, cfg config.Config, store *run.Store, fg gitGateway, exec, rev harness.Harness) *seniorReviewPhase {
	t.Helper()
	reg := seniorRegistry(t, cfg, exec, rev)
	p, err := NewSeniorReviewPhase(cfg, reg, fg, store, prompt.NewRenderer(),
		WithSeniorReviewOutput(&strings.Builder{}))
	if err != nil {
		t.Fatalf("new senior review phase: %v", err)
	}
	return p
}

// doneSubtask builds a done subtask (the precondition for the senior phase) with
// the given id and optional carried-forward unresolved findings.
func doneSubtask(id string, unresolved ...run.Finding) run.Subtask {
	s := pending(id, nil, id+".go")
	s.Status = run.SubtaskDone
	s.Unresolved = unresolved
	return s
}

// readSeniorRoundFiles returns the sorted basenames of the files persisted under a
// run's senior-review dir, so a test can assert which round/report artifacts exist.
func readSeniorReviewFiles(t *testing.T, store *run.Store, runID string) []string {
	t.Helper()
	layout := run.Layout{RunsDir: store.RunsDir(), ID: runID, DocsSubdir: "docs"}
	entries, err := os.ReadDir(layout.SeniorReviewDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading senior-review dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// hasFile reports whether names contains want.
func hasFile(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Tests.
// ---------------------------------------------------------------------------

// TestSeniorReviewCleanFirst proves the happy path: with all subtasks done, the
// senior reviewer runs ONCE over the full diff and approves; the phase ends clean,
// exactly one round file (round-1.md) is persisted, Rounds stays 0, the run's
// SeniorReview.Status is done, and the executor (remediation) is never invoked.
func TestSeniorReviewCleanFirst(t *testing.T) {
	cfg := seniorCfg()
	store, r := newSchedRun(t, cfg, []run.Subtask{doneSubtask("st-01"), doneSubtask("st-02")})

	exec := harness.NewMock("executor") // must never be called.
	rev := harness.NewMock("senior")
	rev.PushText(approvedYAML())

	fg := newFakeGit(t.TempDir())
	fg.fullDiffs = []string{"+++ st-01.go\npackage a\n"}

	p := newSeniorPhase(t, cfg, store, fg, exec, rev)
	if err := p.Run(context.Background(), r); err != nil {
		t.Fatalf("senior review: %v", err)
	}

	if r.SeniorReview.Status != run.SeniorReviewDone {
		t.Errorf("SeniorReview.Status = %q; want done", r.SeniorReview.Status)
	}
	if r.SeniorReview.Rounds != 0 {
		t.Errorf("SeniorReview.Rounds = %d; want 0 (approved on the free first review)", r.SeniorReview.Rounds)
	}
	// Clean convergence: no unresolved findings recorded on the run.
	if len(r.SeniorReview.Unresolved) != 0 {
		t.Errorf("SeniorReview.Unresolved = %+v; want empty on a clean convergence", r.SeniorReview.Unresolved)
	}
	if rev.CallCount() != 1 {
		t.Errorf("senior reviewer invoked %d times; want 1", rev.CallCount())
	}
	if exec.CallCount() != 0 {
		t.Errorf("executor invoked %d times; want 0 (clean, no remediation)", exec.CallCount())
	}
	if fg.fullDiffCalls() != 1 {
		t.Errorf("FullDiff called %d times; want 1", fg.fullDiffCalls())
	}
	got := readSeniorReviewFiles(t, store, r.ID)
	if len(got) != 1 || got[0] != "round-1.md" {
		t.Errorf("senior-review files = %v; want [round-1.md]", got)
	}
	// The run was transitioned to seniorReview and persisted.
	reloaded, err := store.Load(r.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Status != run.StatusSeniorReview {
		t.Errorf("reloaded run status = %q; want seniorReview", reloaded.Status)
	}
	if reloaded.SeniorReview.Status != run.SeniorReviewDone {
		t.Errorf("reloaded SeniorReview.Status = %q; want done", reloaded.SeniorReview.Status)
	}
}

// TestSeniorReviewReviewsFullDiff proves the senior reviewer is invoked with the
// FULL baseline→current diff (not a subtask slice) and that the full diff is
// RECOMPUTED each round: the fake returns a different patch per round, and the
// reviewer's prompt on round 2 must carry the SECOND patch (the post-remediation
// diff), proving the loop did not reuse round 1's diff.
func TestSeniorReviewReviewsFullDiff(t *testing.T) {
	cfg := seniorCfg()
	store, r := newSchedRun(t, cfg, []run.Subtask{doneSubtask("st-01")})

	exec := harness.NewMock("executor")
	exec.DefaultResult = harness.Result{Text: "fixed"}
	rev := harness.NewMock("senior")
	rev.PushText(findingsYAML("integration is broken")) // round 1: reject
	rev.PushText(approvedYAML())                        // round 2: approve

	fg := newFakeGit(t.TempDir())
	// Distinct full diffs per round to prove recomputation.
	fg.fullDiffs = []string{
		"+++ whole.go\nBEFORE-REMEDIATION\n",
		"+++ whole.go\nAFTER-REMEDIATION\n",
	}

	p := newSeniorPhase(t, cfg, store, fg, exec, rev)
	if err := p.Run(context.Background(), r); err != nil {
		t.Fatalf("senior review: %v", err)
	}

	if fg.fullDiffCalls() != 2 {
		t.Fatalf("FullDiff called %d times; want 2 (recomputed each round)", fg.fullDiffCalls())
	}
	reqs := rev.Requests()
	if len(reqs) != 2 {
		t.Fatalf("senior reviewer invoked %d times; want 2", len(reqs))
	}
	// Round 1 saw the baseline→current diff (the full diff, by construction).
	if !strings.Contains(reqs[0].Prompt, "BEFORE-REMEDIATION") {
		t.Errorf("round 1 prompt should carry the round-1 full diff:\n%s", reqs[0].Prompt)
	}
	// Round 2 saw the RECOMPUTED full diff, not a reuse of round 1.
	if !strings.Contains(reqs[1].Prompt, "AFTER-REMEDIATION") {
		t.Errorf("round 2 prompt should carry the RECOMPUTED full diff:\n%s", reqs[1].Prompt)
	}
	if strings.Contains(reqs[1].Prompt, "BEFORE-REMEDIATION") {
		t.Errorf("round 2 prompt must NOT reuse round 1's diff:\n%s", reqs[1].Prompt)
	}
	// The full diff was computed against the persisted run baseline directory.
	for i, dir := range fg.fullDiffBaselineDirs {
		if dir != r.Baseline.Dir {
			t.Errorf("FullDiff call %d baselineDir = %q; want the run baseline %q", i, dir, r.Baseline.Dir)
		}
	}
}

// TestSeniorReviewRemediationCarriesFindings proves the findings→remediation→
// re-review cycle: a rejection drives ONE executor pass whose prompt carries the
// senior reviewer's findings, Rounds increments to 1 and persists, and a re-review
// then approves. The executor (remediation) is invoked exactly once, with the
// findings injected.
func TestSeniorReviewRemediationCarriesFindings(t *testing.T) {
	cfg := seniorCfg()
	store, r := newSchedRun(t, cfg, []run.Subtask{doneSubtask("st-01")})

	exec := harness.NewMock("executor")
	exec.DefaultResult = harness.Result{Text: "remediated"}
	rev := harness.NewMock("senior")
	rev.PushText(findingsYAML("cross-cutting bug"))
	rev.PushText(approvedYAML())

	fg := newFakeGit(t.TempDir())
	fg.fullDiffs = []string{"+++ a.go\nv1\n", "+++ a.go\nv2\n"}

	p := newSeniorPhase(t, cfg, store, fg, exec, rev)
	if err := p.Run(context.Background(), r); err != nil {
		t.Fatalf("senior review: %v", err)
	}

	if r.SeniorReview.Status != run.SeniorReviewDone {
		t.Errorf("SeniorReview.Status = %q; want done", r.SeniorReview.Status)
	}
	if r.SeniorReview.Rounds != 1 {
		t.Errorf("SeniorReview.Rounds = %d; want 1 (one remediation cycle)", r.SeniorReview.Rounds)
	}
	if exec.CallCount() != 1 {
		t.Fatalf("executor invoked %d times; want 1 (one remediation pass)", exec.CallCount())
	}
	// The remediation executor prompt must carry the senior reviewer's finding.
	if reqs := exec.Requests(); !strings.Contains(reqs[0].Prompt, "cross-cutting bug") {
		t.Errorf("remediation executor prompt must carry the senior finding:\n%s", reqs[0].Prompt)
	}
	// Rounds persisted to run.yaml.
	reloaded, err := store.Load(r.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.SeniorReview.Rounds != 1 {
		t.Errorf("persisted SeniorReview.Rounds = %d; want 1", reloaded.SeniorReview.Rounds)
	}
	// Two review rounds + one remediation artifact persisted.
	got := readSeniorReviewFiles(t, store, r.ID)
	for _, want := range []string{"round-1.md", "round-2.md", "remediation-1.md"} {
		if !hasFile(got, want) {
			t.Errorf("senior-review files %v missing %q", got, want)
		}
	}
}

// TestSeniorReviewStopsAtMaxLoops proves the cap: a reviewer that NEVER approves
// makes the loop spend exactly maxLoops remediation cycles, then COMPLETE the phase
// (no error) with the remaining findings reported to unresolved.md. Status is done
// (the phase converged on "give up and report"), Rounds == maxLoops.
func TestSeniorReviewStopsAtMaxLoops(t *testing.T) {
	cfg := seniorCfg()
	cfg.Pipeline.SeniorReview.MaxLoops = 2
	store, r := newSchedRun(t, cfg, []run.Subtask{doneSubtask("st-01")})

	exec := harness.NewMock("executor")
	exec.DefaultResult = harness.Result{Text: "tried"}
	rev := harness.NewMock("senior")
	rev.DefaultResult = harness.Result{Text: findingsYAML("still not integrated")}

	fg := newFakeGit(t.TempDir())
	fg.fullDiffs = []string{"+++ a.go\nx\n"}

	p := newSeniorPhase(t, cfg, store, fg, exec, rev)
	if err := p.Run(context.Background(), r); err != nil {
		t.Fatalf("senior review should complete (report-and-proceed), got: %v", err)
	}

	if r.SeniorReview.Status != run.SeniorReviewDone {
		t.Errorf("SeniorReview.Status = %q; want done (report-and-proceed at the cap)", r.SeniorReview.Status)
	}
	if r.SeniorReview.Rounds != 2 {
		t.Errorf("SeniorReview.Rounds = %d; want 2 (maxLoops remediation cycles)", r.SeniorReview.Rounds)
	}
	// Cap reached: the open findings are recorded on the run (the structured signal
	// the end-of-run summary reads), not just in unresolved.md.
	if len(r.SeniorReview.Unresolved) == 0 {
		t.Errorf("SeniorReview.Unresolved is empty; want the open finding(s) recorded at the cap")
	} else if !strings.Contains(r.SeniorReview.Unresolved[0].Message, "still not integrated") {
		t.Errorf("SeniorReview.Unresolved[0] = %+v; want the open finding message", r.SeniorReview.Unresolved[0])
	}
	// And it round-trips through run.yaml.
	if reloaded, err := store.Load(r.ID); err != nil {
		t.Fatalf("reload: %v", err)
	} else if len(reloaded.SeniorReview.Unresolved) != len(r.SeniorReview.Unresolved) {
		t.Errorf("persisted SeniorReview.Unresolved count = %d; want %d",
			len(reloaded.SeniorReview.Unresolved), len(r.SeniorReview.Unresolved))
	}
	// Reviews: free first review + 2 remediation re-reviews = 3 rounds. Executor: 2
	// remediations.
	if rev.CallCount() != 3 {
		t.Errorf("senior reviewer invoked %d times; want 3 (1 free + 2 re-reviews)", rev.CallCount())
	}
	if exec.CallCount() != 2 {
		t.Errorf("executor invoked %d times; want 2 (maxLoops remediations)", exec.CallCount())
	}
	got := readSeniorReviewFiles(t, store, r.ID)
	for _, want := range []string{"round-1.md", "round-2.md", "round-3.md", "unresolved.md"} {
		if !hasFile(got, want) {
			t.Errorf("senior-review files %v missing %q", got, want)
		}
	}
	// The unresolved report names the open finding.
	body, err := os.ReadFile(filepath.Join(store.RunsDir(), r.ID, "senior-review", "unresolved.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "still not integrated") {
		t.Errorf("unresolved.md missing the open finding:\n%s", body)
	}
}

// TestSeniorReviewUnlimitedLoopsUntilClean proves maxLoops: -1 keeps looping until
// the reviewer approves (here on the 3rd review = after two remediations); Rounds
// reaches 2 and the phase ends clean.
func TestSeniorReviewUnlimitedLoopsUntilClean(t *testing.T) {
	cfg := seniorCfg()
	cfg.Pipeline.SeniorReview.MaxLoops = -1
	store, r := newSchedRun(t, cfg, []run.Subtask{doneSubtask("st-01")})

	exec := harness.NewMock("executor")
	exec.DefaultResult = harness.Result{Text: "fix"}
	rev := harness.NewMock("senior")
	rev.PushText(findingsYAML("round1"))
	rev.PushText(findingsYAML("round2"))
	rev.PushText(approvedYAML()) // approve on the 3rd review (K=3).

	fg := newFakeGit(t.TempDir())
	fg.fullDiffs = []string{"+++ a.go\n1\n", "+++ a.go\n2\n", "+++ a.go\n3\n"}

	p := newSeniorPhase(t, cfg, store, fg, exec, rev)
	if err := p.Run(context.Background(), r); err != nil {
		t.Fatalf("senior review: %v", err)
	}

	if r.SeniorReview.Status != run.SeniorReviewDone {
		t.Errorf("SeniorReview.Status = %q; want done", r.SeniorReview.Status)
	}
	if r.SeniorReview.Rounds != 2 {
		t.Errorf("SeniorReview.Rounds = %d; want 2 (two remediations before approval)", r.SeniorReview.Rounds)
	}
	if rev.CallCount() != 3 {
		t.Errorf("senior reviewer invoked %d times; want 3", rev.CallCount())
	}
	// No unresolved report: it converged.
	if got := readSeniorReviewFiles(t, store, r.ID); hasFile(got, "unresolved.md") {
		t.Errorf("unresolved.md should not exist on a clean convergence; files=%v", got)
	}
	// And no unresolved findings recorded on the run.
	if len(r.SeniorReview.Unresolved) != 0 {
		t.Errorf("SeniorReview.Unresolved = %+v; want empty after clean convergence", r.SeniorReview.Unresolved)
	}
}

// TestSeniorReviewDisabledSkips proves seniorReview.enabled: false skips the phase
// entirely: the reviewer is never invoked, no full diff is computed, no round files
// are written, and SeniorReview.Status is skipped.
func TestSeniorReviewDisabledSkips(t *testing.T) {
	cfg := seniorCfg()
	cfg.Pipeline.SeniorReview.Enabled = false
	store, r := newSchedRun(t, cfg, []run.Subtask{doneSubtask("st-01")})

	exec := harness.NewMock("executor") // never called.
	rev := harness.NewMock("senior")    // never called.
	fg := newFakeGit(t.TempDir())

	p := newSeniorPhase(t, cfg, store, fg, exec, rev)
	if err := p.Run(context.Background(), r); err != nil {
		t.Fatalf("senior review: %v", err)
	}

	if r.SeniorReview.Status != run.SeniorReviewSkipped {
		t.Errorf("SeniorReview.Status = %q; want skipped", r.SeniorReview.Status)
	}
	if rev.CallCount() != 0 {
		t.Errorf("senior reviewer invoked %d times; want 0 (disabled)", rev.CallCount())
	}
	if fg.fullDiffCalls() != 0 {
		t.Errorf("FullDiff called %d times; want 0 (disabled)", fg.fullDiffCalls())
	}
	if got := readSeniorReviewFiles(t, store, r.ID); len(got) != 0 {
		t.Errorf("senior-review files = %v; want none (disabled)", got)
	}
	// Persisted as skipped.
	reloaded, err := store.Load(r.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.SeniorReview.Status != run.SeniorReviewSkipped {
		t.Errorf("reloaded SeniorReview.Status = %q; want skipped", reloaded.SeniorReview.Status)
	}
}

// TestSeniorReviewCarriesForwardSubtaskFindings proves the carried-forward path: a
// subtask whose review ended flagged (Subtask.Unresolved) has its findings passed
// to the senior reviewer's prompt as CarriedFindings.
func TestSeniorReviewCarriesForwardSubtaskFindings(t *testing.T) {
	cfg := seniorCfg()
	carried := run.Finding{Severity: "major", File: "pkg/x.go", Line: 7, Message: "left open by subtask review"}
	store, r := newSchedRun(t, cfg, []run.Subtask{
		doneSubtask("st-01", carried),
		doneSubtask("st-02"),
	})

	exec := harness.NewMock("executor")
	rev := harness.NewMock("senior")
	rev.PushText(approvedYAML())

	fg := newFakeGit(t.TempDir())
	fg.fullDiffs = []string{"+++ pkg/x.go\nstuff\n"}

	p := newSeniorPhase(t, cfg, store, fg, exec, rev)
	if err := p.Run(context.Background(), r); err != nil {
		t.Fatalf("senior review: %v", err)
	}

	reqs := rev.Requests()
	if len(reqs) != 1 {
		t.Fatalf("senior reviewer invoked %d times; want 1", len(reqs))
	}
	// The carried finding's message and the "carried forward" section must appear.
	if !strings.Contains(reqs[0].Prompt, "left open by subtask review") {
		t.Errorf("senior prompt should carry the subtask's unresolved finding:\n%s", reqs[0].Prompt)
	}
	if !strings.Contains(reqs[0].Prompt, "carried forward") {
		t.Errorf("senior prompt should render the carried-findings section:\n%s", reqs[0].Prompt)
	}
}

// TestSeniorReviewResumeReentersAtRound proves resume re-enters the loop at the
// persisted round rather than restarting from 0: a run pre-seeded with
// SeniorReview.Status=running and Rounds=1 (interrupted mid-phase) is re-reviewed;
// on approval the next round file is round-2.md (round-1.md is not clobbered) and
// Rounds stays 1.
func TestSeniorReviewResumeReentersAtRound(t *testing.T) {
	cfg := seniorCfg()
	store, r := newSchedRun(t, cfg, []run.Subtask{doneSubtask("st-01")})
	// Simulate an interrupted phase: one remediation already happened.
	r.Status = run.StatusSeniorReview
	r.SeniorReview.Status = run.SeniorReviewRunning
	r.SeniorReview.Rounds = 1
	if err := store.Save(r); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Pre-seed a round-1.md so the assertion that the NEW round is round-2.md is
	// meaningful (resume must not clobber round 1).
	layout := run.Layout{RunsDir: store.RunsDir(), ID: r.ID, DocsSubdir: "docs"}
	if err := os.MkdirAll(layout.SeniorReviewDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.SeniorReviewDir(), "round-1.md"), []byte("# prior round 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := harness.NewMock("executor")
	rev := harness.NewMock("senior")
	rev.PushText(approvedYAML()) // approve on resume's re-review.

	fg := newFakeGit(t.TempDir())
	fg.fullDiffs = []string{"+++ a.go\nresumed\n"}

	p := newSeniorPhase(t, cfg, store, fg, exec, rev)
	if err := p.Run(context.Background(), r); err != nil {
		t.Fatalf("senior review: %v", err)
	}

	if r.SeniorReview.Status != run.SeniorReviewDone {
		t.Errorf("SeniorReview.Status = %q; want done", r.SeniorReview.Status)
	}
	if r.SeniorReview.Rounds != 1 {
		t.Errorf("SeniorReview.Rounds = %d; want 1 preserved across resume", r.SeniorReview.Rounds)
	}
	// The re-review is round Rounds+1 = 2, so round-2.md must now exist alongside
	// the preserved round-1.md.
	got := readSeniorReviewFiles(t, store, r.ID)
	if !hasFile(got, "round-2.md") {
		t.Errorf("senior-review files = %v; want a round-2.md (resume re-entered at the persisted round)", got)
	}
	// Round 1 was not re-asked: the reviewer ran exactly once on resume.
	if rev.CallCount() != 1 {
		t.Errorf("senior reviewer invoked %d times on resume; want 1 (re-review the current round only)", rev.CallCount())
	}
	// The preserved round-1.md still holds the prior content (not clobbered).
	body, err := os.ReadFile(filepath.Join(layout.SeniorReviewDir(), "round-1.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "prior round 1") {
		t.Errorf("round-1.md was clobbered on resume:\n%s", body)
	}
}

// TestSeniorReviewAlreadyDoneIsNoop proves resume on a finished phase
// (SeniorReview.Status=done) is a no-op: the reviewer is not invoked again and no
// full diff is computed.
func TestSeniorReviewAlreadyDoneIsNoop(t *testing.T) {
	cfg := seniorCfg()
	store, r := newSchedRun(t, cfg, []run.Subtask{doneSubtask("st-01")})
	r.SeniorReview.Status = run.SeniorReviewDone
	if err := store.Save(r); err != nil {
		t.Fatalf("save: %v", err)
	}

	exec := harness.NewMock("executor")
	rev := harness.NewMock("senior")
	fg := newFakeGit(t.TempDir())

	p := newSeniorPhase(t, cfg, store, fg, exec, rev)
	if err := p.Run(context.Background(), r); err != nil {
		t.Fatalf("senior review: %v", err)
	}

	if rev.CallCount() != 0 {
		t.Errorf("senior reviewer invoked %d times; want 0 (already done)", rev.CallCount())
	}
	if fg.fullDiffCalls() != 0 {
		t.Errorf("FullDiff called %d times; want 0 (already done)", fg.fullDiffCalls())
	}
}

// TestSeniorReviewMalformedThenWellformed proves the one-lenient-re-ask policy: the
// reviewer's first response is unparseable, the loop re-asks once, the second
// response is a valid approval, and the phase ends clean. The reviewer is invoked
// twice for that single review round; the full diff is computed once (the re-ask
// reuses the same round's diff).
func TestSeniorReviewMalformedThenWellformed(t *testing.T) {
	cfg := seniorCfg()
	store, r := newSchedRun(t, cfg, []run.Subtask{doneSubtask("st-01")})

	exec := harness.NewMock("executor")
	rev := harness.NewMock("senior")
	rev.PushText("no verdict block here, oops") // malformed
	rev.PushText(approvedYAML())                // lenient re-ask succeeds

	fg := newFakeGit(t.TempDir())
	fg.fullDiffs = []string{"+++ a.go\nx\n"}

	p := newSeniorPhase(t, cfg, store, fg, exec, rev)
	if err := p.Run(context.Background(), r); err != nil {
		t.Fatalf("senior review: %v", err)
	}

	if r.SeniorReview.Status != run.SeniorReviewDone {
		t.Errorf("SeniorReview.Status = %q; want done (lenient re-ask recovered)", r.SeniorReview.Status)
	}
	if rev.CallCount() != 2 {
		t.Errorf("senior reviewer invoked %d times; want 2 (malformed + one re-ask)", rev.CallCount())
	}
	// The re-ask reuses the same round's diff: FullDiff computed once.
	if fg.fullDiffCalls() != 1 {
		t.Errorf("FullDiff called %d times; want 1 (re-ask reuses the round diff)", fg.fullDiffCalls())
	}
}

// TestSeniorReviewMalformedTwiceFails proves a reviewer that is unparseable even
// after the single lenient re-ask is a HARD error: Run returns an error and the
// malformed raw output is still persisted to round-1.md for inspection.
func TestSeniorReviewMalformedTwiceFails(t *testing.T) {
	cfg := seniorCfg()
	store, r := newSchedRun(t, cfg, []run.Subtask{doneSubtask("st-01")})

	exec := harness.NewMock("executor")
	rev := harness.NewMock("senior")
	rev.DefaultResult = harness.Result{Text: "never a verdict block"} // malformed on every call

	fg := newFakeGit(t.TempDir())
	fg.fullDiffs = []string{"+++ a.go\nx\n"}

	p := newSeniorPhase(t, cfg, store, fg, exec, rev)
	err := p.Run(context.Background(), r)
	if err == nil {
		t.Fatal("senior review should fail when the reviewer is unparseable twice")
	}
	if rev.CallCount() != 2 {
		t.Errorf("senior reviewer invoked %d times; want 2 (initial + one re-ask)", rev.CallCount())
	}
	// The malformed raw output is still persisted (round-1.md).
	got := readSeniorReviewFiles(t, store, r.ID)
	if !hasFile(got, "round-1.md") {
		t.Errorf("senior-review files = %v; want round-1.md (the failed round is kept)", got)
	}
}

// TestSeniorReviewReviewerFailureIsHardError proves a harness/transport error from
// the reviewer (not a parse error) is returned immediately, NOT retried as a
// lenient re-ask.
func TestSeniorReviewReviewerFailureIsHardError(t *testing.T) {
	cfg := seniorCfg()
	store, r := newSchedRun(t, cfg, []run.Subtask{doneSubtask("st-01")})

	exec := harness.NewMock("executor")
	rev := harness.NewMock("senior")
	rev.PushError(harness.Result{}, errors.New("reviewer transport blew up"))

	fg := newFakeGit(t.TempDir())
	fg.fullDiffs = []string{"+++ a.go\nx\n"}

	p := newSeniorPhase(t, cfg, store, fg, exec, rev)
	if err := p.Run(context.Background(), r); err == nil {
		t.Fatal("senior review should fail when the reviewer harness errors")
	}
	if rev.CallCount() != 1 {
		t.Errorf("senior reviewer invoked %d times; want 1 (transport error is not re-asked)", rev.CallCount())
	}
}

// TestNewSeniorReviewPhaseUnknownReviewer proves NewSeniorReviewPhase fails clearly
// when the seniorReviewer role references a harness that is not registered.
func TestNewSeniorReviewPhaseUnknownReviewer(t *testing.T) {
	cfg := seniorCfg()
	cfg.Roles.SeniorReviewer.Harness = "nope"
	store, _ := newSchedRun(t, cfg, []run.Subtask{doneSubtask("st-01")})

	// Registry with only the executor harness wired.
	reg, err := harness.NewRegistry(cfg, harness.Options{
		Factories: map[string]harness.Factory{
			cfg.Roles.Executor.Harness: func(string, config.Harness) (harness.Harness, error) {
				return harness.NewMock("executor"), nil
			},
		},
	})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	_, err = NewSeniorReviewPhase(cfg, reg, newFakeGit(t.TempDir()), store, prompt.NewRenderer())
	if err == nil {
		t.Fatal("NewSeniorReviewPhase should fail for an unknown seniorReviewer harness")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error should name the missing reviewer harness; got: %v", err)
	}
}
