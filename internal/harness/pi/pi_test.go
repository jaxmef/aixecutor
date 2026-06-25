package pi

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

// testdataPath resolves a file under the repo-root testdata/harness/pi dir. The
// package lives at internal/harness/pi, so testdata is three levels up.
func testdataPath(name string) string {
	return filepath.Join("..", "..", "..", "testdata", "harness", "pi", name)
}

// readTestdata reads a recorded pi text sample, failing the test on error.
func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(testdataPath(name))
	if err != nil {
		t.Fatalf("reading testdata %q: %v", name, err)
	}
	return data
}

// helperConfig builds a config.Harness whose command re-execs THIS test binary
// into TestHelperProcess (the standard Go exec-test pattern), so the preset's Run
// spawns a fake "pi" that emits canned text to stdout and records the argv it
// received. The args mirror the real default pi block so {{.Model}} and the
// positional {{.Prompt}} are templated through exactly as in prod.
func helperConfig(t *testing.T) config.Harness {
	t.Helper()
	return config.Harness{
		Type:           "cli",
		Command:        os.Args[0],
		PromptDelivery: "arg",
		Args: []string{
			"-test.run=TestHelperProcess",
			"--",
			"--print",
			"--model",
			"{{.Model}}",
			"{{.Prompt}}",
		},
		Output:  "text",
		Timeout: config.Duration(30 * time.Second),
		Env:     map[string]string{},
	}
}

// TestRunTrimsTextAndPreservesRaw drives the preset's full Run through a real
// subprocess (no real pi): the fake agent emits the recorded result text with a
// trailing newline, and Run must return the trimmed text in Result.Text while
// Result.Raw retains the untrimmed stdout verbatim.
func TestRunTrimsTextAndPreservesRaw(t *testing.T) {
	h, err := Factory("pi", helperConfig(t))
	if err != nil {
		t.Fatalf("Factory: %v", err)
	}
	if h.Name() != "pi" {
		t.Errorf("Name = %q, want pi", h.Name())
	}

	res, err := h.Run(context.Background(), harness.Request{
		Prompt: "do the thing",
		Model:  "gemini-2.5-pro",
		Env: map[string]string{
			"GO_WANT_HELPER_PROCESS": "1",
			"HELPER_MODE":            "pi-success",
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The recorded fixture is the canonical "what pi printed"; the fake helper
	// emits the same text (plus a trailing newline). Result.Text must be the
	// trimmed form; Result.Raw must be the untrimmed stdout.
	wantTrimmed := strings.TrimRight(string(readTestdata(t, "success.txt")), " \t\r\n")
	if res.Text != wantTrimmed {
		t.Errorf("Text = %q, want trimmed %q", res.Text, wantTrimmed)
	}
	if !strings.HasSuffix(string(res.Raw), "\n") {
		t.Errorf("Raw should preserve the trailing newline, got %q", res.Raw)
	}
	if strings.TrimRight(string(res.Raw), " \t\r\n") != wantTrimmed {
		t.Errorf("Raw body = %q, want %q (verbatim stdout)", res.Raw, wantTrimmed)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
}

// TestRunDeliversPromptPositionallyWithModel asserts the verified contract: the
// rendered invocation passes --print, --model <model>, and the prompt as a
// POSITIONAL argument (not stdin, no --prompt flag). The fake agent writes its
// argv to a file, which we read back and check positions on.
func TestRunDeliversPromptPositionallyWithModel(t *testing.T) {
	h, err := Factory("pi", helperConfig(t))
	if err != nil {
		t.Fatalf("Factory: %v", err)
	}
	argsFile := filepath.Join(t.TempDir(), "argv.txt")
	if _, err := h.Run(context.Background(), harness.Request{
		Prompt: "list all .ts files",
		Model:  "gemini-2.5-pro",
		Env: map[string]string{
			"GO_WANT_HELPER_PROCESS": "1",
			"HELPER_MODE":            "pi-success",
			"HELPER_ARGS_FILE":       argsFile,
		},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	argv, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("reading recorded argv: %v", err)
	}
	// The helper writes one arg per line; reconstruct the rendered argv slice.
	got := splitNonEmptyLines(string(argv))

	// --print must be present (headless), and --model must be immediately
	// followed by the model value.
	assertContainsAdjacent(t, got, "--print")
	assertPairFollows(t, got, "--model", "gemini-2.5-pro")

	// The prompt must appear as a standalone positional arg — i.e. NOT introduced
	// by a flag like --prompt/-p. It is the last templated arg in the default
	// block, so assert it is the final element and is the verbatim prompt.
	if len(got) == 0 || got[len(got)-1] != "list all .ts files" {
		t.Errorf("prompt should be the final positional arg, got argv: %v", got)
	}
	// Defense against a regression to flag-delivered prompts: no --prompt flag.
	for _, a := range got {
		if a == "--prompt" || a == "-p" {
			// -p is pi's alias for --print; the default uses --print, so seeing
			// -p here would mean a drift. We only flag the prompt-flag case.
			if a == "--prompt" {
				t.Errorf("prompt must be positional, but found a --prompt flag in argv: %v", got)
			}
		}
	}
}

// TestRunPropagatesExecError proves that when the inner adapter fails (here, a
// non-zero exit), the preset returns that error unchanged rather than fabricating
// a result; Result.Raw is still available for logging.
func TestRunPropagatesExecError(t *testing.T) {
	h, err := Factory("pi", helperConfig(t))
	if err != nil {
		t.Fatalf("Factory: %v", err)
	}
	_, err = h.Run(context.Background(), harness.Request{
		Prompt: "x",
		Model:  "gemini-2.5-pro",
		Env: map[string]string{
			"GO_WANT_HELPER_PROCESS": "1",
			"HELPER_MODE":            "pi-fail",
		},
	})
	if err == nil {
		t.Fatal("expected an error on non-zero exit, got nil")
	}
	if !strings.Contains(err.Error(), "exited with code") {
		t.Errorf("error should come from the adapter (exit code), got: %v", err)
	}
	if !strings.Contains(err.Error(), "pi") {
		t.Errorf("error should name the harness, got: %v", err)
	}
}

// TestFactoryRejectsBadTemplate proves construction surfaces a malformed arg
// template as an error (the preset does not swallow generic-adapter validation).
func TestFactoryRejectsBadTemplate(t *testing.T) {
	cfg := helperConfig(t)
	cfg.Args = []string{"{{.Prompt"} // unterminated action
	if _, err := Factory("pi", cfg); err == nil {
		t.Fatal("expected a construction error for a bad template, got nil")
	}
}

// TestFactoriesWiring proves the registry-wiring helper binds the pi key to the
// preset Factory and, end-to-end, that NewRegistry uses the preset (not the
// generic adapter) for the pi harness built from the real default config.
func TestFactoriesWiring(t *testing.T) {
	facs := Factories()
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
		t.Errorf("pi harness = %T, want *harnessWrapper under retry (preset wired)", got)
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

// TestAvailableMissingBinary proves the preflight returns a clear, actionable
// error when pi is not on PATH. PATH is set to an empty temp dir so the lookup is
// guaranteed to miss; t.Setenv restores it automatically.
func TestAvailableMissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := Available()
	if err == nil {
		t.Fatal("expected an error when pi is absent from PATH, got nil")
	}
	if !strings.Contains(err.Error(), "pi") {
		t.Errorf("error should name the tool, got: %v", err)
	}
	if !strings.Contains(err.Error(), installHint) {
		t.Errorf("error should include the install hint %q, got: %v", installHint, err)
	}
}

// splitNonEmptyLines splits the recorded argv file into its non-empty lines (one
// rendered arg per line).
func splitNonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// assertContainsAdjacent fails unless want appears as an element of argv.
func assertContainsAdjacent(t *testing.T, argv []string, want string) {
	t.Helper()
	for _, a := range argv {
		if a == want {
			return
		}
	}
	t.Errorf("argv missing %q\nfull argv: %v", want, argv)
}

// assertPairFollows fails unless flag appears immediately before value in argv.
func assertPairFollows(t *testing.T, argv []string, flag, value string) {
	t.Helper()
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag {
			if argv[i+1] == value {
				return
			}
			t.Errorf("%q followed by %q, want %q\nfull argv: %v", flag, argv[i+1], value, argv)
			return
		}
	}
	t.Errorf("argv missing flag %q\nfull argv: %v", flag, argv)
}

// keys returns the keys of a factory map for error messages.
func keys(m map[string]harness.Factory) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
