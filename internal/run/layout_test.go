package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLayoutPaths(t *testing.T) {
	l := Layout{RunsDir: "/runs", ID: "20260623T120501-x", DocsSubdir: "docs"}

	checks := map[string]string{
		"Dir":                l.Dir(),
		"RunFile":            l.RunFile(),
		"TaskFile":           l.TaskFile(),
		"ConfigSnapshotFile": l.ConfigSnapshotFile(),
		"DocsDir":            l.DocsDir(),
		"SubtasksDir":        l.SubtasksDir(),
		"SeniorReviewDir":    l.SeniorReviewDir(),
		"LogsDir":            l.LogsDir(),
		"BaselineDir":        l.BaselineDir(),
	}
	wantSuffix := map[string]string{
		"Dir":                filepath.Join("runs", "20260623T120501-x"),
		"RunFile":            "run.yaml",
		"TaskFile":           "task.md",
		"ConfigSnapshotFile": "config.snapshot.yaml",
		"DocsDir":            "docs",
		"SubtasksDir":        "subtasks",
		"SeniorReviewDir":    "senior-review",
		"LogsDir":            "logs",
		"BaselineDir":        ".baseline",
	}
	for name, got := range checks {
		if !strings.HasSuffix(got, wantSuffix[name]) {
			t.Errorf("%s = %q, want suffix %q", name, got, wantSuffix[name])
		}
		if !strings.HasPrefix(got, l.Dir()) {
			t.Errorf("%s = %q, want under run dir %q", name, got, l.Dir())
		}
	}
}

func TestLayoutDocsSubdirDefaults(t *testing.T) {
	l := Layout{RunsDir: "/runs", ID: "id"} // DocsSubdir empty
	if got := l.DocsDir(); !strings.HasSuffix(got, "docs") {
		t.Errorf("DocsDir with empty subdir = %q, want default 'docs'", got)
	}
	if l.DocsDir() == l.Dir() {
		t.Error("DocsDir with empty subdir must not equal the run root")
	}

	l2 := Layout{RunsDir: "/runs", ID: "id", DocsSubdir: "planning"}
	if got := l2.DocsDir(); !strings.HasSuffix(got, "planning") {
		t.Errorf("DocsDir = %q, want suffix 'planning'", got)
	}
}

func TestLayoutSubtaskPathsSanitized(t *testing.T) {
	l := Layout{RunsDir: "/runs", ID: "id", DocsSubdir: "docs"}
	// A hostile subtask id must not escape the subtasks dir.
	dir := l.SubtaskDir("../../etc")
	if !strings.HasPrefix(dir, l.SubtasksDir()) {
		t.Errorf("SubtaskDir(%q) = %q escaped %q", "../../etc", dir, l.SubtasksDir())
	}
	diff := l.SubtaskDiffFile("ok")
	if !strings.HasSuffix(diff, filepath.Join("subtasks", "ok", "diff.patch")) {
		t.Errorf("SubtaskDiffFile = %q, want .../subtasks/ok/diff.patch", diff)
	}
	rev := l.SubtaskReviewsDir("ok")
	if !strings.HasSuffix(rev, filepath.Join("subtasks", "ok", "reviews")) {
		t.Errorf("SubtaskReviewsDir = %q, want .../subtasks/ok/reviews", rev)
	}
}

func TestEnsureDirsIdempotent(t *testing.T) {
	root := t.TempDir()
	l := Layout{RunsDir: root, ID: "run1", DocsSubdir: "docs"}

	for i := 0; i < 2; i++ { // calling twice must not error (resume-safe)
		if err := l.EnsureDirs(); err != nil {
			t.Fatalf("EnsureDirs call %d: %v", i, err)
		}
	}
	for _, d := range []string{l.Dir(), l.DocsDir(), l.SubtasksDir(), l.SeniorReviewDir(), l.LogsDir(), l.BaselineDir()} {
		info, err := os.Stat(d)
		if err != nil || !info.IsDir() {
			t.Errorf("expected dir %q to exist: err=%v", d, err)
		}
	}
}

func TestLatestPointerRoundTrip(t *testing.T) {
	root := t.TempDir()

	if _, err := ReadLatest(root); !os.IsNotExist(err) {
		t.Errorf("ReadLatest with no pointer: err=%v, want os.ErrNotExist", err)
	}

	if err := WriteLatest(root, "run-a"); err != nil {
		t.Fatalf("WriteLatest: %v", err)
	}
	got, err := ReadLatest(root)
	if err != nil {
		t.Fatalf("ReadLatest: %v", err)
	}
	if got != "run-a" {
		t.Errorf("ReadLatest = %q, want %q", got, "run-a")
	}

	// Overwriting is idempotent and updates the value.
	if err := WriteLatest(root, "run-b"); err != nil {
		t.Fatalf("WriteLatest overwrite: %v", err)
	}
	if got, _ := ReadLatest(root); got != "run-b" {
		t.Errorf("ReadLatest after overwrite = %q, want %q", got, "run-b")
	}

	// The pointer is a plain file, not a symlink (Windows portability).
	info, err := os.Lstat(LatestPointerPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Error("latest pointer must be a regular file, not a symlink")
	}
}
