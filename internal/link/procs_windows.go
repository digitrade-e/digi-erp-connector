//go:build windows

package link

import (
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// listProcesses lists the processes whose executable is named name (any case) in every session, with their full image
// path and session: a Toolhelp snapshot, then QueryFullProcessImageName. LocalSystem may open a user's process to
// query and to end it (its default DACL grants SYSTEM full access). A process that cannot be opened has no image
// path, so it is never taken for the installed connector.
func listProcesses(name string) ([]RunningProcess, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = windows.CloseHandle(snap) }()
	var out []RunningProcess
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if !strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), name) {
			continue
		}
		p := RunningProcess{PID: pe.ProcessID, Image: imagePath(pe.ProcessID)}
		_ = windows.ProcessIdToSessionId(pe.ProcessID, &p.Session)
		out = append(out, p)
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return out, err
	}
	return out, nil
}

// imagePath is a process's executable as a Win32 path, "" when it cannot be read.
func imagePath(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer func() { _ = windows.CloseHandle(h) }()
	img, _ := imageOf(h)
	return img
}

// imageOf is the executable of the process h names, as a Win32 path.
func imageOf(h windows.Handle) (string, error) {
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:n]), nil
}

// killProcess ends process pid at once (TerminateProcess: no chance to save) — only when its executable is image. One
// handle does both: the image is read through the handle that ends it, so a PID that another process took meanwhile is
// never ended (review m5: the image was read on one handle and the process ended by PID on another).
func killProcess(pid uint32, image string) error {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	got, err := imageOf(h)
	if err != nil {
		return fmt.Errorf("process %d: its image cannot be read, so it is not ended: %w", pid, err)
	}
	if !samePath(got, image) {
		return fmt.Errorf("%w: process %d is %s now", errNotThatProcess, pid, got)
	}
	return windows.TerminateProcess(h, 1)
}
