//go:build !windows

package agent

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
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

// InspectProcess returns host evidence for a persisted CLI PID. Signal 0 does
// not modify the process; EPERM still proves that the PID exists. On Linux the
// /proc start-time token also protects recovery from mistaking a reused PID for
// the original CLI process. Other Unix hosts safely fall back to PID liveness.
func InspectProcess(pid int) (ProcessEvidence, error) {
	if pid <= 0 {
		return ProcessEvidence{}, nil
	}
	if err := syscall.Kill(pid, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return ProcessEvidence{Running: false}, nil
		}
		if !errors.Is(err, syscall.EPERM) {
			return ProcessEvidence{}, err
		}
	}
	evidence := ProcessEvidence{Running: true}
	if raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		// The second field is parenthesized and may contain spaces. Fields after
		// the final ") " begin at stat field 3; process start time is field 22.
		if end := strings.LastIndex(string(raw), ") "); end >= 0 {
			fields := strings.Fields(string(raw)[end+2:])
			if len(fields) > 19 {
				evidence.Identity = "linux-start:" + fields[19]
			}
			if len(fields) > 0 && (fields[0] == "Z" || fields[0] == "X") {
				evidence.Running = false
			}
		}
	}
	return evidence, nil
}
