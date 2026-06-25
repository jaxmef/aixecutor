package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jaxmef/aixecutor/internal/config"
	"github.com/jaxmef/aixecutor/internal/harness"
)

// testdataPath resolves a file under the repo-root testdata/harness/claude dir.
// The package lives at internal/harness/claude, so testdata is three levels up.
func testdataPath(name string) string {
	return filepath.Join("..", "..", "..", "testdata", "harness", "claude", name)
}

// readTestdata reads a recorded claude JSON sample, failing the test on error.
func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(testdataPath(name))
	if err != nil {
		t.Fatalf("reading testdata %q: %v", name, err)
	}
	return data
}

// TestParseResultSuccess proves the parser extracts the "result" text from a
// faithful success envelope carrying the usual metadata fields.
func TestParseResultSuccess(t *testing.T) {
	text, err := ParseResult(readTestdata(t, "success.json"))
	if err != nil {
		t.Fatalf("ParseResult: %v", err)
	}
	want := "Implemented the feature and all tests pass."
	if text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
}

// TestParseResultExtraFields proves unknown/extra envelope fields are tolerated:
// a sample with fields the struct does not model must still parse and yield the
// result text.
func TestParseResultExtraFields(t *testing.T) {
	text, err := ParseResult(readTestdata(t, "success_extra_fields.json"))
	if err != nil {
		t.Fatalf("ParseResult: %v", err)
	}
	want := "Refactored the parser and added table-driven tests."
	if text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
}

// TestParseResultErrorEnvelope proves an is_error:true envelope yields a Go
// error (not the error text as a result), and that the error includes the
// subtype and the envelope's message for actionable diagnostics.
func TestParseResultErrorEnvelope(t *testing.T) {
	text, err := ParseResult(readTestdata(t, "error.json"))
	if err == nil {
		t.Fatalf("expected an error for an error envelope, got text %q", text)
	}
	if text != "" {
		t.Errorf("text = %q, want empty on error", text)
	}
	if !strings.Contains(err.Error(), "error_during_execution") {
		t.Errorf("error should include the subtype, got: %v", err)
	}
	if !strings.Contains(err.Error(), "could not complete the request") {
		t.Errorf("error should include the envelope message, got: %v", err)
	}
}

// TestParseResultMalformed proves non-JSON output is a clear decode error rather
// than a panic or a silent empty result.
func TestParseResultMalformed(t *testing.T) {
	_, err := ParseResult([]byte("not json at all"))
	if err == nil {
		t.Fatal("expected a decode error for non-JSON output, got nil")
	}
	if !strings.Contains(err.Error(), "decoding claude JSON envelope") {
		t.Errorf("error should mention JSON decoding, got: %v", err)
	}
}

// TestParseResultEmpty proves empty output is a clear error.
func TestParseResultEmpty(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte(""), []byte("   \n\t ")} {
		if _, err := ParseResult(raw); err == nil {
			t.Errorf("ParseResult(%q): expected error, got nil", raw)
		}
	}
}

// TestParseResultMissingResult proves a non-error envelope with no result text
// is treated as an error, so an empty envelope is never mistaken for success.
func TestParseResultMissingResult(t *testing.T) {
	_, err := ParseResult([]byte(`{"type":"result","subtype":"success","is_error":false}`))
	if err == nil {
		t.Fatal("expected an error for a missing result, got nil")
	}
	if !strings.Contains(err.Error(), "no \"result\" text") {
		t.Errorf("error should explain the missing result, got: %v", err)
	}
}

// helperConfig builds a config.Harness whose command re-execs THIS test binary
// into TestHelperProcess (the standard Go exec-test pattern), so the preset's
// Run spawns a fake "claude" that emits a canned JSON envelope to stdout and
// records the argv it received. The args mirror the real default claude block so
// {{.Model}} and {{.PermissionMode}} are templated through exactly as in prod.
func helperConfig(t *testing.T) config.Harness {
	t.Helper()
	return config.Harness{
		Type:           "cli",
		Command:        os.Args[0],
		PromptDelivery: "arg",
		Args: []string{
			"-test.run=TestHelperProcess",
			"--",
			"-p",
			"{{.Prompt}}",
			"--output-format",
			"json",
			"--model",
			"{{.Model}}",
			"--permission-mode",
			"{{.PermissionMode}}",
		},
		// Mirror the real default (json/resultPath); the preset forces text
		// output on the inner adapter internally, so this exercises that path.
		Output:     "json",
		ResultPath: "result",
		Timeout:    config.Duration(30 * time.Second),
		Env:        map[string]string{},
	}
}

// TestRunExtractsResult drives the preset's full Run through a real subprocess
// (no real claude): the fake agent emits a success envelope and Run must return
// the extracted "result" text with the raw stdout preserved.
func TestRunExtractsResult(t *testing.T) {
	h, err := Factory("claude", helperConfig(t))
	if err != nil {
		t.Fatalf("Factory: %v", err)
	}
	if h.Name() != "claude" {
		t.Errorf("Name = %q, want claude", h.Name())
	}

	argsFile := filepath.Join(t.TempDir(), "argv.txt")
	res, err := h.Run(context.Background(), harness.Request{
		Prompt:         "do the thing",
		Model:          "sonnet",
		PermissionMode: "acceptEdits",
		Env: map[string]string{
			"GO_WANT_HELPER_PROCESS": "1",
			"HELPER_MODE":            "claude-success",
			"HELPER_ARGS_FILE":       argsFile,
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "Implemented the feature and all tests pass." {
		t.Errorf("Text = %q, want the envelope result", res.Text)
	}
	if !strings.Contains(string(res.Raw), "session_id") {
		t.Errorf("Raw should hold the full stdout envelope, got %q", res.Raw)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
}

// TestRunPropagatesModelAndPermissionMode asserts that the role-supplied model
// and permission mode reach the actual invocation: the fake agent writes its
// argv to a file, which we read back and check for --model/--permission-mode.
func TestRunPropagatesModelAndPermissionMode(t *testing.T) {
	h, err := Factory("claude", helperConfig(t))
	if err != nil {
		t.Fatalf("Factory: %v", err)
	}
	argsFile := filepath.Join(t.TempDir(), "argv.txt")
	if _, err := h.Run(context.Background(), harness.Request{
		Prompt:         "x",
		Model:          "opus",
		PermissionMode: "plan",
		Env: map[string]string{
			"GO_WANT_HELPER_PROCESS": "1",
			"HELPER_MODE":            "claude-success",
			"HELPER_ARGS_FILE":       argsFile,
		},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	argv, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("reading recorded argv: %v", err)
	}
	got := string(argv)
	for _, want := range []string{"--model\nopus", "--permission-mode\nplan"} {
		if !strings.Contains(got, want) {
			t.Errorf("argv missing %q\nfull argv:\n%s", want, got)
		}
	}
}

// TestRunSurfacesErrorEnvelope proves a claude error envelope (is_error:true)
// emitted on a clean (exit 0) run is surfaced as a Go error from Run, naming the
// harness, rather than returned as result text.
func TestRunSurfacesErrorEnvelope(t *testing.T) {
	h, err := Factory("claude", helperConfig(t))
	if err != nil {
		t.Fatalf("Factory: %v", err)
	}
	res, err := h.Run(context.Background(), harness.Request{
		Prompt: "x",
		Model:  "sonnet",
		Env: map[string]string{
			"GO_WANT_HELPER_PROCESS": "1",
			"HELPER_MODE":            "claude-error",
		},
	})
	if err == nil {
		t.Fatalf("expected an error from an error envelope, got text %q", res.Text)
	}
	if !strings.Contains(err.Error(), "claude") {
		t.Errorf("error should name the harness, got: %v", err)
	}
	if !strings.Contains(err.Error(), "error_during_execution") {
		t.Errorf("error should carry the subtype, got: %v", err)
	}
	// Raw must still be retained for logging even on an error envelope.
	if !strings.Contains(string(res.Raw), "is_error") {
		t.Errorf("Raw should hold the full error envelope, got %q", res.Raw)
	}
}

// TestRunPropagatesExecError proves that when the inner adapter fails (here, a
// non-zero exit), the preset returns that error unchanged rather than trying to
// parse stdout as an envelope.
func TestRunPropagatesExecError(t *testing.T) {
	h, err := Factory("claude", helperConfig(t))
	if err != nil {
		t.Fatalf("Factory: %v", err)
	}
	_, err = h.Run(context.Background(), harness.Request{
		Prompt: "x",
		Model:  "sonnet",
		Env: map[string]string{
			"GO_WANT_HELPER_PROCESS": "1",
			"HELPER_MODE":            "claude-fail",
		},
	})
	if err == nil {
		t.Fatal("expected an error on non-zero exit, got nil")
	}
	if !strings.Contains(err.Error(), "exited with code") {
		t.Errorf("error should come from the adapter (exit code), got: %v", err)
	}
}

// TestFactoryRejectsBadTemplate proves construction surfaces a malformed arg
// template as an error (the preset does not swallow generic-adapter validation).
func TestFactoryRejectsBadTemplate(t *testing.T) {
	cfg := helperConfig(t)
	cfg.Args = []string{"{{.Prompt"} // unterminated action
	if _, err := Factory("claude", cfg); err == nil {
		t.Fatal("expected a construction error for a bad template, got nil")
	}
}

// TestFactoriesWiring proves the registry-wiring helper binds the claude key to
// the preset Factory and, end-to-end, that NewRegistry uses the preset (not the
// generic adapter) for the claude harness. pi, which has no factory, must stay a
// generic CLI harness.
func TestFactoriesWiring(t *testing.T) {
	facs := claudeFactories(t)
	if _, ok := facs[Name]; !ok {
		t.Fatalf("Factories() missing key %q; got %v", Name, keys(facs))
	}

	reg, err := harness.NewRegistry(config.Default(), harness.Options{Factories: facs})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	got, ok := reg.Get(Name)
	if !ok {
		t.Fatalf("registry missing %q", Name)
	}
	// The registry wraps every real harness in the retry layer (AIX-0014); unwrap
	// it to reach the preset wrapper underneath.
	if _, isWrapper := unwrap(got).(*harnessWrapper); !isWrapper {
		t.Errorf("claude harness = %T, want *harnessWrapper under retry (preset wired)", got)
	}
}

// unwrap peels the registry's retry/dry-run decorators off a Harness so the test
// can assert on the preset's concrete type.
func unwrap(h harness.Harness) harness.Harness {
	for {
		u, ok := h.(interface{ Unwrap() harness.Harness })
		if !ok {
			return h
		}
		h = u.Unwrap()
	}
}

// claudeFactories is a tiny indirection so the test reads clearly; it returns the
// preset's wiring map.
func claudeFactories(t *testing.T) map[string]harness.Factory {
	t.Helper()
	return Factories()
}

// keys returns the sorted-ish keys of a factory map for error messages.
func keys(m map[string]harness.Factory) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestAvailableMissingBinary proves the preflight returns a clear, actionable
// error when claude is not on PATH. PATH is set to an empty temp dir so the
// lookup is guaranteed to miss; t.Setenv restores it automatically.
func TestAvailableMissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := Available()
	if err == nil {
		t.Fatal("expected an error when claude is absent from PATH, got nil")
	}
	if !strings.Contains(err.Error(), "claude") {
		t.Errorf("error should name the tool, got: %v", err)
	}
	if !strings.Contains(err.Error(), installHint) {
		t.Errorf("error should include the install hint %q, got: %v", installHint, err)
	}
}
