//go:build windows

package agent

import (
	"fmt"
	"os/exec"
)

// Windows does not support Unix process groups. taskkill /T terminates the
// spawned CLI process together with its descendants, which is the equivalent
// safety boundary for a desktop-owned agent run.
func configureProcess(_ *exec.Cmd) {}

func terminateProcessTree(cmd *exec.Cmd, force bool) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	args := []string{"/PID", fmt.Sprint(cmd.Process.Pid), "/T"}
	if force {
		args = append(args, "/F")
	}
	return exec.Command("taskkill", args...).Run()
}
