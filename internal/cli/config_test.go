package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runCLI executes the root command with args and captured output, returning the
// combined stdout/stderr buffer and the error.
func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd(&GlobalOptions{})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	// Inject an empty, non-TTY stdin so a command with no task source fails fast
	// (no task) instead of blocking on the process's real stdin during tests.
	root.SetIn(strings.NewReader(""))
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// TestConfigShowPrintsDefaults runs `config show` with explicit non-existent
// config paths so no real files are read, and asserts the full default config is
// printed.
func TestConfigShowPrintsDefaults(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	out, err := runCLI(t, "--config", missing, "--global-config", missing, "config", "show")
	if err != nil {
		t.Fatalf("config show: %v\n%s", err, out)
	}
	for _, want := range []string{"version: 1", "harnesses:", "command: claude", "policy: read-only", "maxParallel: 4"} {
		if !strings.Contains(out, want) {
			t.Errorf("config show output missing %q\n%s", want, out)
		}
	}
}

// TestConfigPathReportsLocations runs `config path` and asserts both layer rows
// are reported with existence status.
func TestConfigPathReportsLocations(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "local.yaml")
	if err := os.WriteFile(local, []byte("version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.yaml")

	out, err := runCLI(t, "--global-config", missing, "--config", local, "config", "path")
	if err != nil {
		t.Fatalf("config path: %v\n%s", err, out)
	}
	if !strings.Contains(out, "global") || !strings.Contains(out, "missing") {
		t.Errorf("config path missing global/missing row:\n%s", out)
	}
	if !strings.Contains(out, "local") || !strings.Contains(out, "exists") {
		t.Errorf("config path missing local/exists row:\n%s", out)
	}
	if !strings.Contains(out, local) {
		t.Errorf("config path did not print local path %q:\n%s", local, out)
	}
}

// TestConfigInitWritesAndRefuses runs `config init` in a temp working dir,
// confirms the file is written, and that a second run refuses without --force.
func TestConfigInitWritesAndRefuses(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir) // config init writes to the process working directory.

	out, err := runCLI(t, "config", "init")
	if err != nil {
		t.Fatalf("config init: %v\n%s", err, out)
	}
	target := filepath.Join(dir, ".aixecutor", "config.yaml")
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("config init did not create %s: %v", target, err)
	}
	if !strings.Contains(out, "wrote") {
		t.Errorf("config init output missing confirmation:\n%s", out)
	}

	// Second run without --force should error.
	_, err = runCLI(t, "config", "init")
	if err == nil {
		t.Fatal("second config init should refuse without --force")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error %q should mention the file already exists", err.Error())
	}

	// With --force it succeeds.
	if _, err := runCLI(t, "config", "init", "--force"); err != nil {
		t.Fatalf("config init --force: %v", err)
	}
}

// TestConfigInvalidFileSurfacesError ensures a semantically invalid config file
// produces an actionable error via the CLI.
func TestConfigInvalidFileSurfacesError(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("git:\n  policy: nonsense\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.yaml")
	_, err := runCLI(t, "--global-config", missing, "--config", bad, "config", "show")
	if err == nil {
		t.Fatal("expected error for invalid git.policy")
	}
	if !strings.Contains(err.Error(), "git.policy") {
		t.Errorf("error %q should mention git.policy", err.Error())
	}
}
