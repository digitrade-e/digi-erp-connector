//go:build windows

package provision

import (
	"path/filepath"

	"golang.org/x/sys/windows"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/tcptable"
)

// Listeners reads the listening TCP sockets and their processes from the TCP
// table (GetExtendedTcpTable), IPv4 and IPv6 — no netstat to parse, nothing
// localised.
func (winSystem) Listeners() ([]Listener, error) {
	var out []Listener
	for _, family := range []uint32{afInet, afInet6} {
		rows, err := tcptable.Read(family, tcptable.OwnerPIDListener)
		if err != nil {
			return nil, err
		}
		out = append(out, listenersOf(rows)...)
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
