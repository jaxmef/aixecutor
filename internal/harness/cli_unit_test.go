package harness

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jaxmef/aixecutor/internal/config"
)

// fakeRunner is an injectable runnerFunc that records the command it was handed
// (args, dir, env, stdin) and returns a canned runResult, so the parsing/
// delivery logic can be unit-tested without spawning a process.
type fakeRunner struct {
	gotArgs  []string
	gotDir   string
	gotEnv   []string
	gotStdin []byte
	result   runResult
	err      error
}

func (f *fakeRunner) run(_ context.Context, cmd *exec.Cmd, stdin []byte) (runResult, error) {
	// cmd.Args[0] is the command path; the rendered args follow.
	if len(cmd.Args) > 1 {
		f.gotArgs = cmd.Args[1:]
	} else {
		f.gotArgs = nil
	}
	f.gotDir = cmd.Dir
	f.gotEnv = cmd.Env
	f.gotStdin = stdin
	return f.result, f.err
}

// newFakeHarness builds a cliHarness with an injected fakeRunner.
func newFakeHarness(t *testing.T, cfg config.Harness, fr *fakeRunner) *cliHarness {
	t.Helper()
	h, err := newCLIHarness("fake", cfg)
	if err != nil {
		t.Fatalf("newCLIHarness: %v", err)
	}
	h.runner = fr.run
	return h
}

// TestRenderArgsViaInjectedRunner checks template rendering of args using the
// injected runner (no subprocess), including that arg delivery inlines the
// prompt.
func TestRenderArgsViaInjectedRunner(t *testing.T) {
	fr := &fakeRunner{result: runResult{stdout: []byte("ok")}}
	cfg := config.Harness{
		Type:           "cli",
		Command:        "agent",
		PromptDelivery: deliveryArg,
		Args:           []string{"-p", "{{.Prompt}}", "--model", "{{.Model}}", "--mode", "{{.PermissionMode}}", "--dir", "{{.WorkDir}}"},
		Output:         outputText,
	}
	h := newFakeHarness(t, cfg, fr)
	_, err := h.Run(context.Background(), Request{
		Prompt:         "hello",
		Model:          "opus",
		PermissionMode: "plan",
		WorkDir:        "/work",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"-p", "hello", "--model", "opus", "--mode", "plan", "--dir", "/work"}
	if strings.Join(fr.gotArgs, "|") != strings.Join(want, "|") {
		t.Errorf("args = %v, want %v", fr.gotArgs, want)
	}
	if fr.gotDir != "/work" {
		t.Errorf("dir = %q, want %q", fr.gotDir, "/work")
	}
}

// TestStdinDeliveryViaInjectedRunner checks stdin delivery passes the prompt as
// stdin and does not add it to args.
func TestStdinDeliveryViaInjectedRunner(t *testing.T) {
	fr := &fakeRunner{result: runResult{stdout: []byte("ok")}}
	cfg := config.Harness{
		Type:           "cli",
		Command:        "agent",
		PromptDelivery: deliveryStdin,
		Args:           []string{"--flag"},
		Output:         outputText,
	}
	h := newFakeHarness(t, cfg, fr)
	if _, err := h.Run(context.Background(), Request{Prompt: "the prompt"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(fr.gotStdin) != "the prompt" {
		t.Errorf("stdin = %q, want %q", fr.gotStdin, "the prompt")
	}
	if strings.Join(fr.gotArgs, "|") != "--flag" {
		t.Errorf("args = %v, want [--flag]", fr.gotArgs)
	}
}

// TestRunnerErrorWrapped checks a runner error (not a normal exit) is wrapped
// with the harness name and command, and is unwrappable.
func TestRunnerErrorWrapped(t *testing.T) {
	sentinel := errors.New("exec: \"agent\": executable file not found in $PATH")
	fr := &fakeRunner{err: sentinel, result: runResult{exitCode: -1}}
	cfg := config.Harness{
		Type:           "cli",
		Command:        "agent",
		PromptDelivery: deliveryArg,
		Output:         outputText,
	}
	h := newFakeHarness(t, cfg, fr)
	_, err := h.Run(context.Background(), Request{Prompt: "x"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error should wrap the runner error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "fake") {
		t.Errorf("error should name the harness, got: %v", err)
	}
}

// TestTimeoutErrorViaInjectedRunner checks the timed-out runResult yields a
// descriptive timeout error mentioning elapsed time bounds and the harness name.
func TestTimeoutErrorViaInjectedRunner(t *testing.T) {
	fr := &fakeRunner{result: runResult{timedOut: true, duration: 1234 * time.Millisecond, exitCode: -1}}
	cfg := config.Harness{
		Type:           "cli",
		Command:        "agent",
		PromptDelivery: deliveryArg,
		Output:         outputText,
		Timeout:        config.Duration(time.Second),
	}
	h := newFakeHarness(t, cfg, fr)
	res, err := h.Run(context.Background(), Request{Prompt: "x"})
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "fake") {
		t.Errorf("timeout error should mention timeout and harness, got: %v", err)
	}
	if res.Duration != 1234*time.Millisecond {
		t.Errorf("Duration = %v, want 1.234s", res.Duration)
	}
}

// TestRunErrorClassification proves the generic adapter tags each failure mode
// with the FailureKind the retry wrapper relies on (AIX-0014): spawn/timeout/
// empty-output/parse-failure are TRANSIENT, while a non-zero exit (a real semantic
// failure) is HARD. The classification is read with harness.Classify.
func TestRunErrorClassification(t *testing.T) {
	jsonCfg := config.Harness{
		Type: "cli", Command: "agent", PromptDelivery: deliveryArg,
		Output: outputJSON, ResultPath: "result", Timeout: config.Duration(time.Second),
	}
	textCfg := config.Harness{
		Type: "cli", Command: "agent", PromptDelivery: deliveryArg, Output: outputText,
	}

	cases := []struct {
		name string
		cfg  config.Harness
		fr   *fakeRunner
		want FailureKind
	}{
		{
			name: "spawn failure (couldn't run the agent)",
			cfg:  textCfg,
			fr:   &fakeRunner{result: runResult{exitCode: -1}, err: errors.New("exec: \"agent\": executable file not found")},
			want: FailureSpawn,
		},
		{
			name: "timeout",
			cfg:  textCfg,
			fr:   &fakeRunner{result: runResult{timedOut: true, exitCode: -1}},
			want: FailureTimeout,
		},
		{
			name: "non-zero exit is hard",
			cfg:  textCfg,
			fr:   &fakeRunner{result: runResult{exitCode: 2, stdout: []byte("boom")}},
			want: FailureHard,
		},
		{
			name: "empty output is bad-output",
			cfg:  textCfg,
			fr:   &fakeRunner{result: runResult{exitCode: 0, stdout: []byte("   \n")}},
			want: FailureBadOutput,
		},
		{
			name: "unparseable json is bad-output",
			cfg:  jsonCfg,
			fr:   &fakeRunner{result: runResult{exitCode: 0, stdout: []byte("not json")}},
			want: FailureBadOutput,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHarness(t, tc.cfg, tc.fr)
			_, err := h.Run(context.Background(), Request{Prompt: "x"})
			if err == nil {
				t.Fatalf("%s: expected an error, got nil", tc.name)
			}
			if got := Classify(err); got != tc.want {
				t.Errorf("%s: kind = %v, want %v (err: %v)", tc.name, got, tc.want, err)
			}
		})
	}
}

// TestPerRequestTimeoutOverridesConfig verifies that an explicit request timeout
// takes precedence over the harness config timeout. We assert via the deadline
// observed by the runner.
func TestPerRequestTimeoutOverridesConfig(t *testing.T) {
	var sawDeadline time.Duration
	cfg := config.Harness{
		Type:           "cli",
		Command:        "agent",
		PromptDelivery: deliveryArg,
		Output:         outputText,
		Timeout:        config.Duration(time.Hour), // large config timeout
	}
	h, err := newCLIHarness("fake", cfg)
	if err != nil {
		t.Fatalf("newCLIHarness: %v", err)
	}
	h.runner = func(ctx context.Context, _ *exec.Cmd, _ []byte) (runResult, error) {
		if dl, ok := ctx.Deadline(); ok {
			sawDeadline = time.Until(dl)
		}
		return runResult{stdout: []byte("ok")}, nil
	}
	if _, err := h.Run(context.Background(), Request{Prompt: "x", Timeout: 2 * time.Second}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The observed deadline should reflect the 2s request timeout, not 1h.
	if sawDeadline <= 0 || sawDeadline > 30*time.Second {
		t.Errorf("deadline %v does not reflect the 2s per-request timeout", sawDeadline)
	}
}

// TestNoTimeoutWhenZero verifies that with no request and no config timeout, the
// runner sees a context without a deadline.
func TestNoTimeoutWhenZero(t *testing.T) {
	hadDeadline := true
	cfg := config.Harness{
		Type:           "cli",
		Command:        "agent",
		PromptDelivery: deliveryArg,
		Output:         outputText,
		// no Timeout
	}
	h, err := newCLIHarness("fake", cfg)
	if err != nil {
		t.Fatalf("newCLIHarness: %v", err)
	}
	h.runner = func(ctx context.Context, _ *exec.Cmd, _ []byte) (runResult, error) {
		_, hadDeadline = ctx.Deadline()
		return runResult{stdout: []byte("ok")}, nil
	}
	if _, err := h.Run(context.Background(), Request{Prompt: "x"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if hadDeadline {
		t.Error("expected no deadline when neither request nor config sets a timeout")
	}
}

// TestExtractPath exercises the dot-path JSON extractor directly across hits,
// nesting, scalar coercion, and the error cases.
func TestExtractPath(t *testing.T) {
	doc := map[string]any{
		"result": "top",
		"data":   map[string]any{"result": "nested", "count": float64(3), "ok": true},
		"null":   nil,
		"arr":    []any{1, 2},
	}
	tests := []struct {
		name    string
		path    string
		want    string
		wantErr string
	}{
		{name: "top-level string", path: "result", want: "top"},
		{name: "nested string", path: "data.result", want: "nested"},
		{name: "number coerced", path: "data.count", want: "3"},
		{name: "bool coerced", path: "data.ok", want: "true"},
		{name: "missing top", path: "nope", wantErr: "not found"},
		{name: "missing nested", path: "data.nope", wantErr: "not found"},
		{name: "descend into scalar", path: "result.deeper", wantErr: "not a JSON object"},
		{name: "null value", path: "null", wantErr: "is null"},
		{name: "non-string leaf", path: "arr", wantErr: "not a string"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractPath(doc, tc.path)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNewCLIHarnessValidation checks construction rejects invalid configs with a
// clear, harness-named error and accepts valid ones.
func TestNewCLIHarnessValidation(t *testing.T) {
	base := func() config.Harness {
		return config.Harness{
			Type:           "cli",
			Command:        "agent",
			PromptDelivery: deliveryArg,
			Output:         outputText,
		}
	}
	tests := []struct {
		name    string
		mutate  func(c *config.Harness)
		wantErr string
	}{
		{name: "valid", mutate: func(*config.Harness) {}},
		{name: "bad type", mutate: func(c *config.Harness) { c.Type = "rpc" }, wantErr: "unsupported type"},
		{name: "empty command", mutate: func(c *config.Harness) { c.Command = "" }, wantErr: "command must be set"},
		{name: "bad delivery", mutate: func(c *config.Harness) { c.PromptDelivery = "carrier-pigeon" }, wantErr: "promptDelivery"},
		{name: "bad output", mutate: func(c *config.Harness) { c.Output = "yaml" }, wantErr: "output"},
		{name: "json without resultPath", mutate: func(c *config.Harness) { c.Output = outputJSON; c.ResultPath = "" }, wantErr: "resultPath"},
		{name: "bad arg template", mutate: func(c *config.Harness) { c.Args = []string{"{{.Prompt"} }, wantErr: "parsing arg template"},
		{name: "empty type allowed", mutate: func(c *config.Harness) { c.Type = "" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mutate(&c)
			_, err := newCLIHarness("agentx", c)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), "agentx") {
				t.Errorf("error should name the harness, got: %v", err)
			}
		})
	}
}

// TestArgTemplateExecError checks a runtime template-exec failure (referencing
// PromptFile under a non-file delivery, with missingkey=error) is reported with
// context. We construct the harness with file delivery so PromptFile is set, and
// invert: reference an undefined key.
func TestArgTemplateExecError(t *testing.T) {
	fr := &fakeRunner{result: runResult{stdout: []byte("ok")}}
	cfg := config.Harness{
		Type:           "cli",
		Command:        "agent",
		PromptDelivery: deliveryArg,
		Args:           []string{"{{.Nonexistent}}"},
		Output:         outputText,
	}
	h := newFakeHarness(t, cfg, fr)
	_, err := h.Run(context.Background(), Request{Prompt: "x"})
	if err == nil {
		t.Fatal("expected template exec error, got nil")
	}
	if !strings.Contains(err.Error(), "rendering arg template") {
		t.Errorf("error should mention rendering, got: %v", err)
	}
}

// TestJSONParseErrorBadStdout checks invalid JSON stdout produces a clear error.
func TestJSONParseErrorBadStdout(t *testing.T) {
	fr := &fakeRunner{result: runResult{stdout: []byte("not json")}}
	cfg := config.Harness{
		Type:           "cli",
		Command:        "agent",
		PromptDelivery: deliveryArg,
		Output:         outputJSON,
		ResultPath:     "result",
	}
	h := newFakeHarness(t, cfg, fr)
	_, err := h.Run(context.Background(), Request{Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "decoding JSON") {
		t.Fatalf("err = %v, want JSON decode error", err)
	}
}
