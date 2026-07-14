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
const createNoWindow = 0x08000000

func configureProcess(cmd *exec.Cmd) {
	// HideWindow only hides the initial window. CREATE_NO_WINDOW also prevents
	// Windows from allocating a console for .cmd-based local CLIs, which avoids
	// a visible black window even when the child starts a console executable.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
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
	taskkill.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
	return taskkill.Run()
}
