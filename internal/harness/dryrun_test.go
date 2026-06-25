package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// recordingHarness is a Harness that fails the test if Run is ever called. It
// proves the dry-run wrapper short-circuits and never touches the inner harness.
type recordingHarness struct {
	name   string
	called bool
}

func (r *recordingHarness) Name() string { return r.name }
func (r *recordingHarness) Run(context.Context, Request) (Result, error) {
	r.called = true
	return Result{}, errors.New("inner harness should not run under dry-run")
}

// captureLogger records Infof lines so the dry-run log can be asserted.
type captureLogger struct{ lines []string }

func (c *captureLogger) Infof(format string, args ...any) {
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

// TestDryRunDoesNotExecuteInner is the headline guard: the wrapper returns a
// deterministic placeholder and never calls the underlying harness.
func TestDryRunDoesNotExecuteInner(t *testing.T) {
	inner := &recordingHarness{name: "claude"}
	log := &captureLogger{}
	d := newDryRun(inner, log)

	if d.Name() != "claude" {
		t.Errorf("Name = %q, want claude (wrapper must be transparent)", d.Name())
	}

	res, err := d.Run(context.Background(), Request{
		Prompt:  "this should never reach a process",
		Model:   "opus",
		WorkDir: "/repo",
	})
	if err != nil {
		t.Fatalf("dry-run Run returned error: %v", err)
	}
	if inner.called {
		t.Fatal("inner harness was executed under dry-run")
	}
	if res.Text != "[dry-run] claude would run" {
		t.Errorf("placeholder Text = %q, want %q", res.Text, "[dry-run] claude would run")
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	// It should have logged the intended invocation.
	if len(log.lines) != 1 || !strings.Contains(log.lines[0], "harness=claude") || !strings.Contains(log.lines[0], "model=opus") {
		t.Errorf("expected one log line naming harness/model, got %v", log.lines)
	}
}

// TestDryRunNilLoggerSafe checks a nil logger does not panic.
func TestDryRunNilLoggerSafe(t *testing.T) {
	inner := &recordingHarness{name: "pi"}
	d := newDryRun(inner, nil)
	if _, err := d.Run(context.Background(), Request{Prompt: "x"}); err != nil {
		t.Fatalf("Run with nil logger: %v", err)
	}
	if inner.called {
		t.Fatal("inner harness ran")
	}
}

// TestTruncate checks the prompt-preview truncation counts runes and only adds
// the marker when it actually cut.
func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate short = %q, want unchanged", got)
	}
	long := strings.Repeat("a", 200)
	got := truncate(long, promptPreviewLen)
	if len([]rune(got)) != promptPreviewLen+1 { // +1 for the ellipsis rune
		t.Errorf("truncated rune length = %d, want %d", len([]rune(got)), promptPreviewLen+1)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected ellipsis suffix, got %q", got)
	}
}
