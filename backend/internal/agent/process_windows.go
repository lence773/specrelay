//go:build windows

package agent

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
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

// InspectProcess queries Windows without signalling or opening the process for
// mutation. The creation-time token distinguishes the original CLI from a
// later process that happens to reuse the same PID.
func InspectProcess(pid int) (ProcessEvidence, error) {
	if pid <= 0 {
		return ProcessEvidence{}, nil
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return ProcessEvidence{Running: false}, nil
		}
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return ProcessEvidence{Running: true}, nil
		}
		return ProcessEvidence{}, err
	}
	defer windows.CloseHandle(handle)
	var exitCode uint32
	if err = windows.GetExitCodeProcess(handle, &exitCode); err != nil {
		return ProcessEvidence{}, err
	}
	const stillActive = 259
	if exitCode != stillActive {
		return ProcessEvidence{Running: false}, nil
	}
	var created, exited, kernel, user windows.Filetime
	if err = windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return ProcessEvidence{Running: true}, nil
	}
	return ProcessEvidence{Running: true, Identity: fmt.Sprintf("windows-created:%d", created.Nanoseconds())}, nil
}
