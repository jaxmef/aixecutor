//go:build !unix

package harness

import "os/exec"

// TODO(windows): process-group semantics differ on Windows. Setpgid is not
// available; isolating and group-killing a subprocess tree needs a job object
// (CREATE_NEW_PROCESS_GROUP / AssignProcessToJobObject) instead. The ticket
// targets Unix first, so this fallback merely kills the direct child and does
// not guarantee descendants are reaped. Implement proper job-object teardown
// before claiming Windows support.

// setProcessGroup is a no-op on non-Unix platforms (see the TODO above).
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup kills only the direct child on non-Unix platforms.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
