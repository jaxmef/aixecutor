package pipeline

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jaxmef/aixecutor/internal/config"
	"github.com/jaxmef/aixecutor/internal/harness"
	"github.com/jaxmef/aixecutor/internal/prompt"
	"github.com/jaxmef/aixecutor/internal/run"
)

// fakeBaseliner is a hermetic Baseliner: it records a Baseline value pointing at
// dstDir and never touches git, so planning tests seed runs without a real repo.
type fakeBaseliner struct{}

func (fakeBaseliner) CaptureBaseline(dstDir string) (run.Baseline, error) {
	return run.Baseline{Dir: dstDir}, nil
}

// fakeSummarizer returns a canned repo summary, so the planner under test never
// shells out to git for the summary.
type fakeSummarizer struct{ summary string }

func (f fakeSummarizer) Summary(context.Context) (string, error) { return f.summary, nil }

// goodBundle is a valid @@AIXECUTOR_DOC bundle with all four docs and a small
// valid subtasks DAG (st-02 depends on st-01). The docs deliberately contain a
// fenced code block and an "=====" line to prove the marker is robust against
// content that would break a naive delimiter.
const goodBundle = `Here is the plan you asked for.

@@AIXECUTOR_DOC:plan.md@@
# Plan

We will add the feature in two steps.

` + "```go" + `
func Example() {} // fenced code must not confuse the parser
` + "```" + `

=====
@@AIXECUTOR_DOC:context.md@@
# Context

Relevant package: internal/example.
@@AIXECUTOR_DOC:manual-testing.md@@
# Manual testing

Run the binary and check the output.
@@AIXECUTOR_DOC:subtasks.yaml@@
subtasks:
  - id: st-01
    title: "Add the type"
    description: "Define the new type."
    deps: []
    files: ["internal/example/type.go"]
    acceptance:
      - "The type compiles."
  - id: st-02
    title: "Wire it"
    description: "Use the new type."
    deps: ["st-01"]
    files: ["internal/example/use.go"]
    acceptance:
      - "It is used."
`

// newTestStore builds a hermetic run.Store (fake baseliner, fixed clock) rooted in
// a temp dir, and returns the store. The fixed clock makes the run id stable.
func newTestStore(t *testing.T) *run.Store {
	t.Helper()
	cfg := config.Default()
	at := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	store, err := run.NewStoreFromConfig(cfg, t.TempDir(),
		run.WithBaseliner(fakeBaseliner{}),
		run.WithClock(run.ClockFunc(func() time.Time { return at })),
	)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return store
}

// plannerRole is the default planner role config, used to drive the planner under
// test (template name, model, permission mode, timeout).
func plannerRole() config.Role { return config.Default().Roles.Planner }

// newTestPlanner wires a Planner with the given harness and a canned summary,
// writing its human output to out. It returns the planner and the created run.
func newTestPlanner(t *testing.T, store *run.Store, h harness.Harness, out *bytes.Buffer, opts ...PlannerOption) (*Planner, *run.Run) {
	t.Helper()
	r, err := store.Create("add a feature", config.Default())
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	base := []PlannerOption{WithOutput(out)}
	p := NewPlanner(h, prompt.NewRenderer(), store, fakeSummarizer{summary: "internal/example/*.go"}, plannerRole(), t.TempDir(), append(base, opts...)...)
	return p, r
}

// TestPlanWritesDocsAndPersistsSubtasks is the happy path: a mock harness returns
// a valid bundle; the four docs are written under <run>/docs/, the subtasks are
// parsed and persisted to run.yaml as pending (proven by a fresh Store.Load), the
// run reaches planned, and the docs path is printed.
func TestPlanWritesDocsAndPersistsSubtasks(t *testing.T) {
	store := newTestStore(t)
	h := harness.NewMock("planner").PushText(goodBundle)
	var out bytes.Buffer
	p, r := newTestPlanner(t, store, h, &out)

	if err := p.Plan(context.Background(), r); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// All four docs written under <run>/docs/ with the expected content.
	docsDir := store.DocsDir(r.ID)
	wantContent := map[string]string{
		"plan.md":           "# Plan",
		"context.md":        "Relevant package: internal/example.",
		"manual-testing.md": "Run the binary and check the output.",
		"subtasks.yaml":     "id: st-01",
	}
	for name, want := range wantContent {
		got, err := os.ReadFile(filepath.Join(docsDir, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if !strings.Contains(string(got), want) {
			t.Errorf("%s does not contain %q; got:\n%s", name, want, got)
		}
	}
	// The fenced code block content survived into plan.md (marker robustness).
	planBytes, _ := os.ReadFile(filepath.Join(docsDir, "plan.md"))
	if !strings.Contains(string(planBytes), "fenced code must not confuse the parser") {
		t.Errorf("plan.md lost its fenced code block:\n%s", planBytes)
	}

	// In-memory run reached planned with the two pending subtasks.
	if r.Status != run.StatusPlanned {
		t.Errorf("run status = %q, want planned", r.Status)
	}
	if len(r.Subtasks) != 2 {
		t.Fatalf("got %d subtasks, want 2", len(r.Subtasks))
	}

	// Persistence: reload from disk and confirm the subtasks are there, pending,
	// with deps preserved.
	reloaded, err := store.Load(r.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Status != run.StatusPlanned {
		t.Errorf("reloaded status = %q, want planned", reloaded.Status)
	}
	if len(reloaded.Subtasks) != 2 {
		t.Fatalf("reloaded %d subtasks, want 2", len(reloaded.Subtasks))
	}
	for _, s := range reloaded.Subtasks {
		if s.Status != run.SubtaskPending {
			t.Errorf("reloaded subtask %q status = %q, want pending", s.ID, s.Status)
		}
	}
	if got := reloaded.Subtasks[1]; len(got.Deps) != 1 || got.Deps[0] != "st-01" {
		t.Errorf("reloaded st-02 deps = %v, want [st-01]", got.Deps)
	}

	// The docs path was printed prominently.
	if !strings.Contains(out.String(), docsDir) {
		t.Errorf("docs path %q not printed; output:\n%s", docsDir, out.String())
	}
	if !strings.Contains(out.String(), "Planning complete") {
		t.Errorf("planning-complete notice missing; output:\n%s", out.String())
	}

	// The planner harness was invoked exactly once, in plan mode, at the repo root.
	reqs := h.Requests()
	if len(reqs) != 1 {
		t.Fatalf("planner invoked %d times, want 1", len(reqs))
	}
	if reqs[0].PermissionMode != plannerRole().PermissionMode {
		t.Errorf("planner permission mode = %q, want %q", reqs[0].PermissionMode, plannerRole().PermissionMode)
	}
	if reqs[0].WorkDir == "" {
		t.Error("planner WorkDir was empty (should be the repo root)")
	}
	if !strings.Contains(reqs[0].Prompt, "internal/example/*.go") {
		t.Errorf("planner prompt did not include the repo summary; got:\n%s", reqs[0].Prompt)
	}
}

// TestPlanMissingDocKeepsRawAndErrors covers a bundle missing a required document:
// the planner returns a clear error AND keeps the raw response under
// <run>/docs/planner-raw.txt for inspection. The single retry also fails (the mock
// returns the same bad bundle for the second attempt), so the run stays planning.
func TestPlanMissingDocKeepsRawAndErrors(t *testing.T) {
	// A bundle missing manual-testing.md and subtasks.yaml.
	const badBundle = "@@AIXECUTOR_DOC:plan.md@@\n# Plan\n@@AIXECUTOR_DOC:context.md@@\n# Context\n"

	store := newTestStore(t)
	h := harness.NewMock("planner").PushText(badBundle).PushText(badBundle)
	var out bytes.Buffer
	p, r := newTestPlanner(t, store, h, &out)

	err := p.Plan(context.Background(), r)
	if err == nil {
		t.Fatal("Plan with a missing-doc bundle should error")
	}
	for _, want := range []string{"did not validate", "planner-raw.txt"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}

	// Raw response kept for inspection.
	raw, rerr := os.ReadFile(filepath.Join(store.DocsDir(r.ID), rawResponseFileName))
	if rerr != nil {
		t.Fatalf("raw response not kept: %v", rerr)
	}
	if !strings.Contains(string(raw), "# Plan") {
		t.Errorf("raw response file does not contain the agent output:\n%s", raw)
	}

	// Two attempts were made (the single re-prompt).
	if h.CallCount() != 2 {
		t.Errorf("planner invoked %d times, want 2 (one retry)", h.CallCount())
	}
	// The run was not marked planned.
	if r.Status == run.StatusPlanned {
		t.Error("run should not be planned after a validation failure")
	}
}

// TestPlanMalformedSubtasksErrors covers a complete bundle whose subtasks.yaml is
// present but invalid (a cycle): planning fails with a clear error after the retry.
func TestPlanMalformedSubtasksErrors(t *testing.T) {
	badSubtasks := strings.Replace(goodBundle,
		"  - id: st-01\n    title: \"Add the type\"\n    description: \"Define the new type.\"\n    deps: []",
		"  - id: st-01\n    title: \"Add the type\"\n    description: \"Define the new type.\"\n    deps: [\"st-02\"]",
		1)
	if badSubtasks == goodBundle {
		t.Fatal("test setup: failed to inject a cycle into the bundle")
	}

	store := newTestStore(t)
	h := harness.NewMock("planner").PushText(badSubtasks).PushText(badSubtasks)
	var out bytes.Buffer
	p, r := newTestPlanner(t, store, h, &out)

	err := p.Plan(context.Background(), r)
	if err == nil {
		t.Fatal("Plan with a cyclic subtasks.yaml should error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "cycle") {
		t.Errorf("error should mention the cycle; got: %v", err)
	}
}

// TestPlanRetrySucceedsOnSecondAttempt proves the single re-prompt path: the mock
// returns a bad bundle first and a good one second, so planning succeeds, the
// prior error is fed into the retry prompt, and exactly two invocations occur.
func TestPlanRetrySucceedsOnSecondAttempt(t *testing.T) {
	const badBundle = "@@AIXECUTOR_DOC:plan.md@@\n# Plan only, missing the rest\n"

	store := newTestStore(t)
	h := harness.NewMock("planner").PushText(badBundle).PushText(goodBundle)
	var out bytes.Buffer
	p, r := newTestPlanner(t, store, h, &out)

	if err := p.Plan(context.Background(), r); err != nil {
		t.Fatalf("Plan should succeed on the second attempt: %v", err)
	}
	if r.Status != run.StatusPlanned {
		t.Errorf("run status = %q, want planned", r.Status)
	}
	if len(r.Subtasks) != 2 {
		t.Errorf("got %d subtasks, want 2", len(r.Subtasks))
	}

	reqs := h.Requests()
	if len(reqs) != 2 {
		t.Fatalf("planner invoked %d times, want 2", len(reqs))
	}
	// The first prompt carries no prior-error section; the second one does.
	if strings.Contains(reqs[0].Prompt, "previous attempt was rejected") {
		t.Errorf("first prompt unexpectedly contained the prior-error section")
	}
	if !strings.Contains(reqs[1].Prompt, "previous attempt was rejected") {
		t.Errorf("retry prompt did not feed back the validation error; got:\n%s", reqs[1].Prompt)
	}
	// The retry prompt names the missing-doc failure so the agent can fix it.
	if !strings.Contains(reqs[1].Prompt, "manual-testing.md") {
		t.Errorf("retry prompt did not surface the missing-doc detail; got:\n%s", reqs[1].Prompt)
	}
}

// TestPlanHarnessErrorIsNotRetried proves a transport/harness error is surfaced
// directly (not treated as a validation failure to retry): the mock errors on the
// first call and planning fails after a single invocation.
func TestPlanHarnessErrorIsNotRetried(t *testing.T) {
	store := newTestStore(t)
	h := harness.NewMock("planner").PushError(harness.Result{}, context.DeadlineExceeded)
	var out bytes.Buffer
	p, r := newTestPlanner(t, store, h, &out)

	err := p.Plan(context.Background(), r)
	if err == nil {
		t.Fatal("Plan should fail when the harness errors")
	}
	if !strings.Contains(err.Error(), "planner invocation failed") {
		t.Errorf("error should identify an invocation failure; got: %v", err)
	}
	if h.CallCount() != 1 {
		t.Errorf("harness invoked %d times, want 1 (no retry on transport error)", h.CallCount())
	}
}

// TestPlanDryRunWritesPlaceholders covers the --dry-run path: with the dry-run
// option set, planning writes clearly-marked placeholder docs (including a valid
// subtasks.yaml), persists a placeholder pending subtask, reaches planned, and
// prints a "[dry-run]" notice — without depending on the harness output being a
// real bundle.
func TestPlanDryRunWritesPlaceholders(t *testing.T) {
	store := newTestStore(t)
	// A dry-run harness would return a non-bundle placeholder; emulate that.
	h := harness.NewMock("planner").PushText("[dry-run] not a real bundle")
	var out bytes.Buffer
	p, r := newTestPlanner(t, store, h, &out, WithDryRun(true))

	if err := p.Plan(context.Background(), r); err != nil {
		t.Fatalf("dry-run Plan: %v", err)
	}
	if r.Status != run.StatusPlanned {
		t.Errorf("run status = %q, want planned", r.Status)
	}
	if len(r.Subtasks) != 1 {
		t.Fatalf("dry-run should persist 1 placeholder subtask, got %d", len(r.Subtasks))
	}
	if r.Subtasks[0].Status != run.SubtaskPending {
		t.Errorf("placeholder subtask status = %q, want pending", r.Subtasks[0].Status)
	}

	// Placeholder docs are present and clearly marked.
	plan, err := os.ReadFile(filepath.Join(store.DocsDir(r.ID), "plan.md"))
	if err != nil {
		t.Fatalf("reading placeholder plan.md: %v", err)
	}
	if !strings.Contains(string(plan), "[dry-run]") {
		t.Errorf("placeholder plan.md not marked dry-run:\n%s", plan)
	}
	// The notice clearly indicates dry-run.
	if !strings.Contains(out.String(), "[dry-run]") {
		t.Errorf("dry-run notice missing; output:\n%s", out.String())
	}

	// Reload proves the placeholder subtask persisted.
	reloaded, err := store.Load(r.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded.Subtasks) != 1 {
		t.Errorf("reloaded %d subtasks, want 1", len(reloaded.Subtasks))
	}
}
