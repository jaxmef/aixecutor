package git

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// recordingRunner is an injectable runnerFunc that records every git invocation
// it is asked to make and returns canned output/error, WITHOUT ever executing
// git. It is the workhorse for hermetic tests: per CLAUDE.md §7, no test may run
// a mutating git command, so worktree/command-surface tests use this to assert
// "git would have been called with these args" rather than actually calling git.
type recordingRunner struct {
	// calls accumulates each invocation's args (the git subcommand and flags).
	calls [][]string
	// stdout/stderr are returned for every call.
	stdout []byte
	stderr []byte
	// err, if set, is returned for every call.
	err error
	// failOn, if non-nil, returns failErr for the Nth matching call to let tests
	// simulate a mid-sequence failure (e.g. one worktree remove failing).
	failOn  func(args []string) bool
	failErr error
}

func (r *recordingRunner) run(_ context.Context, _ string, args ...string) (stdout, stderr []byte, err error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if r.failOn != nil && r.failOn(args) {
		return nil, r.stderr, r.failErr
	}
	return r.stdout, r.stderr, r.err
}

// callArgs joins a recorded call into a single space-separated string for easy
// assertions.
func callArgs(call []string) string { return strings.Join(call, " ") }

// TestReadAllowlistRefusesMutatingCommands is acceptance criterion 1: the
// gateway refuses every non-allowlisted subcommand with a clear error and does
// so WITHOUT executing git (the refusal happens before the runner is called). We
// prove "without executing git" by injecting a runner that fails the test if it
// is ever invoked.
func TestReadAllowlistRefusesMutatingCommands(t *testing.T) {
	refused := []string{
		// mutating / dangerous commands the invariant forbids
		"commit", "push", "add", "rm", "reset", "stash", "merge", "rebase",
		"checkout", "tag", "apply", "clean", "restore", "switch",
		"fetch", "pull", "clone", "init",
		// worktree must NOT be reachable through the read path: it is only allowed
		// via a policy-gated WorktreeManager, never as a generic read command.
		"worktree",
	}

	for _, cmd := range refused {
		t.Run(cmd, func(t *testing.T) {
			// A runner that explodes if called, proving refusal precedes exec.
			explode := func(context.Context, string, ...string) ([]byte, []byte, error) {
				t.Fatalf("runner was invoked for refused command %q; refusal must happen before exec", cmd)
				return nil, nil, nil
			}
			g := newGatewayWithRunner("/repo", explode)

			_, err := g.read(context.Background(), cmd, "--some-flag")
			if err == nil {
				t.Fatalf("read(%q) = nil error; want refusal", cmd)
			}
			want := fmt.Sprintf("command %q is not permitted", cmd)
			if !strings.Contains(err.Error(), want) {
				t.Errorf("read(%q) error = %q; want it to contain %q", cmd, err.Error(), want)
			}
			if !strings.Contains(err.Error(), "read-only gateway") {
				t.Errorf("read(%q) error = %q; want it to mention the read-only gateway", cmd, err.Error())
			}
		})
	}
}

// TestReadAllowlistPermitsReadCommands confirms each allowlisted read command is
// permitted (the runner IS invoked) and that the gateway runs it in repoRoot.
func TestReadAllowlistPermitsReadCommands(t *testing.T) {
	for cmd := range allowedReadCmds {
		t.Run(cmd, func(t *testing.T) {
			rr := &recordingRunner{stdout: []byte("ok")}
			g := newGatewayWithRunner("/repo", rr.run)
			out, err := g.read(context.Background(), cmd, "--flag")
			if err != nil {
				t.Fatalf("read(%q): unexpected error %v", cmd, err)
			}
			if string(out) != "ok" {
				t.Errorf("read(%q) stdout = %q; want %q", cmd, out, "ok")
			}
			if len(rr.calls) != 1 {
				t.Fatalf("read(%q): runner called %d times; want 1", cmd, len(rr.calls))
			}
			if got := callArgs(rr.calls[0]); got != cmd+" --flag" {
				t.Errorf("read(%q): runner args = %q; want %q", cmd, got, cmd+" --flag")
			}
		})
	}
}

// TestReadEmptyArgsRefused covers the empty-args guard.
func TestReadEmptyArgsRefused(t *testing.T) {
	g := newGatewayWithRunner("/repo", func(context.Context, string, ...string) ([]byte, []byte, error) {
		t.Fatal("runner must not be called for empty args")
		return nil, nil, nil
	})
	if _, err := g.read(context.Background()); err == nil {
		t.Fatal("read() with no args = nil error; want refusal")
	}
}

// TestReadWrapsRunnerError checks that a git failure surfaces an actionable error
// including the subcommand and a tail of stderr.
func TestReadWrapsRunnerError(t *testing.T) {
	sentinel := errors.New("boom")
	rr := &recordingRunner{stderr: []byte("fatal: not a thing"), err: sentinel}
	g := newGatewayWithRunner("/repo", rr.run)
	_, err := g.read(context.Background(), "status", "--porcelain")
	if err == nil {
		t.Fatal("want error")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error %v does not wrap sentinel", err)
	}
	if !strings.Contains(err.Error(), "status --porcelain") {
		t.Errorf("error %q should name the subcommand", err.Error())
	}
	if !strings.Contains(err.Error(), "fatal: not a thing") {
		t.Errorf("error %q should include stderr tail", err.Error())
	}
}

// TestStatusAndLsFilesHelpersRouteThroughRead verifies the public read helpers
// issue the expected git commands and parse NUL-delimited output.
func TestStatusAndLsFilesHelpersRouteThroughRead(t *testing.T) {
	t.Run("TrackedFiles", func(t *testing.T) {
		rr := &recordingRunner{stdout: []byte("a.go\x00dir/b.go\x00")}
		g := newGatewayWithRunner("/repo", rr.run)
		got, err := g.TrackedFiles(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(got, ",") != "a.go,dir/b.go" {
			t.Errorf("tracked = %v", got)
		}
		if callArgs(rr.calls[0]) != "ls-files -z" {
			t.Errorf("args = %q", callArgs(rr.calls[0]))
		}
	})

	t.Run("UntrackedFiles", func(t *testing.T) {
		rr := &recordingRunner{stdout: []byte("new.txt\x00")}
		g := newGatewayWithRunner("/repo", rr.run)
		got, err := g.UntrackedFiles(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(got, ",") != "new.txt" {
			t.Errorf("untracked = %v", got)
		}
		if callArgs(rr.calls[0]) != "ls-files --others --exclude-standard -z" {
			t.Errorf("args = %q", callArgs(rr.calls[0]))
		}
	})
}

// TestSplitNUL covers the NUL splitter edge cases directly.
func TestSplitNUL(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a\x00", []string{"a"}},
		{"a\x00b\x00", []string{"a", "b"}},
		{"a\x00\x00b", []string{"a", "b"}}, // empty segments dropped
	}
	for _, c := range cases {
		got := splitNUL([]byte(c.in))
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("splitNUL(%q) = %v; want %v", c.in, got, c.want)
		}
	}
}
