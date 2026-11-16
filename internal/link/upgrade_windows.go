//go:build windows

package link

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// spawnDetached starts the upgrade runner so that it outlives the link (plan 086 C8). The link is a service started by
// the SCM through golang.org/x/sys/windows/svc; the SCM puts an own-process service in no job object and does not end
// its children when it stops, and Go's os/exec puts the child in none either — provision.exe already relies on this
// (a command keeps running when the link stops, plan 083 A10). The flags make sure of it whatever the host does:
//
//   - DETACHED_PROCESS: no console, inherited or new (the link has none to give: session 0);
//   - CREATE_NEW_PROCESS_GROUP: no console control event meant for the link reaches it;
//   - CREATE_BREAKAWAY_FROM_JOB: should a host (an agent, a sandbox) have put the link in a job, the runner leaves it,
//     so the job's end — at the link's exit — cannot end the runner.
//
// A job that forbids breaking away refuses the flag (ERROR_ACCESS_DENIED). The runner then starts inside it when that
// job does not kill its processes on close, and otherwise the upgrade is refused before anything is run
// (runner_not_detachable) — never a runner that dies with the link half-way through the setup. Only the standard
// handles are passed (os/exec's handle list), all three NUL.
func spawnDetached(exe string, args []string, dir string) error {
	start := func(flags uint32) error {
		cmd := exec.Command(exe, args...)
		cmd.Dir = dir
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}
	base := uint32(windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP)
	err := start(base | windows.CREATE_BREAKAWAY_FROM_JOB)
	if err == nil || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return err
	}
	if !inJob() {
		return err // denied for another reason: the runner's file, say
	}
	if killOnJobClose() {
		return upgradeRefuse("runner_not_detachable", "upgrade by hand at the server",
			"this link runs in a job that ends its processes with it and forbids leaving it: the runner could not outlive the link")
	}
	return start(base)
}

var procIsProcessInJob = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")

// inJob reports whether this process is in a job object (true when it cannot tell).
func inJob() bool {
	var in int32
	r, _, _ := procIsProcessInJob.Call(uintptr(windows.CurrentProcess()), 0, uintptr(unsafe.Pointer(&in)))
	return r == 0 || in != 0
}

// killOnJobClose reports whether this process's job ends every process in it when it closes (true when it cannot tell).
func killOnJobClose() bool {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	err := windows.QueryInformationJobObject(0, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil)
	if err != nil {
		return true
	}
	return info.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE != 0
}

// runSetupSilent runs the Inno Setup installer with its silent flags and its log at logPath. The command line is
// written out whole (SysProcAttr.CmdLine), because Inno Setup reads /LOG="path" with the quotes inside the argument.
// A setup that outlives timeout is left running: killing it half-way would leave the install worse than either build.
func runSetupSilent(setup, logPath string, timeout time.Duration) (int, error) {
	cmd := exec.Command(setup)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine:    fmt.Sprintf(`"%s" %s /LOG="%s"`, setup, setupArgs, logPath),
		HideWindow: true,
	}
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exit *exec.ExitError
		if err == nil || errors.As(err, &exit) {
			return cmd.ProcessState.ExitCode(), nil
		}
		return -1, err
	case <-time.After(timeout):
		return -1, fmt.Errorf("still running after %s; left running", timeout)
	}
}
