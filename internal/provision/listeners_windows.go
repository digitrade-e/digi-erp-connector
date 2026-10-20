//go:build windows

package provision

import (
	"errors"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modIphlpapi             = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedTcpTable = modIphlpapi.NewProc("GetExtendedTcpTable")
)

const (
	tcpTableOwnerPIDListener = 3   // TCP_TABLE_OWNER_PID_LISTENER
	errInsufficientBuffer    = 122 // ERROR_INSUFFICIENT_BUFFER
)

// Listeners reads the listening TCP sockets and their processes from the TCP
// table (GetExtendedTcpTable), IPv4 and IPv6 — no netstat to parse, nothing
// localised.
func (winSystem) Listeners() ([]Listener, error) {
	var out []Listener
	for _, family := range []uint32{afInet, afInet6} {
		rows, err := tcpListeners(family)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	names := map[uint32]string{}
	for i := range out {
		pid := out[i].PID
		if _, ok := names[pid]; !ok {
			names[pid] = processName(pid)
		}
		out[i].Process = names[pid]
	}
	return out, nil
}

func tcpListeners(family uint32) ([]Listener, error) {
	var size uint32
	var buf []byte
	for attempt := 0; attempt < 5; attempt++ {
		var p uintptr
		if len(buf) > 0 {
			p = uintptr(unsafe.Pointer(&buf[0]))
		}
		r, _, _ := procGetExtendedTcpTable.Call(p, uintptr(unsafe.Pointer(&size)), 0,
			uintptr(family), tcpTableOwnerPIDListener, 0)
		switch r {
		case 0:
			return parseTCPTable(buf, family)
		case errInsufficientBuffer:
			buf = make([]byte, size)
		default:
			return nil, syscall.Errno(r)
		}
	}
	return nil, errors.New("the TCP table kept growing")
}

func processName(pid uint32) string {
	switch pid {
	case 0:
		return "Idle"
	case 4:
		return "System"
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return ""
	}
	return filepath.Base(windows.UTF16ToString(buf[:size]))
}
