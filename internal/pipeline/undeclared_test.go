package pipeline

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jaxmef/aixecutor/internal/config"
	"github.com/jaxmef/aixecutor/internal/harness"
	"github.com/jaxmef/aixecutor/internal/run"
)

// errInjectedManifest is returned by a fakeGit configured to fail Manifest, to drive
// the best-effort detection error path.
var errInjectedManifest = errors.New("injected manifest failure")

// subtaskByID returns the (post-run) subtask with the given id, failing the test if
// it is absent.
func subtaskByID(t *testing.T, r *run.Run, id string) run.Subtask {
	t.Helper()
	for _, st := range r.Subtasks {
		if st.ID == id {
			return st
		}
	}
	t.Fatalf("subtask %q not found in run", id)
	return run.Subtask{}
}

func TestUndeclaredEditWarnsRecordsAndKeepsDiffScope(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.Execution.Parallel = false

	subs := []run.Subtask{pending("st-01", nil, "feature/new.go")}
	root := t.TempDir()
	store, r := newSchedRun(t, cfg, subs)

	h := newWritingHarness("executor")
	h.filesByCall = []map[string]string{{
		"feature/new.go": "package feature\n",
		"rogue/other.go": "package rogue\n", // declared by NO subtask
	}}

	var out strings.Builder
	if err := runScheduler(t, cfg, store, r, newFakeGit(root), h, WithSchedulerOutput(&out)); err != nil {
		t.Fatalf("scheduler: %v", err)
	}

	log := out.String()
	if !strings.Contains(log, "rogue/other.go") || !strings.Contains(log, "outside its declared files") {
		t.Errorf("expected an undeclared-edit warning naming rogue/other.go, got:\n%s", log)
	}

	st := subtaskByID(t, r, "st-01")
	if want := []string{"rogue/other.go"}; !reflect.DeepEqual(st.Undeclared, want) {
		t.Errorf("subtask Undeclared = %v; want %v", st.Undeclared, want)
	}

	layout := run.Layout{RunsDir: store.RunsDir(), ID: r.ID, DocsSubdir: "docs"}
	patch, err := os.ReadFile(layout.SubtaskDiffFile("st-01"))
	if err != nil {
		t.Fatalf("reading diff.patch: %v", err)
	}
	if strings.Contains(string(patch), "rogue/other.go") {
		t.Errorf("diff.patch scope leaked the undeclared file:\n%s", patch)
	}
	if !strings.Contains(string(patch), "feature/new.go") {
		t.Errorf("diff.patch missing the declared file:\n%s", patch)
	}
}

// TestSiblingDeclaredEditNoUndeclaredWarning proves the union-of-all-declared-globs
// subtraction: under non-overlapping parallelism a subtask's edit to its OWN declared
// file must never register as "undeclared" on a concurrently-running sibling that
// happens to observe it in the shared main tree.
func TestSiblingDeclaredEditNoUndeclaredWarning(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.Execution.Parallel = true
	cfg.Pipeline.Execution.MaxParallel = 4
	cfg.Pipeline.Execution.Isolation = isolationNonOverlapping

	subs := []run.Subtask{
		pending("st-01", nil, "a/x.go"),
		pending("st-02", nil, "b/y.go"),
	}
	root := t.TempDir()
	store, r := newSchedRun(t, cfg, subs)

	h := newWritingHarness("executor")
	// Delay so the two subtasks' before/after windows overlap in the shared tree.
	h.mock.PushDelay(40*time.Millisecond, harness.Result{Text: "ok"})
	h.mock.PushDelay(40*time.Millisecond, harness.Result{Text: "ok"})
	h.filesByCall = []map[string]string{
		{"a/x.go": "package a\n"},
		{"b/y.go": "package b\n"},
	}

	var out strings.Builder
	if err := runScheduler(t, cfg, store, r, newFakeGit(root), h, WithSchedulerOutput(&out)); err != nil {
		t.Fatalf("scheduler: %v", err)
	}

	if strings.Contains(out.String(), "outside its declared files") {
		t.Errorf("unexpected undeclared-edit warning for sibling-declared edits:\n%s", out.String())
	}
	for _, id := range []string{"st-01", "st-02"} {
		if st := subtaskByID(t, r, id); len(st.Undeclared) != 0 {
			t.Errorf("subtask %s recorded undeclared edits %v; want none", id, st.Undeclared)
		}
	}
}

// TestExcludedPathsNeverUndeclared proves runsDir, .git, and editor-dir paths (which
// the manifest excludes by construction) never trigger a warning even when the
// executor writes into them.
func TestExcludedPathsNeverUndeclared(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.Execution.Parallel = false

	subs := []run.Subtask{pending("st-01", nil, "src/app.go")}
	root := t.TempDir()
	store, r := newSchedRun(t, cfg, subs)

	h := newWritingHarness("executor")
	h.filesByCall = []map[string]string{{
		"src/app.go":             "package src\n",
		".git/hooks/post-commit": "#!/bin/sh\n",
		".aixecutor/runs/x/note": "artifact\n",
		".idea/workspace.xml":    "<xml/>\n",
	}}

	fg := newFakeGit(root)
	fg.manifestExcludeDirs = []string{".aixecutor", ".idea"}

	var out strings.Builder
	if err := runScheduler(t, cfg, store, r, fg, h, WithSchedulerOutput(&out)); err != nil {
		t.Fatalf("scheduler: %v", err)
	}

	if strings.Contains(out.String(), "outside its declared files") {
		t.Errorf("excluded paths must not warn, got:\n%s", out.String())
	}
	if st := subtaskByID(t, r, "st-01"); len(st.Undeclared) != 0 {
		t.Errorf("excluded paths recorded as undeclared: %v", st.Undeclared)
	}
}

// TestUndeclaredDetectionErrorSwallowed proves detection is best-effort: an injected
// manifest failure is logged, but the subtask still completes successfully and the
// diff.patch is untouched.
func TestUndeclaredDetectionErrorSwallowed(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.Execution.Parallel = false

	subs := []run.Subtask{pending("st-01", nil, "feature/new.go")}
	root := t.TempDir()
	store, r := newSchedRun(t, cfg, subs)

	h := newWritingHarness("executor")
	h.filesByCall = []map[string]string{{"feature/new.go": "package feature\n"}}

	fg := newFakeGit(root)
	fg.manifestErr = errInjectedManifest

	var out strings.Builder
	if err := runScheduler(t, cfg, store, r, fg, h, WithSchedulerOutput(&out)); err != nil {
		t.Fatalf("scheduler: %v", err)
	}

	if st := subtaskByID(t, r, "st-01"); st.Status != run.SubtaskDone {
		t.Errorf("subtask status = %q; want done despite detection error", st.Status)
	}
	if !strings.Contains(out.String(), "before-manifest failed") {
		t.Errorf("expected a swallowed-detection log line, got:\n%s", out.String())
	}
	layout := run.Layout{RunsDir: store.RunsDir(), ID: r.ID, DocsSubdir: "docs"}
	if _, err := os.ReadFile(layout.SubtaskDiffFile("st-01")); err != nil {
		t.Errorf("diff.patch should still be written on the detection-error path: %v", err)
	}
}

func TestCollectDeclaredGlobs(t *testing.T) {
	got := collectDeclaredGlobs([]run.Subtask{
		{Files: []string{"a/x.go", "b/**"}},
		{Files: []string{"a/x.go", "c/y.go", ".", ""}},
	})
	want := []string{"a/x.go", "b/**", "c/y.go"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("collectDeclaredGlobs = %v; want %v", got, want)
	}
}

func TestPathDeclared(t *testing.T) {
	patterns := []string{"internal/git", "cmd/*.go", "docs/**"}
	cases := []struct {
		path string
		want bool
	}{
		{"internal/git/gateway.go", true}, // literal dir prefix
		{"internal/git", true},            // literal exact
		{"internal/gitextra/x.go", false}, // prefix must be a full segment
		{"cmd/main.go", true},             // single-segment glob
		{"cmd/sub/main.go", false},        // `*` does not cross a slash
		{"docs/a/b/c.md", true},           // `**` spans segments
		{"other/z.go", false},
	}
	for _, c := range cases {
		if got := pathDeclared(c.path, patterns); got != c.want {
			t.Errorf("pathDeclared(%q) = %v; want %v", c.path, got, c.want)
		}
	}
}

func TestUndeclaredChanges(t *testing.T) {
	changed := []string{"a/x.go", "rogue.go", "b/deep/y.go"}
	patterns := []string{"a/x.go", "b/**"}
	got := undeclaredChanges(changed, patterns)
	if want := []string{"rogue.go"}; !reflect.DeepEqual(got, want) {
		t.Errorf("undeclaredChanges = %v; want %v", got, want)
	}
}
