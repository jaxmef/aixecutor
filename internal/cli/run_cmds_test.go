package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jaxmef/aixecutor/internal/config"
	"github.com/jaxmef/aixecutor/internal/run"
)

// fakeBaseliner is a hermetic Baseliner for CLI tests: it never touches git, just
// records a Baseline value. (The run package has its own; CLI tests need a local
// one to seed runs without a real repo.)
type fakeBaseliner struct{}

func (fakeBaseliner) CaptureBaseline(dstDir string) (run.Baseline, error) {
	return run.Baseline{Dir: dstDir}, nil
}

// fixedClock returns a constant time so seeded run ids are deterministic.
type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// seedRun creates a run in the runs dir the CLI will resolve from the current
// working directory, using a fixed clock so the id is known. It returns the run.
// The store is constructed exactly as openStore would resolve it (config default
// runsDir under repoRoot()), so the CLI command under test reads the same dir.
func seedRun(t *testing.T, task string, at time.Time, mutate func(*run.Run)) *run.Run {
	t.Helper()
	cfg := config.Default()
	store, err := run.NewStoreFromConfig(cfg, repoRoot(),
		run.WithBaseliner(fakeBaseliner{}),
		run.WithClock(fixedClock{t: at}),
	)
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	r, err := store.Create(task, cfg)
	if err != nil {
		t.Fatalf("seed Create: %v", err)
	}
	if mutate != nil {
		mutate(r)
		if err := store.Save(r); err != nil {
			t.Fatalf("seed Save: %v", err)
		}
	}
	return r
}

// missingConfigArgs returns CLI args that point both config layers at a
// non-existent file, so config.Load uses only the built-in defaults and never
// reads a real ~/.aixecutor or repo config.
func missingConfigArgs(t *testing.T, rest ...string) []string {
	t.Helper()
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	return append([]string{"--config", missing, "--global-config", missing}, rest...)
}

func TestListNoRuns(t *testing.T) {
	t.Chdir(t.TempDir()) // isolated, non-git working dir → runsDir under it, empty.

	out, err := runCLI(t, missingConfigArgs(t, "list")...)
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	if !strings.Contains(out, "No runs found") {
		t.Errorf("list with no runs should say so:\n%s", out)
	}
}

func TestListShowsRunsNewestFirst(t *testing.T) {
	t.Chdir(t.TempDir())

	base := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	older := seedRun(t, "older task", base, nil)
	newer := seedRun(t, "newer task", base.Add(2*time.Hour), nil)

	out, err := runCLI(t, missingConfigArgs(t, "list")...)
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	for _, want := range []string{"RUN ID", "STATUS", "TASK", older.ID, newer.ID, "created"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
	// Newest first: newer's id appears before older's in the output.
	if strings.Index(out, newer.ID) > strings.Index(out, older.ID) {
		t.Errorf("list not newest-first: %q should precede %q\n%s", newer.ID, older.ID, out)
	}
}

func TestStatusShowsSubtasksAndSeniorReview(t *testing.T) {
	t.Chdir(t.TempDir())

	at := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	r := seedRun(t, "implement feature", at, func(r *run.Run) {
		r.Status = run.StatusExecuting
		r.Subtasks = []run.Subtask{
			{ID: "st-1", Title: "schema", Status: run.SubtaskDone, Loops: 2},
			{ID: "st-2", Title: "store", Status: run.SubtaskImplementing, Loops: 1, Deps: []string{"st-1"}},
		}
		r.SeniorReview = run.SeniorReview{Enabled: true, Status: run.SeniorReviewPending}
	})

	out, err := runCLI(t, missingConfigArgs(t, "status", r.ID)...)
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	for _, want := range []string{
		r.ID,
		"executing",
		"st-1", "done",
		"st-2", "implementing",
		"1/2 done",
		"Senior review: pending",
		"LOOPS",
		// AIX-0014 richer status: elapsed time, docs path, and the unresolved column.
		"Elapsed:",
		"Docs:",
		"UNRESOLVED",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
}

// TestStatusShowsUnresolvedFindingsAndSeniorCap proves the richer status surfaces
// unresolved findings (from a flagged subtask and from a cap-reached senior
// review) and the senior-review clean-vs-cap distinction, plus a non-zero elapsed.
func TestStatusShowsUnresolvedFindingsAndSeniorCap(t *testing.T) {
	t.Chdir(t.TempDir())

	created := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	r := seedRun(t, "finish it", created, func(r *run.Run) {
		r.Status = run.StatusCompleted
		r.Subtasks = []run.Subtask{
			{ID: "st-1", Title: "schema", Status: run.SubtaskDone, Loops: 3, Unresolved: []run.Finding{
				{Severity: "minor", File: "a.go", Line: 10, Message: "leftover TODO"},
			}},
		}
		r.SeniorReview = run.SeniorReview{
			Enabled: true, Status: run.SeniorReviewDone, Rounds: 3,
			Unresolved: []run.Finding{{Severity: "major", Message: "missing error handling"}},
		}
	})

	out, err := runCLI(t, missingConfigArgs(t, "status", r.ID)...)
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	for _, want := range []string{
		"Senior review: done — 1 unresolved finding(s) after 3 round(s) (cap reached)",
		"Unresolved findings (2)",
		"subtask st-1", "leftover TODO",
		"senior review", "missing error handling",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
}

func TestStatusDefaultsToLatest(t *testing.T) {
	t.Chdir(t.TempDir())

	base := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	_ = seedRun(t, "first", base, nil)
	latest := seedRun(t, "second", base.Add(time.Hour), nil)

	// No id argument → latest.
	out, err := runCLI(t, missingConfigArgs(t, "status")...)
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if !strings.Contains(out, latest.ID) {
		t.Errorf("status with no id should show latest run %q:\n%s", latest.ID, out)
	}
}

func TestStatusNoRunsErrors(t *testing.T) {
	t.Chdir(t.TempDir())

	out, err := runCLI(t, missingConfigArgs(t, "status")...)
	if err == nil {
		t.Fatalf("status with no runs should error, got:\n%s", out)
	}
	if !strings.Contains(err.Error(), "no runs") {
		t.Errorf("error %q should mention there are no runs", err.Error())
	}
}

// TestResumeInNonGitDirUsesWorkspaceMode proves the AIX-0020 change: resuming a
// resumable run OUTSIDE a git repository no longer fails — it operates in workspace
// mode over the plain directory and drives the pipeline to completion (here under
// --dry-run so it is hermetic). This replaces the old "resume requires a git repo"
// contract, which AIX-0020 intentionally relaxed.
func TestResumeInNonGitDirUsesWorkspaceMode(t *testing.T) {
	t.Chdir(t.TempDir()) // a NON-git temp dir → workspace mode.

	at := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	r := seedRun(t, "resume me", at, func(r *run.Run) {
		r.Status = run.StatusExecuting
		r.Subtasks = []run.Subtask{
			{ID: "st-1", Title: "done one", Status: run.SubtaskDone},
			{ID: "st-2", Title: "waiting", Status: run.SubtaskPending},
		}
	})

	out, err := runCLI(t, missingConfigArgs(t, "--dry-run", "resume", r.ID)...)
	if err != nil {
		t.Fatalf("resume in a non-git dir should work in workspace mode (AIX-0020): %v\n%s", err, out)
	}
	if !strings.Contains(out, "Nothing was committed") {
		t.Errorf("expected the run to drive to completion in workspace mode:\n%s", out)
	}
}

func TestResumeCompletedRunIsNoop(t *testing.T) {
	t.Chdir(t.TempDir())

	at := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	r := seedRun(t, "all done", at, func(r *run.Run) {
		r.Status = run.StatusCompleted
	})

	// A completed run is short-circuited through the READ-ONLY store, so it works
	// even outside a git repo and never reaches the orchestrator.
	out, err := runCLI(t, missingConfigArgs(t, "resume", r.ID)...)
	if err != nil {
		t.Fatalf("resume: %v\n%s", err, out)
	}
	if !strings.Contains(out, "nothing to resume") {
		t.Errorf("resume of a completed run should say nothing to resume:\n%s", out)
	}
}

func TestResumeFailedRunIsNoop(t *testing.T) {
	t.Chdir(t.TempDir())

	at := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	r := seedRun(t, "it failed", at, func(r *run.Run) {
		r.Status = run.StatusFailed
	})

	out, err := runCLI(t, missingConfigArgs(t, "resume", r.ID)...)
	if err != nil {
		t.Fatalf("resume: %v\n%s", err, out)
	}
	if !strings.Contains(out, "nothing to resume") {
		t.Errorf("resume of a failed run should say nothing to resume:\n%s", out)
	}
}

// TestRunInNonGitDirUsesWorkspaceMode proves the AIX-0020 change: `run` outside a
// git repository no longer fails — it operates in workspace mode over the plain
// directory and drives the full pipeline (here under --dry-run, hermetically). This
// replaces the old "run requires a git repo" contract.
func TestRunInNonGitDirUsesWorkspaceMode(t *testing.T) {
	t.Chdir(t.TempDir())

	out, err := runCLI(t, missingConfigArgs(t, "--dry-run", "run", "add a flag")...)
	if err != nil {
		t.Fatalf("run in a non-git dir should work in workspace mode (AIX-0020): %v\n%s", err, out)
	}
	if !strings.Contains(out, "Nothing was committed") {
		t.Errorf("expected a completed dry-run summary in workspace mode:\n%s", out)
	}
}

// TestRunRequiresTaskArg proves `run` with no positional argument errors cleanly
// (MaximumNArgs(1) + resolveTaskInput): no task source → a "no task" usage error.
func TestRunRequiresTaskArg(t *testing.T) {
	t.Chdir(t.TempDir())

	out, err := runCLI(t, missingConfigArgs(t, "run")...)
	if err == nil {
		t.Fatalf("run with no task should error, got:\n%s", out)
	}
	if !strings.Contains(err.Error(), "no task provided") {
		t.Errorf("error should explain the missing task; got: %v", err)
	}
}

// TestRunTaskFileExclusivity proves the AIX-0017 exclusivity is enforced through
// the real `run` command wiring (not just the helper): passing both a positional
// task and --task-file is a clear usage error, before any git/run work.
func TestRunTaskFileExclusivity(t *testing.T) {
	t.Chdir(t.TempDir())

	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte("a task\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, missingConfigArgs(t, "run", "inline task", "--task-file", spec)...)
	if err == nil {
		t.Fatalf("run with both a task and --task-file should error, got:\n%s", out)
	}
	if !strings.Contains(err.Error(), "not both") {
		t.Errorf("error should explain the exclusivity; got: %v", err)
	}
}
