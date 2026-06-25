package pipeline

import (
	"context"
	"fmt"
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
// Review-loop test helpers. All hermetic: a recording Mock reviewer, a
// file-writing executor, a fake git gateway. No real agent, network, or git.
// ---------------------------------------------------------------------------

// reviewCfg returns a default config tuned for the review loop: serial execution
// (deterministic call order) and the executor/reviewer bound to DISTINCT harness
// names ("claude" for the executor, "pi" for the reviewer — both exist in the
// default harnesses) so the two agents can be scripted and asserted independently.
func reviewCfg() config.Config {
	cfg := config.Default()
	cfg.Pipeline.Execution.Parallel = false
	cfg.Roles.Executor.Harness = "claude"
	cfg.Roles.SubtaskReviewer.Harness = "pi"
	return cfg
}

// registryWith2 builds a registry whose executor-harness name resolves to exec and
// whose subtaskReviewer-harness name resolves to rev. It mirrors registryWith but
// wires both roles to their own recording harness.
func registryWith2(t *testing.T, cfg config.Config, exec, rev harness.Harness) *harness.Registry {
	t.Helper()
	reg, err := harness.NewRegistry(cfg, harness.Options{
		Factories: map[string]harness.Factory{
			cfg.Roles.Executor.Harness: func(string, config.Harness) (harness.Harness, error) {
				return exec, nil
			},
			cfg.Roles.SubtaskReviewer.Harness: func(string, config.Harness) (harness.Harness, error) {
				return rev, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	return reg
}

// approvedYAML is a well-formed reviewer response that approves the change.
func approvedYAML() string {
	return "Looks good.\n\n```yaml\napproved: true\nfindings: []\n```\n"
}

// findingsYAML is a well-formed reviewer response that rejects the change with one
// blocker finding carrying the given message.
func findingsYAML(message string) string {
	return "Needs work.\n\n```yaml\napproved: false\nfindings:\n  - severity: blocker\n" +
		"    file: a/x.go\n    line: 1\n    message: \"" + message + "\"\n```\n"
}

// newReviewLoopScheduler wires a Scheduler with the REAL subtask review loop hook
// (via NewSchedulerWithReview) over r, using the given config, fake git, executor
// harness, and reviewer harness. It returns the scheduler.
func newReviewLoopScheduler(t *testing.T, cfg config.Config, store *run.Store, r *run.Run, fg gitGateway, exec, rev harness.Harness) *Scheduler {
	t.Helper()
	reg := registryWith2(t, cfg, exec, rev)
	s, err := NewSchedulerWithReview(r, cfg, reg, fg, store, prompt.NewRenderer(),
		WithSchedulerOutput(&strings.Builder{}))
	if err != nil {
		t.Fatalf("new scheduler with review: %v", err)
	}
	return s
}

// readRoundFiles returns the sorted basenames of the round-N.md files persisted
// for a subtask's reviews, so a test can assert how many rounds were recorded.
func readRoundFiles(t *testing.T, store *run.Store, runID, subtaskID string) []string {
	t.Helper()
	layout := run.Layout{RunsDir: store.RunsDir(), ID: runID, DocsSubdir: "docs"}
	entries, err := os.ReadDir(layout.SubtaskReviewsDir(subtaskID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading reviews dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// ---------------------------------------------------------------------------
// Tests.
// ---------------------------------------------------------------------------

// TestReviewApprovedOnFirstReview proves the happy path: the executor runs once,
// the reviewer approves on the first review, the subtask reaches done, Loops stays
// 0, and exactly ONE round file (round-1.md) is persisted.
func TestReviewApprovedOnFirstReview(t *testing.T) {
	cfg := reviewCfg()
	subs := []run.Subtask{pending("st-01", nil, "a/x.go")}
	root := t.TempDir()
	store, r := newSchedRun(t, cfg, subs)

	exec := newWritingHarness("executor")
	exec.filesByCall = []map[string]string{{"a/x.go": "package a\n"}}
	rev := harness.NewMock("reviewer")
	rev.PushText(approvedYAML())

	s := newReviewLoopScheduler(t, cfg, store, r, newFakeGit(root), exec, rev)
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("scheduler: %v", err)
	}

	if st, _ := r.SubtaskByID("st-01"); st.Status != run.SubtaskDone {
		t.Errorf("st-01 = %q; want done", st.Status)
	}
	if st, _ := r.SubtaskByID("st-01"); st.Loops != 0 {
		t.Errorf("st-01 Loops = %d; want 0 (approved on the free first review)", st.Loops)
	}
	if exec.callCount() != 1 {
		t.Errorf("executor invoked %d times; want 1 (no remediation)", exec.callCount())
	}
	if rev.CallCount() != 1 {
		t.Errorf("reviewer invoked %d times; want 1", rev.CallCount())
	}
	if got := readRoundFiles(t, store, r.ID, "st-01"); len(got) != 1 || got[0] != "round-1.md" {
		t.Errorf("round files = %v; want [round-1.md]", got)
	}
}

// TestReviewFindingsTriggerRemediation proves a rejection drives an executor
// remediation pass carrying the findings, then a re-review approves: Loops
// increments to 1 and persists, the executor is invoked twice with the SECOND
// prompt carrying the reviewer's findings, and two round files exist.
func TestReviewFindingsTriggerRemediation(t *testing.T) {
	cfg := reviewCfg()
	subs := []run.Subtask{pending("st-01", nil, "a/x.go")}
	root := t.TempDir()
	store, r := newSchedRun(t, cfg, subs)

	exec := newWritingHarness("executor")
	exec.filesByCall = []map[string]string{
		{"a/x.go": "package a // v1\n"},
		{"a/x.go": "package a // v2 fixed\n"},
	}
	rev := harness.NewMock("reviewer")
	rev.PushText(findingsYAML("fix the thing"))
	rev.PushText(approvedYAML())

	s := newReviewLoopScheduler(t, cfg, store, r, newFakeGit(root), exec, rev)
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("scheduler: %v", err)
	}

	if st, _ := r.SubtaskByID("st-01"); st.Status != run.SubtaskDone {
		t.Errorf("st-01 = %q; want done", st.Status)
	}
	if st, _ := r.SubtaskByID("st-01"); st.Loops != 1 {
		t.Errorf("st-01 Loops = %d; want 1 (one remediation cycle)", st.Loops)
	}
	if exec.callCount() != 2 {
		t.Fatalf("executor invoked %d times; want 2 (initial + remediation)", exec.callCount())
	}
	// The remediation (second) executor prompt must carry the reviewer's finding.
	reqs := exec.mock.Requests()
	if strings.Contains(reqs[0].Prompt, "fix the thing") {
		t.Errorf("the INITIAL executor prompt should not carry findings:\n%s", reqs[0].Prompt)
	}
	if !strings.Contains(reqs[1].Prompt, "fix the thing") {
		t.Errorf("the remediation executor prompt must carry the reviewer finding:\n%s", reqs[1].Prompt)
	}
	// Loops persisted to run.yaml.
	reloaded, err := store.Load(r.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if st, _ := reloaded.SubtaskByID("st-01"); st.Loops != 1 {
		t.Errorf("persisted st-01 Loops = %d; want 1", st.Loops)
	}
	if got := readRoundFiles(t, store, r.ID, "st-01"); len(got) != 2 {
		t.Errorf("round files = %v; want 2 (round-1.md, round-2.md)", got)
	}
}

// TestReviewStopsAtMaxLoops proves the cap: a reviewer that NEVER approves makes
// the loop spend exactly maxLoops remediation cycles, then mark the subtask
// done-but-flagged with the open findings carried forward onto Subtask.Unresolved
// (the proceed-flagged default). Round files exist for every review.
func TestReviewStopsAtMaxLoops(t *testing.T) {
	cfg := reviewCfg()
	cfg.Pipeline.SubtaskReview.MaxLoops = 2
	subs := []run.Subtask{pending("st-01", nil, "a/x.go")}
	root := t.TempDir()
	store, r := newSchedRun(t, cfg, subs)

	exec := newWritingHarness("executor")
	exec.mock.DefaultResult = harness.Result{Text: "ok"}
	exec.filesByCall = []map[string]string{{"a/x.go": "package a\n"}}
	rev := harness.NewMock("reviewer")
	// Never approves: default result is also a rejection so we cannot run short.
	rev.DefaultResult = harness.Result{Text: findingsYAML("still broken")}

	s := newReviewLoopScheduler(t, cfg, store, r, newFakeGit(root), exec, rev)
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("scheduler should complete (proceed-flagged), got: %v", err)
	}

	st, _ := r.SubtaskByID("st-01")
	if st.Status != run.SubtaskDone {
		t.Errorf("st-01 = %q; want done (proceed-flagged at the cap)", st.Status)
	}
	if st.Loops != 2 {
		t.Errorf("st-01 Loops = %d; want 2 (maxLoops remediation cycles)", st.Loops)
	}
	if len(st.Unresolved) == 0 {
		t.Fatalf("st-01 Unresolved is empty; want the open findings carried forward")
	}
	if st.Unresolved[0].Message != "still broken" || st.Unresolved[0].Severity != "blocker" {
		t.Errorf("carried finding mismatch: %+v", st.Unresolved[0])
	}
	// Reviews: free first review + 2 remediation re-reviews = 3 rounds.
	if got := readRoundFiles(t, store, r.ID, "st-01"); len(got) != 3 {
		t.Errorf("round files = %v; want 3", got)
	}
	// Executor: initial + 2 remediations = 3.
	if exec.callCount() != 3 {
		t.Errorf("executor invoked %d times; want 3", exec.callCount())
	}
	// Carry-forward persisted to run.yaml and human-readable.
	reloaded, err := store.Load(r.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if rst, _ := reloaded.SubtaskByID("st-01"); len(rst.Unresolved) == 0 {
		t.Errorf("persisted st-01 Unresolved is empty; want it carried forward")
	}
	raw, err := os.ReadFile(filepath.Join(store.RunsDir(), r.ID, "run.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "unresolved:") || !strings.Contains(string(raw), "still broken") {
		t.Errorf("run.yaml missing human-readable carried findings:\n%s", raw)
	}
}

// TestReviewUnlimitedLoopsUntilApproval proves maxLoops: -1 keeps looping until
// the reviewer approves (here on the third review = after two remediations).
func TestReviewUnlimitedLoopsUntilApproval(t *testing.T) {
	cfg := reviewCfg()
	cfg.Pipeline.SubtaskReview.MaxLoops = -1
	subs := []run.Subtask{pending("st-01", nil, "a/x.go")}
	root := t.TempDir()
	store, r := newSchedRun(t, cfg, subs)

	exec := newWritingHarness("executor")
	exec.mock.DefaultResult = harness.Result{Text: "ok"}
	exec.filesByCall = []map[string]string{{"a/x.go": "package a\n"}}
	rev := harness.NewMock("reviewer")
	rev.PushText(findingsYAML("round1"))
	rev.PushText(findingsYAML("round2"))
	rev.PushText(approvedYAML()) // approve on the 3rd review (K=3).

	s := newReviewLoopScheduler(t, cfg, store, r, newFakeGit(root), exec, rev)
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("scheduler: %v", err)
	}

	st, _ := r.SubtaskByID("st-01")
	if st.Status != run.SubtaskDone {
		t.Errorf("st-01 = %q; want done", st.Status)
	}
	if st.Loops != 2 {
		t.Errorf("st-01 Loops = %d; want 2 (two remediations before approval)", st.Loops)
	}
	if len(st.Unresolved) != 0 {
		t.Errorf("st-01 Unresolved = %+v; want empty (it converged)", st.Unresolved)
	}
	if rev.CallCount() != 3 {
		t.Errorf("reviewer invoked %d times; want 3", rev.CallCount())
	}
}

// TestReviewDisabledSkipsReview proves subtaskReview.enabled: false skips the
// reviewer entirely and marks the subtask done directly (the executor still runs
// once; the reviewer is never invoked).
func TestReviewDisabledSkipsReview(t *testing.T) {
	cfg := reviewCfg()
	cfg.Pipeline.SubtaskReview.Enabled = false
	subs := []run.Subtask{pending("st-01", nil, "a/x.go")}
	root := t.TempDir()
	store, r := newSchedRun(t, cfg, subs)

	exec := newWritingHarness("executor")
	exec.filesByCall = []map[string]string{{"a/x.go": "package a\n"}}
	rev := harness.NewMock("reviewer") // should never be called.

	s := newReviewLoopScheduler(t, cfg, store, r, newFakeGit(root), exec, rev)
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("scheduler: %v", err)
	}

	if st, _ := r.SubtaskByID("st-01"); st.Status != run.SubtaskDone {
		t.Errorf("st-01 = %q; want done", st.Status)
	}
	if rev.CallCount() != 0 {
		t.Errorf("reviewer invoked %d times; want 0 (review disabled)", rev.CallCount())
	}
	if got := readRoundFiles(t, store, r.ID, "st-01"); len(got) != 0 {
		t.Errorf("round files = %v; want none (review disabled)", got)
	}
}

// TestReviewResumeReentersAtRound proves resume re-enters the loop at the
// persisted round rather than restarting from 0: a subtask pre-seeded with
// Loops=1 and an interrupted reviewing status (rewound to pending by the
// scheduler) is re-executed once and re-reviewed; on approval its Loops stays 1
// (the persisted remediation count is preserved) and the next round file is
// round-2.md, not round-1.md.
func TestReviewResumeReentersAtRound(t *testing.T) {
	cfg := reviewCfg()
	subs := []run.Subtask{func() run.Subtask {
		s := pending("st-01", nil, "a/x.go")
		s.Status = run.SubtaskReviewing // interrupted mid-review
		s.Loops = 1                     // one remediation already happened pre-interruption
		return s
	}()}
	root := t.TempDir()
	store, r := newSchedRun(t, cfg, subs)

	// Pre-seed a round-1.md so the assertion that the NEW round is round-2.md is
	// meaningful (resume must not clobber round 1).
	layout := run.Layout{RunsDir: store.RunsDir(), ID: r.ID, DocsSubdir: "docs"}
	if err := os.MkdirAll(layout.SubtaskReviewsDir("st-01"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.SubtaskReviewRoundFile("st-01", 1), []byte("# prior round 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := newWritingHarness("executor")
	exec.filesByCall = []map[string]string{{"a/x.go": "package a\n"}}
	rev := harness.NewMock("reviewer")
	rev.PushText(approvedYAML()) // approve on resume's re-review.

	s := newReviewLoopScheduler(t, cfg, store, r, newFakeGit(root), exec, rev)
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("scheduler: %v", err)
	}

	st, _ := r.SubtaskByID("st-01")
	if st.Status != run.SubtaskDone {
		t.Errorf("st-01 = %q; want done", st.Status)
	}
	if st.Loops != 1 {
		t.Errorf("st-01 Loops = %d; want 1 preserved across resume (not reset, not double-counted)", st.Loops)
	}
	// The re-review is round Loops+1 = 2, so round-2.md must now exist alongside
	// the preserved round-1.md.
	got := readRoundFiles(t, store, r.ID, "st-01")
	hasRound2 := false
	for _, n := range got {
		if n == "round-2.md" {
			hasRound2 = true
		}
	}
	if !hasRound2 {
		t.Errorf("round files = %v; want a round-2.md (resume re-entered at the persisted round)", got)
	}
}

// TestReviewMalformedThenWellformed proves the one-lenient-re-ask policy: the
// reviewer's first response is unparseable, the loop re-asks once, the second
// response is a valid approval, and the subtask reaches done. The reviewer is
// invoked twice for that single review round.
func TestReviewMalformedThenWellformed(t *testing.T) {
	cfg := reviewCfg()
	subs := []run.Subtask{pending("st-01", nil, "a/x.go")}
	root := t.TempDir()
	store, r := newSchedRun(t, cfg, subs)

	exec := newWritingHarness("executor")
	exec.filesByCall = []map[string]string{{"a/x.go": "package a\n"}}
	rev := harness.NewMock("reviewer")
	rev.PushText("I have no verdict block, oops.") // malformed
	rev.PushText(approvedYAML())                   // lenient re-ask succeeds

	s := newReviewLoopScheduler(t, cfg, store, r, newFakeGit(root), exec, rev)
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("scheduler: %v", err)
	}

	if st, _ := r.SubtaskByID("st-01"); st.Status != run.SubtaskDone {
		t.Errorf("st-01 = %q; want done (lenient re-ask recovered)", st.Status)
	}
	if rev.CallCount() != 2 {
		t.Errorf("reviewer invoked %d times; want 2 (initial malformed + one re-ask)", rev.CallCount())
	}
	if exec.callCount() != 1 {
		t.Errorf("executor invoked %d times; want 1 (no remediation; the re-ask approved)", exec.callCount())
	}
}

// TestReviewMalformedTwiceFailsSubtask proves that a reviewer that is unparseable
// even after the single lenient re-ask is a HARD error: the subtask is recorded
// failed and the scheduler returns an error naming the subtask.
func TestReviewMalformedTwiceFailsSubtask(t *testing.T) {
	cfg := reviewCfg()
	subs := []run.Subtask{pending("st-01", nil, "a/x.go")}
	root := t.TempDir()
	store, r := newSchedRun(t, cfg, subs)

	exec := newWritingHarness("executor")
	exec.filesByCall = []map[string]string{{"a/x.go": "package a\n"}}
	rev := harness.NewMock("reviewer")
	rev.DefaultResult = harness.Result{Text: "still no verdict block"} // malformed on every call

	s := newReviewLoopScheduler(t, cfg, store, r, newFakeGit(root), exec, rev)
	err := s.Run(context.Background())
	if err == nil {
		t.Fatal("scheduler should fail when the reviewer is unparseable twice")
	}
	if !strings.Contains(err.Error(), "st-01") {
		t.Errorf("error should name the failed subtask st-01; got: %v", err)
	}
	if st, _ := r.SubtaskByID("st-01"); st.Status != run.SubtaskFailed {
		t.Errorf("st-01 = %q; want failed", st.Status)
	}
	if rev.CallCount() != 2 {
		t.Errorf("reviewer invoked %d times; want 2 (initial + one re-ask, then give up)", rev.CallCount())
	}
	// The malformed raw output is still persisted for inspection (round-1.md).
	if got := readRoundFiles(t, store, r.ID, "st-01"); len(got) != 1 || got[0] != "round-1.md" {
		t.Errorf("round files = %v; want [round-1.md] (the failed round is kept)", got)
	}
}

// TestSchedulerCompletesWithFlaggedSubtask is the scheduler-integration test: the
// REAL review hook is installed (via NewSchedulerWithReview), a never-approve
// reviewer drives a parallel batch of subtasks to the cap, and the SCHEDULER still
// completes (proceed-flagged) with every subtask done and flagged. Run under -race
// to prove the hook mutates only via the commit seam.
func TestSchedulerCompletesWithFlaggedSubtask(t *testing.T) {
	cfg := reviewCfg()
	cfg.Pipeline.Execution.Parallel = true
	cfg.Pipeline.Execution.MaxParallel = 4
	cfg.Pipeline.SubtaskReview.MaxLoops = 1

	const n = 4
	var subs []run.Subtask
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("st-%02d", i)
		subs = append(subs, pending(id, nil, fmt.Sprintf("pkg/p%02d/file.go", i)))
	}
	root := t.TempDir()
	store, r := newSchedRun(t, cfg, subs)

	exec := newWritingHarness("executor")
	exec.mock.DefaultResult = harness.Result{Text: "ok"}
	// Each executor call writes that subtask's file (derive from WorkDir-relative
	// path is awkward; just write a fixed set keyed by call index is fine since the
	// content only needs to differ from before to produce a diff).
	for i := 0; i < n*2; i++ {
		exec.filesByCall = append(exec.filesByCall, map[string]string{
			fmt.Sprintf("pkg/p%02d/file.go", (i%n)+1): fmt.Sprintf("package p // %d\n", i),
		})
	}
	rev := harness.NewMock("reviewer")
	rev.DefaultResult = harness.Result{Text: findingsYAML("never happy")}

	s := newReviewLoopScheduler(t, cfg, store, r, newFakeGit(root), exec, rev)
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("scheduler should complete proceed-flagged, got: %v", err)
	}

	reloaded, err := store.Load(r.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	for _, st := range reloaded.Subtasks {
		if st.Status != run.SubtaskDone {
			t.Errorf("subtask %s = %q; want done (proceed-flagged)", st.ID, st.Status)
		}
		if len(st.Unresolved) == 0 {
			t.Errorf("subtask %s has no carried findings; want flagged", st.ID)
		}
	}
}

// TestNewSchedulerWithReviewUnknownReviewer proves NewSchedulerWithReview fails
// clearly when the subtaskReviewer role references a harness that is not
// registered.
func TestNewSchedulerWithReviewUnknownReviewer(t *testing.T) {
	cfg := reviewCfg()
	cfg.Roles.SubtaskReviewer.Harness = "nope"
	subs := []run.Subtask{pending("st-01", nil, "a/x.go")}
	store, r := newSchedRun(t, cfg, subs)

	// Registry with only the executor harness wired.
	reg, err := harness.NewRegistry(cfg, harness.Options{
		Factories: map[string]harness.Factory{
			cfg.Roles.Executor.Harness: func(string, config.Harness) (harness.Harness, error) {
				return newWritingHarness("executor"), nil
			},
		},
	})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	_, err = NewSchedulerWithReview(r, cfg, reg, newFakeGit(t.TempDir()), store, prompt.NewRenderer())
	if err == nil {
		t.Fatal("NewSchedulerWithReview should fail for an unknown reviewer harness")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error should name the missing reviewer harness; got: %v", err)
	}
}
