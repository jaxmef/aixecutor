package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/jaxmef/aixecutor/internal/run"
)

// TestStopRequestsStop proves `stop` on a running run writes a stop request to the
// control channel and prints the resume hint.
func TestStopRequestsStop(t *testing.T) {
	t.Chdir(t.TempDir())
	at := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	r := seedRun(t, "build it", at, func(r *run.Run) { r.Status = run.StatusExecuting })

	out, err := runCLI(t, missingConfigArgs(t, "stop", r.ID)...)
	if err != nil {
		t.Fatalf("stop: %v\n%s", err, out)
	}
	for _, want := range []string{"Stop requested", "cancelling in-flight work", "aixecutor resume"} {
		if !strings.Contains(out, want) {
			t.Errorf("stop output missing %q:\n%s", want, out)
		}
	}
	// The stop marker now exists, so a running scheduler would honor it.
	store, err := openStore(&GlobalOptions{ConfigPath: "x", GlobalConfigPath: "x"})
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	if !store.StopRequested(r.ID) {
		t.Error("expected a stop request to be recorded")
	}
}

// TestStopOnTerminalRun proves `stop` on a finished run reports there is nothing to
// stop (and records no stop request).
func TestStopOnTerminalRun(t *testing.T) {
	t.Chdir(t.TempDir())
	at := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	r := seedRun(t, "done already", at, func(r *run.Run) { r.Status = run.StatusCompleted })

	out, err := runCLI(t, missingConfigArgs(t, "stop", r.ID)...)
	if err != nil {
		t.Fatalf("stop: %v\n%s", err, out)
	}
	if !strings.Contains(out, "nothing to stop") {
		t.Errorf("expected 'nothing to stop', got:\n%s", out)
	}
	store, err := openStore(&GlobalOptions{ConfigPath: "x", GlobalConfigPath: "x"})
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	if store.StopRequested(r.ID) {
		t.Error("a terminal run must not record a stop request")
	}
}

// TestStopAlias proves the `abort` alias resolves to the same command.
func TestStopAlias(t *testing.T) {
	t.Chdir(t.TempDir())
	at := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	r := seedRun(t, "build it", at, func(r *run.Run) { r.Status = run.StatusExecuting })

	out, err := runCLI(t, missingConfigArgs(t, "abort", r.ID)...)
	if err != nil {
		t.Fatalf("abort: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Stop requested") {
		t.Errorf("abort alias output missing 'Stop requested':\n%s", out)
	}
}
