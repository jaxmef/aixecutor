package harness

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"
)

// execRunner is the production runnerFunc: it runs cmd as a real subprocess,
// feeds it stdin, and captures stdout/stderr separately. On context-deadline
// expiry it kills the whole process group (so a misbehaving agent that spawns
// children is fully torn down) and reports timedOut, with the elapsed time on
// runResult so the caller can build a descriptive error.
//
// The command is launched in its own process group via setProcessGroup (Unix);
// see the // TODO(windows) note there — group semantics differ on Windows and
// the ticket targets Unix first.
func execRunner(ctx context.Context, cmd *exec.Cmd, stdin []byte) (runResult, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	setProcessGroup(cmd)

	start := time.Now()
	if err := cmd.Start(); err != nil {
		// Failed before the process existed (e.g. command not found). No exit
		// code is meaningful here.
		return runResult{
			stdout:   stdout.Bytes(),
			stderr:   stderr.Bytes(),
			exitCode: -1,
			duration: time.Since(start),
		}, err
	}

	// Wait in a goroutine so we can race the wait against ctx cancellation and
	// guarantee the whole process group is killed on deadline.
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		// Deadline (or cancellation) hit: kill the entire group, then reap.
		killProcessGroup(cmd)
		<-waitErr
		return runResult{
			stdout:   stdout.Bytes(),
			stderr:   stderr.Bytes(),
			exitCode: -1,
			duration: time.Since(start),
			timedOut: true,
		}, nil
	case err := <-waitErr:
		rr := runResult{
			stdout:   stdout.Bytes(),
			stderr:   stderr.Bytes(),
			exitCode: 0,
			duration: time.Since(start),
		}
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				// Normal non-zero exit: surface the code, not a Go error, so the
				// caller can build a clean "exited with code N" message.
				rr.exitCode = ee.ExitCode()
				return rr, nil
			}
			rr.exitCode = -1
			return rr, err
		}
		return rr, nil
	}
}
