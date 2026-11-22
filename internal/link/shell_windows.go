//go:build windows

package link

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// defaultShellExec runs a script with Windows PowerShell 5.1 (plan 094) as the link's own account.
//
// The script is written to a file in workDir (inside the data directory, whose DACL lets only SYSTEM and the
// administrators write) and run with -File: -EncodedCommand would carry it on the command line, whose 32 767
// characters hold a UTF-16, base64 script of about 12 KiB only. The file starts with a UTF-8 byte-order mark —
// PowerShell 5.1 reads a file without one in the system code page — and is deleted after the run.
//
// The process is put in a job object that ends every process in it when the job's handle closes: at the timeout, and
// after the script ends, so nothing it started in the background outlives it. A process it starts in the instant
// before the assignment escapes the job, and a scheduled task or a service it registers is not a process of the job:
// both are what the journal is for.
var defaultShellExec ShellExecFunc = func(ctx context.Context, workDir string, script []byte) ShellOutput {
	path, err := writeScript(workDir, script)
	if err != nil {
		return ShellOutput{ExitCode: -1, Error: "the script could not be written: " + err.Error()}
	}
	defer func() { _ = os.Remove(path) }()

	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return ShellOutput{ExitCode: -1, Error: "the system folder: " + err.Error()}
	}
	cmd := exec.Command(filepath.Join(sys, `WindowsPowerShell\v1.0\powershell.exe`),
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", path)
	cmd.Dir = workDir
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	var stdout, stderr limited
	stdout.max, stderr.max = ShellOutputCap+1, ShellOutputCap+1
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	job, err := killOnCloseJob()
	if err != nil {
		return ShellOutput{ExitCode: -1, Error: "the job object: " + err.Error()}
	}
	var once sync.Once // a handle closed twice could close another one that reused its value
	closeJob := func() { once.Do(func() { _ = windows.CloseHandle(job) }) }
	if err := cmd.Start(); err != nil {
		closeJob()
		return ShellOutput{ExitCode: -1, Error: "PowerShell did not start: " + err.Error()}
	}
	if h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid)); err == nil {
		_ = windows.AssignProcessToJobObject(job, h)
		_ = windows.CloseHandle(h)
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			closeJob()             // the timeout: the whole tree ends
			_ = cmd.Process.Kill() // and PowerShell itself, should the assignment to the job have failed
		case <-done:
		}
	}()
	err = cmd.Wait()
	close(done)
	closeJob() // whatever the script left running ends with it

	out := ShellOutput{Stdout: stdout.b.Bytes(), Stderr: stderr.b.Bytes()}
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		out.ExitCode = exit.ExitCode()
	case err != nil:
		out.ExitCode, out.Error = -1, err.Error()
	}
	return out
}

func writeScript(dir string, script []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "run-"+hex.EncodeToString(id[:])+".ps1")
	bom := []byte{0xEF, 0xBB, 0xBF}
	return path, os.WriteFile(path, append(bom, script...), 0o600)
}

func killOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}
