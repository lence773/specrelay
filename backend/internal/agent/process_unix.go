//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

// configureProcess puts every local CLI invocation in its own process group.
// This lets shutdown stop the CLI and anything it spawned without touching
// unrelated host processes.
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateProcessTree(cmd *exec.Cmd, force bool) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	return syscall.Kill(-cmd.Process.Pid, signal)
}
