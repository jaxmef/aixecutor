package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runCLIExit runs the root command and returns the error and the process exit code
// Execute would produce (via exitCodeFor), so error-UX tests can assert on both the
// stable classified code and the actionable message. The command silences its own
// error printing (SilenceErrors), so the message lives on the returned error.
func runCLIExit(t *testing.T, args ...string) (error, int) {
	t.Helper()
	root := newRootCmd(&GlobalOptions{})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader("")) // never block on real stdin during tests
	root.SetArgs(args)
	err := root.Execute()
	return err, exitCodeFor(err)
}

func TestExitCodeHelpers(t *testing.T) {
	if exitCodeFor(nil) != exitOK {
		t.Errorf("nil error → %d, want %d", exitCodeFor(nil), exitOK)
	}
	if exitCodeFor(errors.New("plain")) != exitGeneric {
		t.Errorf("unclassified error → %d, want %d", exitCodeFor(errors.New("plain")), exitGeneric)
	}
	wrapped := withExit(exitConfig, errors.New("bad config"))
	if exitCodeFor(wrapped) != exitConfig {
		t.Errorf("wrapped → %d, want %d", exitCodeFor(wrapped), exitConfig)
	}
	// First classification wins: re-wrapping does not change the code.
	rewrapped := withExit(exitNotFound, wrapped)
	if exitCodeFor(rewrapped) != exitConfig {
		t.Errorf("re-wrapped → %d, want first code %d", exitCodeFor(rewrapped), exitConfig)
	}
	// The message is preserved verbatim.
	if wrapped.Error() != "bad config" {
		t.Errorf("wrapped message = %q, want %q", wrapped.Error(), "bad config")
	}
	// nil stays nil.
	if withExit(exitConfig, nil) != nil {
		t.Errorf("withExit(_, nil) should be nil")
	}
}

// TestStatusUnknownRunExitCode proves an unknown run id maps to the "not found"
// exit code with an actionable message.
func TestStatusUnknownRunExitCode(t *testing.T) {
	t.Chdir(t.TempDir())
	err, code := runCLIExit(t, missingConfigArgs(t, "status", "no-such-run")...)
	if code != exitNotFound {
		t.Errorf("exit code = %d, want %d (not found): %v", code, exitNotFound, err)
	}
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("no run")) {
		t.Errorf("expected an actionable 'no run' message, got: %v", err)
	}
}

// TestMissingBinaryExitCode proves a missing harness binary maps to the
// missing-binary exit code with an actionable install hint. PATH is emptied so the
// preset preflight is guaranteed to miss.
func TestMissingBinaryExitCode(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no binaries resolvable
	err := preflightHarness("claude")
	if err == nil {
		t.Fatal("expected a missing-binary error when claude is not on PATH")
	}
	if exitCodeFor(err) != exitMissingBinary {
		t.Errorf("exit code = %d, want %d (missing binary)", exitCodeFor(err), exitMissingBinary)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("PATH")) {
		t.Errorf("missing-binary error should be actionable (mention PATH): %v", err)
	}
	// An unknown harness has no preset preflight, so it never fails here.
	if err := preflightHarness("some-generic-harness"); err != nil {
		t.Errorf("unknown harness preflight should be nil, got: %v", err)
	}
}

// TestBadConfigExitCode proves an invalid config file maps to the config exit code.
func TestBadConfigExitCode(t *testing.T) {
	t.Chdir(t.TempDir())
	// Point the local config at a file with an unknown top-level key, which the
	// strict loader rejects.
	bad := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(bad, []byte("bogusTopLevelKey: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missingGlobal := filepath.Join(t.TempDir(), "nope.yaml")
	err, code := runCLIExit(t, "--global-config", missingGlobal, "--config", bad, "config", "show")
	if code != exitConfig {
		t.Errorf("exit code = %d, want %d (config): %v", code, exitConfig, err)
	}
}
