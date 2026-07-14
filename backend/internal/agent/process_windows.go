//go:build windows

package agent

import (
	"fmt"
	"os/exec"
	"syscall"
)

// Windows does not support Unix process groups. taskkill /T terminates the
// spawned CLI process together with its descendants, which is the equivalent
// safety boundary for a desktop-owned agent run. HideWindow also prevents
// .cmd-based local CLIs from flashing a console window during execution.
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

func terminateProcessTree(cmd *exec.Cmd, force bool) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	args := []string{"/PID", fmt.Sprint(cmd.Process.Pid), "/T"}
	if force {
		args = append(args, "/F")
	}
	taskkill := exec.Command("taskkill", args...)
	taskkill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return taskkill.Run()
}
