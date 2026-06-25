package log

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jaxmef/aixecutor/internal/harness"
)

// stubHarness is a minimal harness.Harness for the invocation-logging tests: it
// returns a fixed result/error and records nothing (the wrapper is what we test).
type stubHarness struct {
	name string
	res  harness.Result
	err  error
}

func (h *stubHarness) Name() string { return h.name }
func (h *stubHarness) Run(context.Context, harness.Request) (harness.Result, error) {
	return h.res, h.err
}

// TestInvocationLogsStructuredLineAndPersistsRaw proves a wrapped invocation emits
// a structured log line with role/harness/model/duration/exit-code AND a pointer
// to a persisted raw-output file that actually exists with the agent's output.
func TestInvocationLogsStructuredLineAndPersistsRaw(t *testing.T) {
	var console bytes.Buffer
	logger := New(Normal, &console)
	logsDir := t.TempDir()
	if err := logger.AttachRunFile(logsDir); err != nil {
		t.Fatalf("AttachRunFile: %v", err)
	}

	raw := []byte("the full agent stdout\nwith detail")
	inner := &stubHarness{name: "claude", res: harness.Result{
		Text: "ok", Raw: raw, ExitCode: 0, Duration: 1500 * time.Millisecond,
	}}
	wrapped := WrapHarness(inner, logger)

	res, err := wrapped.Run(context.Background(), harness.Request{
		Role: "planner", Model: "opus", WorkDir: "/repo",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "ok" {
		t.Errorf("wrapper changed the result Text: %q", res.Text)
	}

	logTxt := console.String()
	for _, want := range []string{
		"role=planner", "harness=claude", "model=opus", "workdir=/repo",
		"duration=1.5s", "exitCode=0", "output=",
	} {
		if !strings.Contains(logTxt, want) {
			t.Errorf("log line missing %q:\n%s", want, logTxt)
		}
	}

	// The persisted raw-output file must exist and hold the raw bytes.
	matches, _ := filepath.Glob(filepath.Join(logsDir, "planner-*.out"))
	if len(matches) != 1 {
		t.Fatalf("expected exactly one planner-*.out file, got %v", matches)
	}
	got, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("reading persisted raw output: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("persisted raw = %q, want %q", got, raw)
	}
}

// TestInvocationRedactsSecretEnv is the load-bearing redaction guard (AIX-0014): a
// secret env VALUE must NEVER appear in the logs — neither in the structured line
// nor anywhere else — while the (redacted) key name may.
func TestInvocationRedactsSecretEnv(t *testing.T) {
	var console bytes.Buffer
	logger := New(Normal, &console)
	logsDir := t.TempDir()
	if err := logger.AttachRunFile(logsDir); err != nil {
		t.Fatalf("AttachRunFile: %v", err)
	}

	const secret = "sk-ant-SUPER-SECRET-VALUE"
	inner := &stubHarness{name: "claude", res: harness.Result{Text: "ok", Raw: []byte("out")}}
	wrapped := WrapHarness(inner, logger)

	_, err := wrapped.Run(context.Background(), harness.Request{
		Role:  "executor",
		Model: "sonnet",
		Env:   map[string]string{"ANTHROPIC_API_KEY": secret},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Assert the secret value is absent from BOTH sinks.
	if strings.Contains(console.String(), secret) {
		t.Errorf("secret value leaked into console log:\n%s", console.String())
	}
	fileData, _ := os.ReadFile(filepath.Join(logsDir, "aixecutor.log"))
	if strings.Contains(string(fileData), secret) {
		t.Errorf("secret value leaked into run log file:\n%s", fileData)
	}
	// The redacted key name should still be recorded (so the env's presence is
	// visible) — proving redaction, not omission.
	if !strings.Contains(console.String(), "ANTHROPIC_API_KEY (redacted)") {
		t.Errorf("expected the redacted key name in the log:\n%s", console.String())
	}
}

// TestInvocationLogsFailureWithKind proves a failed invocation is logged at warn
// level with the classified failure kind and the error, distinguishing "couldn't
// run" from "bad output".
func TestInvocationLogsFailureWithKind(t *testing.T) {
	var console bytes.Buffer
	logger := New(Normal, &console)

	inner := &stubHarness{
		name: "claude",
		res:  harness.Result{ExitCode: -1},
		err:  harness.BadOutputError(errors.New("agent ran but produced no usable result")),
	}
	wrapped := WrapHarness(inner, logger)
	if _, err := wrapped.Run(context.Background(), harness.Request{Role: "subtask-reviewer"}); err == nil {
		t.Fatal("expected the inner error to surface")
	}

	out := console.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("failure should log at WARN:\n%s", out)
	}
	if !strings.Contains(out, "kind=bad-output") {
		t.Errorf("failure log should carry the classified kind:\n%s", out)
	}
	if !strings.Contains(out, "role=subtask-reviewer") {
		t.Errorf("failure log should carry the role:\n%s", out)
	}
}

// TestWrapHarnessNilLoggerIsPassthrough proves wrapping with a nil logger returns
// the harness unchanged (no overhead, no decoration).
func TestWrapHarnessNilLoggerIsPassthrough(t *testing.T) {
	inner := &stubHarness{name: "claude"}
	if got := WrapHarness(inner, nil); got != inner {
		t.Errorf("WrapHarness(_, nil) = %T, want the inner harness unchanged", got)
	}
}
