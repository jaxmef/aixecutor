package git

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
)

// execRunner is the production runnerFunc: it runs `git <args...>` as a real
// subprocess in dir, capturing stdout and stderr separately. It does not enforce
// the allowlist — that is the gateway's read method's job, applied before this
// runner is ever called. For worktree add/remove (the sole mutating exception),
// the WorktreeManager calls this runner directly, behind its own policy gate.
//
// On a normal non-zero exit, exec.Cmd.Run returns an *exec.ExitError; we return
// it as the error and also hand back the captured streams so callers can build
// actionable messages and, where relevant (git diff --no-index), inspect the
// exit code. The git binary is taken from PATH.
func execRunner(ctx context.Context, dir string, args ...string) (stdout, stderr []byte, err error) {
	var outBuf, errBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.Bytes(), errBuf.Bytes(), err
}

// exitCode extracts the process exit code from an error returned by the runner.
// It returns (code, true) for a normal non-zero exit (an *exec.ExitError), and
// (0, false) otherwise (nil error, or a failure-to-start where no exit code is
// meaningful). It is used by the diff engine, where `git diff --no-index` exits
// 1 to mean "differences found" — a success for our purposes.
func exitCode(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), true
	}
	return 0, false
}
