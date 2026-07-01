package pipeline

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jaxmef/aixecutor/internal/harness"
	"github.com/jaxmef/aixecutor/internal/run"
)

// readExecutionRoundFiles returns the sorted basenames of the execution round-N.md
// files persisted for a subtask, mirroring readRoundFiles for the reviews dir.
func readExecutionRoundFiles(t *testing.T, store *run.Store, runID, subtaskID string) []string {
	t.Helper()
	layout := run.Layout{RunsDir: store.RunsDir(), ID: runID, DocsSubdir: "docs"}
	entries, err := os.ReadDir(layout.SubtaskExecutionsDir(subtaskID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading execution dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func readExecutionRound(t *testing.T, store *run.Store, runID, subtaskID string, round int) string {
	t.Helper()
	layout := run.Layout{RunsDir: store.RunsDir(), ID: runID, DocsSubdir: "docs"}
	data, err := os.ReadFile(layout.SubtaskExecutionRoundFile(subtaskID, round))
	if err != nil {
		t.Fatalf("reading execution round %d: %v", round, err)
	}
	return string(data)
}

// TestChangedFilesFromPatch proves the diff-header parser: it takes the `b/<path>`
// token from each `diff --git` header, strips the `b/`, dedupes preserving order,
// and ignores non-header lines.
func TestChangedFilesFromPatch(t *testing.T) {
	patch := "diff --git a/foo.go b/foo.go\n" +
		"index 000..111 100644\n" +
		"--- a/foo.go\n+++ b/foo.go\n@@ -1 +1 @@\n-old\n+new\n" +
		"diff --git a/pkg/bar.go b/pkg/bar.go\n" +
		"new file mode 100644\n+++ b/pkg/bar.go\n" +
		"diff --git a/foo.go b/foo.go\n" // duplicate header: must dedupe.

	got := changedFilesFromPatch(patch)
	want := []string{"foo.go", "pkg/bar.go"}
	if len(got) != len(want) {
		t.Fatalf("changedFilesFromPatch = %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("changedFilesFromPatch[%d] = %q; want %q (order preserved)", i, got[i], want[i])
		}
	}
	if files := changedFilesFromPatch(""); len(files) != 0 {
		t.Errorf("changedFilesFromPatch(empty) = %v; want none", files)
	}
}

// TestRenderExecutionRound proves the pure renderer emits every required field, the
// files-touched list, a relative diff.patch link, and the executor summary UNFENCED.
func TestRenderExecutionRound(t *testing.T) {
	md := renderExecutionRound(
		"st-01", "Do the thing", 2,
		"claude", "sonnet", "acceptEdits", 30*time.Minute,
		1500*time.Millisecond, 0,
		[]string{"a/x.go", "a/y.go"},
		"Implemented the change.\n",
	)
	for _, want := range []string{
		"# Subtask st-01 — execution round 2",
		"**Title:** Do the thing",
		"claude", "sonnet", "acceptEdits",
		"[diff.patch](../diff.patch)",
		"`a/x.go`", "`a/y.go`",
		"Implemented the change.",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("render missing %q:\n%s", want, md)
		}
	}
	// The summary is the agent's own markdown, so it must NOT be wrapped in a fence
	// (unlike renderReviewRound's raw block).
	if strings.Contains(md, "```") {
		t.Errorf("execution summary must be emitted unfenced; got:\n%s", md)
	}

	// Empty files list falls back to the sentinel line.
	empty := renderExecutionRound("st-02", "", 1, "claude", "sonnet", "acceptEdits",
		time.Minute, time.Second, 0, nil, "nothing to do")
	if !strings.Contains(empty, "_No files changed._") {
		t.Errorf("empty files must render the fallback; got:\n%s", empty)
	}
	if strings.Contains(empty, "**Title:**") {
		t.Errorf("empty title must be omitted; got:\n%s", empty)
	}
}

// TestExecutionSummarySingleRun proves one executor pass writes execution/round-1.md
// carrying the mock's summary text, the round number, the role's harness/model, and
// a link to that round's diff.patch.
func TestExecutionSummarySingleRun(t *testing.T) {
	cfg := reviewCfg()
	subs := []run.Subtask{pending("st-01", nil, "a/x.go")}
	root := t.TempDir()
	store, r := newSchedRun(t, cfg, subs)

	exec := newWritingHarness("executor")
	exec.mock.DefaultResult = harness.Result{Text: "EXEC-SUMMARY-TEXT", ExitCode: 0}
	exec.filesByCall = []map[string]string{{"a/x.go": "package a\n"}}
	rev := harness.NewMock("reviewer")
	rev.PushText(approvedYAML())

	s := newReviewLoopScheduler(t, cfg, store, r, newFakeGit(root), exec, rev)
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("scheduler: %v", err)
	}

	if got := readExecutionRoundFiles(t, store, r.ID, "st-01"); len(got) != 1 || got[0] != "round-1.md" {
		t.Fatalf("execution round files = %v; want [round-1.md]", got)
	}
	md := readExecutionRound(t, store, r.ID, "st-01", 1)
	for _, want := range []string{
		"EXEC-SUMMARY-TEXT",
		"execution round 1",
		cfg.Roles.Executor.Harness,
		cfg.Roles.Executor.Model,
		"[diff.patch](../diff.patch)",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("round-1.md missing %q:\n%s", want, md)
		}
	}
}

// TestExecutionSummaryTwoRounds proves a subtask that takes one remediation pass
// produces execution/round-1.md AND round-2.md, correctly numbered to pair with the
// review rounds.
func TestExecutionSummaryTwoRounds(t *testing.T) {
	cfg := reviewCfg()
	subs := []run.Subtask{pending("st-01", nil, "a/x.go")}
	root := t.TempDir()
	store, r := newSchedRun(t, cfg, subs)

	exec := newWritingHarness("executor")
	exec.mock.DefaultResult = harness.Result{Text: "pass"}
	exec.filesByCall = []map[string]string{
		{"a/x.go": "package a // v1\n"},
		{"a/x.go": "package a // v2\n"},
	}
	rev := harness.NewMock("reviewer")
	rev.PushText(findingsYAML("fix it"))
	rev.PushText(approvedYAML())

	s := newReviewLoopScheduler(t, cfg, store, r, newFakeGit(root), exec, rev)
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("scheduler: %v", err)
	}

	execRounds := readExecutionRoundFiles(t, store, r.ID, "st-01")
	if len(execRounds) != 2 {
		t.Fatalf("execution round files = %v; want 2 (round-1.md, round-2.md)", execRounds)
	}
	// Execution rounds pair 1:1 with review rounds.
	if reviewRounds := readRoundFiles(t, store, r.ID, "st-01"); len(reviewRounds) != len(execRounds) {
		t.Errorf("review rounds %v vs execution rounds %v; want equal count", reviewRounds, execRounds)
	}
	if md := readExecutionRound(t, store, r.ID, "st-01", 2); !strings.Contains(md, "execution round 2") {
		t.Errorf("round-2.md not numbered 2:\n%s", md)
	}
}

// TestExecutionSummaryReRunOverwrites proves re-running runExecutor for the same
// round overwrites execution/round-N.md rather than duplicating or corrupting it, so
// resume stays idempotent.
func TestExecutionSummaryReRunOverwrites(t *testing.T) {
	cfg := reviewCfg()
	subs := []run.Subtask{pending("st-01", nil, "a/x.go")}
	root := t.TempDir()
	store, r := newSchedRun(t, cfg, subs)

	exec := newWritingHarness("executor")
	exec.mock.DefaultResult = harness.Result{Text: "SUMMARY-A"}
	exec.filesByCall = []map[string]string{{"a/x.go": "package a\n"}}
	rev := harness.NewMock("reviewer") // never invoked: we call runExecutor directly.

	s := newReviewLoopScheduler(t, cfg, store, r, newFakeGit(root), exec, rev)

	// runExecutor -> subtaskSnapshot talks to the run-state actor, which Scheduler.Run
	// normally starts. This test drives runExecutor directly, so start the actor here.
	s.actor = newStateActor(s.run, s.store, s.cfg)
	go s.actor.loop()
	defer s.actor.stop()

	ctx := context.Background()
	if _, err := s.runExecutor(ctx, "st-01", nil); err != nil {
		t.Fatalf("first runExecutor: %v", err)
	}
	// Second pass for the SAME round (Loops unchanged) must overwrite, not append.
	exec.mock.DefaultResult = harness.Result{Text: "SUMMARY-B"}
	if _, err := s.runExecutor(ctx, "st-01", nil); err != nil {
		t.Fatalf("second runExecutor: %v", err)
	}

	if got := readExecutionRoundFiles(t, store, r.ID, "st-01"); len(got) != 1 || got[0] != "round-1.md" {
		t.Fatalf("execution round files = %v; want a single [round-1.md] after re-run", got)
	}
	md := readExecutionRound(t, store, r.ID, "st-01", 1)
	if !strings.Contains(md, "SUMMARY-B") {
		t.Errorf("round-1.md should hold the latest summary; got:\n%s", md)
	}
	if strings.Contains(md, "SUMMARY-A") {
		t.Errorf("round-1.md must be overwritten, not appended; still holds stale text:\n%s", md)
	}
}
