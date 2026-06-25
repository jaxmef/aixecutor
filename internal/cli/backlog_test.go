package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jaxmef/aixecutor/internal/backlog"
)

// TestBacklogRunRequiresDir proves that with no directory argument and no
// backlog.dir in config, `backlog run` fails fast with an actionable usage error —
// before any git/pipeline work.
func TestBacklogRunRequiresDir(t *testing.T) {
	t.Chdir(t.TempDir())
	out, err := runCLI(t, missingConfigArgs(t, "backlog", "run")...)
	if err == nil {
		t.Fatalf("backlog run with no dir should error; output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "no backlog directory") {
		t.Errorf("error should explain the missing dir; got: %v", err)
	}
}

// TestBacklogRunRejectsInvalidGate proves an invalid --gate is a clear usage error,
// resolved before any git/pipeline work.
func TestBacklogRunRejectsInvalidGate(t *testing.T) {
	t.Chdir(t.TempDir())
	out, err := runCLI(t, missingConfigArgs(t, "backlog", "run", t.TempDir(), "--gate", "bogus")...)
	if err == nil {
		t.Fatalf("invalid --gate should error; output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "invalid gate") {
		t.Errorf("error should explain the invalid gate; got: %v", err)
	}
}

// TestLoadBacklogStateRejectsDirMismatch proves the runner state guard: reusing a
// state file that tracks a different backlog directory is refused.
func TestLoadBacklogStateRejectsDirMismatch(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "backlog-state.yaml")
	// Seed a state file that tracks some other directory.
	if err := backlog.SaveState(statePath, &backlog.State{
		Dir:     "/some/other/backlog",
		Tickets: map[string]*backlog.TicketState{},
	}); err != nil {
		t.Fatal(err)
	}

	_, err := loadBacklogState(statePath, dir)
	if err == nil || !strings.Contains(err.Error(), "different directory") {
		t.Errorf("expected a dir-mismatch error, got %v", err)
	}
	if code := exitCodeFor(err); code != exitUsage {
		t.Errorf("exit code = %d, want exitUsage", code)
	}
}

// TestLoadBacklogStateFresh proves a fresh (missing) state adopts the given dir.
func TestLoadBacklogStateFresh(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "nope.yaml")
	state, err := loadBacklogState(statePath, dir)
	if err != nil {
		t.Fatalf("loadBacklogState: %v", err)
	}
	abs, _ := filepath.Abs(dir)
	if state.Dir != abs {
		t.Errorf("state.Dir = %q, want %q", state.Dir, abs)
	}
	_ = os.RemoveAll(statePath)
}
