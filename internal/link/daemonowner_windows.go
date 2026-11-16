//go:build windows

package link

import (
	"errors"
	"fmt"
	"net"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/tcptable"
)

// DaemonOwnerCheck is the data path's check that a connection to the daemon's address reached the connector's own
// service (review B1): the process that accepted it — the TCP table's owner of its far end — must be the process the
// service manager runs as digi-erp-connectord. A local user who takes the port while the daemon restarts (a drain lasts
// up to 150 s), or a port it left behind, gets no stream and so never sees erp-manager's /auth/token credential.
func DaemonOwnerCheck() func(net.Conn) error {
	return ownerCheck(func() (uint32, error) { return servicePID(DaemonServiceName) })
}

func ownerCheck(want func() (uint32, error)) func(net.Conn) error {
	return func(c net.Conn) error {
		pid, err := want()
		if err != nil {
			return fmt.Errorf("%w: the %s service: %v", errDaemonOwner, DaemonServiceName, err)
		}
		if pid == 0 {
			return fmt.Errorf("%w: the %s service is not running", errDaemonOwner, DaemonServiceName)
		}
		got, err := peerPID(c)
		if err != nil {
			return fmt.Errorf("%w: %v", errDaemonOwner, err)
		}
		if got != pid {
			return fmt.Errorf("%w: %s is answered by process %d, not the %s service (process %d)", errDaemonOwner, c.RemoteAddr(), got, DaemonServiceName, pid)
		}
		return nil
	}
}

// peerPID is the process that owns the far end of c: the accepted socket in the connection table (both families: a
// dual-stack listener's connections can sit in the IPv6 table, IPv4-mapped), else the socket listening at the address.
func peerPID(c net.Conn) (uint32, error) {
	local, ok1 := c.LocalAddr().(*net.TCPAddr)
	remote, ok2 := c.RemoteAddr().(*net.TCPAddr)
	if !ok1 || !ok2 {
		return 0, errors.New("not a TCP connection")
	}
	families := []uint32{tcptable.AFInet, tcptable.AFInet6}
	if remote.IP.To4() == nil {
		families = []uint32{tcptable.AFInet6}
	}
	read := func(class uint32) ([]tcptable.Row, error) {
		var all []tcptable.Row
		for _, f := range families {
			rows, err := tcptable.Read(f, class)
			if err != nil {
				return nil, err
			}
			all = append(all, rows...)
		}
		return all, nil
	}
	// The far end's row appears as the handshake completes; on loopback that is at once, but look a few times.
	for attempt := 0; attempt < 5; attempt++ {
		rows, err := read(tcptable.OwnerPIDConnections)
		if err != nil {
			return 0, fmt.Errorf("the TCP table: %w", err)
		}
		if pid, ok := tcptable.ServerSide(rows, local, remote); ok {
			return pid, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	rows, err := read(tcptable.OwnerPIDListener)
	if err != nil {
		return 0, fmt.Errorf("the TCP table: %w", err)
	}
	if pid, ok := tcptable.ListenerOf(rows, remote); ok {
		return pid, nil
	}
	return 0, fmt.Errorf("no socket in the TCP table answers %s", remote)
}

// servicePID is the process the service manager runs name as (0 when it runs none).
func servicePID(name string) (uint32, error) {
	m, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return 0, err
	}
	defer func() { _ = windows.CloseServiceHandle(m) }()
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	s, err := windows.OpenService(m, n, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return 0, err
	}
	defer func() { _ = windows.CloseServiceHandle(s) }()
	var st windows.SERVICE_STATUS_PROCESS
	var need uint32
	if err := windows.QueryServiceStatusEx(s, windows.SC_STATUS_PROCESS_INFO, (*byte)(unsafe.Pointer(&st)), uint32(unsafe.Sizeof(st)), &need); err != nil {
		return 0, err
	}
	return st.ProcessId, nil
}
