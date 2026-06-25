package cli

import (
	"bytes"
	"sort"
	"strings"
	"testing"
)

// TestSubcommandsRegistered asserts the root command wires up every subcommand
// the CLI is expected to expose. If a subcommand is added or removed, update
// this list deliberately.
func TestSubcommandsRegistered(t *testing.T) {
	root := newRootCmd(&GlobalOptions{})

	got := make([]string, 0, len(root.Commands()))
	for _, c := range root.Commands() {
		got = append(got, c.Name())
	}
	sort.Strings(got)

	want := []string{"amend", "backlog", "config", "list", "plan", "resume", "review", "run", "status", "version"}

	if len(got) != len(want) {
		t.Fatalf("subcommand count = %d (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("subcommand[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestGlobalFlagsRegistered ensures every persistent global flag is wired onto
// the root command so later tickets can rely on them being parsed.
func TestGlobalFlagsRegistered(t *testing.T) {
	root := newRootCmd(&GlobalOptions{})

	for _, name := range []string{"config", "global-config", "docs-path", "dry-run", "verbose"} {
		if root.PersistentFlags().Lookup(name) == nil {
			t.Errorf("persistent flag --%s not registered", name)
		}
	}
}

// TestVersionCommandWritesOutput runs the version subcommand and asserts it
// writes a non-empty line containing the program name.
func TestVersionCommandWritesOutput(t *testing.T) {
	root := newRootCmd(&GlobalOptions{})

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"version"})

	if err := root.Execute(); err != nil {
		t.Fatalf("version command returned error: %v", err)
	}

	got := strings.TrimSpace(out.String())
	if got == "" {
		t.Fatal("version command wrote empty output")
	}
	if !strings.Contains(got, "aixecutor") {
		t.Errorf("version output %q does not mention %q", got, "aixecutor")
	}
}

// TestDryRunFlagParsedToOptions confirms a global flag actually lands on the
// shared GlobalOptions before the command runs. It is exercised through `run` in a
// NON-git temp dir so the command errors fast (at git.Open) without executing a
// real pipeline; flag parsing happens before RunE, so opts is populated regardless.
func TestDryRunFlagParsedToOptions(t *testing.T) {
	t.Chdir(t.TempDir()) // non-git dir → run errors before doing any real work.

	opts := &GlobalOptions{}
	root := newRootCmd(opts)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	missing := t.TempDir() + "/nope.yaml"
	root.SetArgs([]string{"--config", missing, "--global-config", missing, "--dry-run", "--verbose", "run", "x"})

	// run errors (not a git repo); we only care that parsing populated opts.
	_ = root.Execute()

	if !opts.DryRun {
		t.Error("--dry-run did not set GlobalOptions.DryRun")
	}
	if !opts.Verbose {
		t.Error("--verbose did not set GlobalOptions.Verbose")
	}
}
