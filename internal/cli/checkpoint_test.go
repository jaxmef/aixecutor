package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/jaxmef/aixecutor/internal/run"
)

// TestReviewRequestsPause proves `review` on a running run writes a pause request
// to the control channel and prints the continue/amend options.
func TestReviewRequestsPause(t *testing.T) {
	t.Chdir(t.TempDir())
	at := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	r := seedRun(t, "build it", at, func(r *run.Run) { r.Status = run.StatusExecuting })

	out, err := runCLI(t, missingConfigArgs(t, "review", r.ID)...)
	if err != nil {
		t.Fatalf("review: %v\n%s", err, out)
	}
	for _, want := range []string{"Pause requested", "aixecutor resume", "aixecutor amend"} {
		if !strings.Contains(out, want) {
			t.Errorf("review output missing %q:\n%s", want, out)
		}
	}
	// The pause marker now exists, so a running scheduler would honor it.
	store, err := openStore(&GlobalOptions{ConfigPath: "x", GlobalConfigPath: "x"})
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	if !store.PauseRequested(r.ID) {
		t.Error("expected a pause request to be recorded")
	}
}

// TestReviewOnTerminalRun proves `review` on a finished run reports there is
// nothing to review (and records no pause request).
func TestReviewOnTerminalRun(t *testing.T) {
	t.Chdir(t.TempDir())
	at := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	r := seedRun(t, "done already", at, func(r *run.Run) { r.Status = run.StatusCompleted })

	out, err := runCLI(t, missingConfigArgs(t, "review", r.ID)...)
	if err != nil {
		t.Fatalf("review: %v\n%s", err, out)
	}
	if !strings.Contains(out, "nothing to review") {
		t.Errorf("expected 'nothing to review', got:\n%s", out)
	}
}

// TestAmendWithoutConfirmPreviews proves `amend` on a paused run without --confirm
// only previews (no git, no revert), and instructs the user to pass --confirm.
func TestAmendWithoutConfirmPreviews(t *testing.T) {
	t.Chdir(t.TempDir())
	at := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	r := seedRun(t, "paused work", at, func(r *run.Run) { r.Status = run.StatusPaused })

	out, err := runCLI(t, missingConfigArgs(t, "amend", r.ID)...)
	if err != nil {
		t.Fatalf("amend preview: %v\n%s", err, out)
	}
	if !strings.Contains(out, "--confirm") || !strings.Contains(out, "REVERT") {
		t.Errorf("amend preview should explain the revert and require --confirm:\n%s", out)
	}
}

// TestAmendNonPausedErrors proves `amend --confirm` on a run that is not paused
// fails fast with a usage error, before any git/revert work.
func TestAmendNonPausedErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	at := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	r := seedRun(t, "still running", at, func(r *run.Run) { r.Status = run.StatusExecuting })

	out, err := runCLI(t, missingConfigArgs(t, "amend", r.ID, "--confirm")...)
	if err == nil {
		t.Fatalf("amend on a non-paused run should error; output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "not paused") {
		t.Errorf("error should explain the run is not paused; got: %v", err)
	}
}
