//go:build unix

package harness

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group (Setpgid) so that, on
// a timeout, we can signal the entire group — the agent plus any helper
// processes it spawned — rather than leaking children. The child's PID becomes
// the process-group ID (PGID).
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup SIGKILLs the whole process group started by setProcessGroup.
// Negating the PID targets the group (kill(2): a negative pid signals every
// process in that group). We fall back to killing just the process if the group
// signal fails (e.g. the child already exited).
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
