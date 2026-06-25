package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jaxmef/aixecutor/internal/config"
)

// helperHarness builds a config.Harness whose command re-execs THIS test binary
// into TestHelperProcess (the standard Go exec-test pattern), so Run spawns a
// real subprocess that behaves as a fake agent. mode selects the helper
// behavior; extraArgs are templated arg entries appended after the fixed
// "-test.run=... --" preamble.
func helperHarness(t *testing.T, delivery, output string, extraArgs ...string) config.Harness {
	t.Helper()
	args := append([]string{
		"-test.run=TestHelperProcess",
		"--",
	}, extraArgs...)
	h := config.Harness{
		Type:           "cli",
		Command:        os.Args[0],
		PromptDelivery: delivery,
		Args:           args,
		Output:         output,
		Timeout:        config.Duration(30 * time.Second),
		Env:            map[string]string{},
	}
	if output == outputJSON {
		h.ResultPath = "result"
	}
	return h
}

// runWithMode constructs the helper harness, forces HELPER_MODE=mode via the
// request env, runs it, and returns the Result/err.
func runWithMode(t *testing.T, cfg config.Harness, mode string, req Request) (Result, error) {
	t.Helper()
	h, err := newCLIHarness("helper", cfg)
	if err != nil {
		t.Fatalf("newCLIHarness: %v", err)
	}
	if req.Env == nil {
		req.Env = map[string]string{}
	}
	req.Env["GO_WANT_HELPER_PROCESS"] = "1"
	req.Env["HELPER_MODE"] = mode
	return h.Run(context.Background(), req)
}

// TestCLIArgDelivery proves promptDelivery: arg renders {{.Prompt}} (and the
// other request fields) straight into argv, exercised through a real subprocess.
func TestCLIArgDelivery(t *testing.T) {
	cfg := helperHarness(t, deliveryArg, outputText, "{{.Prompt}}", "{{.Model}}", "{{.PermissionMode}}")
	res, err := runWithMode(t, cfg, "echo-args", Request{
		Prompt:         "do the thing",
		Model:          "sonnet",
		PermissionMode: "acceptEdits",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := scanLines(res.Text)
	want := []string{"do the thing", "sonnet", "acceptEdits"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("rendered args = %v, want %v (text=%q)", got, want, res.Text)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
	if res.Duration <= 0 {
		t.Errorf("duration = %v, want > 0", res.Duration)
	}
}

// TestCLIStdinDelivery proves promptDelivery: stdin writes the prompt to the
// child's stdin.
func TestCLIStdinDelivery(t *testing.T) {
	cfg := helperHarness(t, deliveryStdin, outputText)
	res, err := runWithMode(t, cfg, "echo-stdin", Request{Prompt: "prompt-via-stdin"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "STDIN:prompt-via-stdin" {
		t.Errorf("stdin echo = %q, want %q", res.Text, "STDIN:prompt-via-stdin")
	}
}

// TestCLIFileDelivery proves promptDelivery: file writes the prompt to a temp
// file under the OS temp dir, exposes it as {{.PromptFile}}, and removes it after
// the run.
func TestCLIFileDelivery(t *testing.T) {
	cfg := helperHarness(t, deliveryFile, outputText, "{{.PromptFile}}")
	res, err := runWithMode(t, cfg, "cat-file", Request{Prompt: "prompt-in-file"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	lines := scanLines(res.Text)
	if len(lines) != 2 {
		t.Fatalf("expected FILE/PATH lines, got %q", res.Text)
	}
	if lines[0] != "FILE:prompt-in-file" {
		t.Errorf("file contents = %q, want %q", lines[0], "FILE:prompt-in-file")
	}
	path := strings.TrimPrefix(lines[1], "PATH:")
	// The temp file must have lived under the OS temp dir, not in the repo.
	tmp := os.TempDir()
	if rel, err := filepath.Rel(tmp, path); err != nil || strings.HasPrefix(rel, "..") {
		t.Errorf("prompt file %q is not under OS temp dir %q (rel=%q, err=%v)", path, tmp, rel, err)
	}
	// And it must be removed now that Run returned.
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("prompt file %q should have been cleaned up, stat err = %v", path, err)
	}
}

// TestCLIJSONOutput proves json output extracts the configured resultPath.
func TestCLIJSONOutput(t *testing.T) {
	cfg := helperHarness(t, deliveryArg, outputJSON)
	cfg.ResultPath = "result"
	res, err := runWithMode(t, cfg, "emit-json", Request{Prompt: "x"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "top-ok" {
		t.Errorf("Text = %q, want %q", res.Text, "top-ok")
	}
	// Raw must retain the full JSON for logging.
	if !strings.Contains(string(res.Raw), "nested-ok") {
		t.Errorf("Raw should hold full stdout, got %q", res.Raw)
	}
}

// TestCLIJSONDottedPath proves a dot-separated resultPath traverses nested
// objects.
func TestCLIJSONDottedPath(t *testing.T) {
	cfg := helperHarness(t, deliveryArg, outputJSON)
	cfg.ResultPath = "outer.result"
	res, err := runWithMode(t, cfg, "emit-json", Request{Prompt: "x"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "nested-ok" {
		t.Errorf("Text = %q, want %q", res.Text, "nested-ok")
	}
}

// TestCLIJSONMissingPath proves a missing resultPath is a clear error.
func TestCLIJSONMissingPath(t *testing.T) {
	cfg := helperHarness(t, deliveryArg, outputJSON)
	cfg.ResultPath = "nope.missing"
	_, err := runWithMode(t, cfg, "emit-json", Request{Prompt: "x"})
	if err == nil {
		t.Fatal("expected error for missing resultPath, got nil")
	}
	if !strings.Contains(err.Error(), "nope.missing") || !strings.Contains(err.Error(), "helper") {
		t.Errorf("error should name the path and harness, got: %v", err)
	}
}

// TestCLITextOutput proves text output returns stdout verbatim.
func TestCLITextOutput(t *testing.T) {
	cfg := helperHarness(t, deliveryStdin, outputText)
	res, err := runWithMode(t, cfg, "echo-stdin", Request{Prompt: "verbatim"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "STDIN:verbatim" {
		t.Errorf("Text = %q, want %q", res.Text, "STDIN:verbatim")
	}
}

// TestCLINonZeroExit proves a non-zero exit yields an error with the code and a
// stderr tail, and that Result.ExitCode is populated.
func TestCLINonZeroExit(t *testing.T) {
	cfg := helperHarness(t, deliveryArg, outputText)
	res, err := runWithMode(t, cfg, "fail", Request{
		Prompt: "x",
		Env:    map[string]string{"HELPER_EXIT": "9"},
	})
	if err == nil {
		t.Fatal("expected error on non-zero exit, got nil")
	}
	if res.ExitCode != 9 {
		t.Errorf("ExitCode = %d, want 9", res.ExitCode)
	}
	if !strings.Contains(err.Error(), "9") {
		t.Errorf("error should mention exit code 9, got: %v", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should include a stderr tail (\"boom\"), got: %v", err)
	}
}

// TestCLIEnvPropagation proves the process env is current env + cfg.Env +
// req.Env, with req winning on conflicts.
func TestCLIEnvPropagation(t *testing.T) {
	cfg := helperHarness(t, deliveryArg, outputText, "FROM_CFG", "FROM_REQ", "OVERRIDE_ME")
	cfg.Env = map[string]string{
		"FROM_CFG":    "cfg-value",
		"OVERRIDE_ME": "cfg-loses",
	}
	res, err := runWithMode(t, cfg, "print-env", Request{
		Prompt: "x",
		Env: map[string]string{
			"FROM_REQ":    "req-value",
			"OVERRIDE_ME": "req-wins",
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := map[string]string{}
	for _, line := range scanLines(res.Text) {
		if k, v, ok := strings.Cut(line, "="); ok {
			got[k] = v
		}
	}
	if got["FROM_CFG"] != "cfg-value" {
		t.Errorf("FROM_CFG = %q, want %q", got["FROM_CFG"], "cfg-value")
	}
	if got["FROM_REQ"] != "req-value" {
		t.Errorf("FROM_REQ = %q, want %q", got["FROM_REQ"], "req-value")
	}
	if got["OVERRIDE_ME"] != "req-wins" {
		t.Errorf("OVERRIDE_ME = %q, want %q (req env must win)", got["OVERRIDE_ME"], "req-wins")
	}
}

// TestCLIWorkDirPropagation proves Request.WorkDir becomes the subprocess cwd.
func TestCLIWorkDirPropagation(t *testing.T) {
	dir := t.TempDir()
	// Resolve symlinks because macOS temp dirs are under /var -> /private/var.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	cfg := helperHarness(t, deliveryArg, outputText)
	res, err := runWithMode(t, cfg, "print-cwd", Request{Prompt: "x", WorkDir: dir})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	gotResolved, err := filepath.EvalSymlinks(strings.TrimSpace(res.Text))
	if err != nil {
		t.Fatalf("EvalSymlinks(child cwd): %v", err)
	}
	if gotResolved != resolved {
		t.Errorf("child cwd = %q, want %q", gotResolved, resolved)
	}
}

// TestCLITimeoutKillsProcess proves a long-running child is killed when the
// request timeout expires, and a descriptive timed-out error is returned within
// a reasonable bound.
func TestCLITimeoutKillsProcess(t *testing.T) {
	cfg := helperHarness(t, deliveryArg, outputText)
	h, err := newCLIHarness("helper", cfg)
	if err != nil {
		t.Fatalf("newCLIHarness: %v", err)
	}
	start := time.Now()
	res, err := h.Run(context.Background(), Request{
		Prompt:  "x",
		Timeout: 150 * time.Millisecond,
		Env: map[string]string{
			"GO_WANT_HELPER_PROCESS": "1",
			"HELPER_MODE":            "sleep",
			"HELPER_SLEEP":           "30s",
		},
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error should mention timeout, got: %v", err)
	}
	if !strings.Contains(err.Error(), "helper") {
		t.Errorf("error should name the harness, got: %v", err)
	}
	// We should not have waited anywhere near the 30s sleep; allow generous
	// slack for CI scheduling but well under the sleep.
	if elapsed > 5*time.Second {
		t.Errorf("timeout took %v, expected to be killed quickly", elapsed)
	}
	if res.Duration <= 0 {
		t.Errorf("Duration should be set even on timeout, got %v", res.Duration)
	}
}

// TestCLIContextCancel proves an already-canceled parent context aborts the run
// promptly (the timeout machinery composes with caller cancellation).
func TestCLIContextCancel(t *testing.T) {
	cfg := helperHarness(t, deliveryArg, outputText)
	h, err := newCLIHarness("helper", cfg)
	if err != nil {
		t.Fatalf("newCLIHarness: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before running
	start := time.Now()
	_, err = h.Run(ctx, Request{
		Prompt: "x",
		Env: map[string]string{
			"GO_WANT_HELPER_PROCESS": "1",
			"HELPER_MODE":            "sleep",
			"HELPER_SLEEP":           "30s",
		},
	})
	if err == nil {
		t.Fatal("expected error from canceled context, got nil")
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("canceled run took too long: %v", time.Since(start))
	}
}
