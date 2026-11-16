//go:build windows

package link

import (
	"errors"
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
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}

// killProcess ends a process at once (TerminateProcess: no chance to save).
func killProcess(pid uint32) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	return windows.TerminateProcess(h, 1)
}
