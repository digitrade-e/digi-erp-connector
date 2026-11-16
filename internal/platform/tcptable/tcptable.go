// Package tcptable reads the TCP table with each socket's owning process (iphlpapi's GetExtendedTcpTable): the
// listening sockets for provision's discover, and the process at the far end of a loopback connection for the link's
// data path (plan 086 review B1: a stream is piped only to the connector's own service). The parsing is plain bytes, so
// it is tested on every platform; Read is Windows only.
package tcptable

import (
	"encoding/binary"
	"errors"
	"net"
)

// The address families and row sizes, as plain numbers.
const (
	AFInet   = 2  // AF_INET
	AFInet6  = 23 // AF_INET6
	RowSize4 = 24 // MIB_TCPROW_OWNER_PID
	RowSize6 = 56 // MIB_TCP6ROW_OWNER_PID
)

// The tables (TCP_TABLE_CLASS) this package reads.
const (
	OwnerPIDListener    = 3 // TCP_TABLE_OWNER_PID_LISTENER
	OwnerPIDConnections = 4 // TCP_TABLE_OWNER_PID_CONNECTIONS
)

// Row is one socket of the table.
type Row struct {
	LocalIP    net.IP
	LocalPort  int
	RemoteIP   net.IP
	RemotePort int
	PID        uint32
}

// Parse reads a MIB_TCPTABLE_OWNER_PID or MIB_TCP6TABLE_OWNER_PID: a row count, then the rows, each port in network
// byte order in the low word.
func Parse(buf []byte, family uint32) ([]Row, error) {
	if len(buf) < 4 {
		return nil, nil
	}
	n := int(binary.LittleEndian.Uint32(buf))
	size := RowSize4
	if family == AFInet6 {
		size = RowSize6
	}
	if n < 0 || n > (len(buf)-4)/size {
		return nil, errors.New("the TCP table is shorter than it says")
	}
	out := make([]Row, 0, n)
	for i := 0; i < n; i++ {
		row := buf[4+i*size : 4+(i+1)*size]
		var r Row
		if family == AFInet6 {
			// ucLocalAddr[16], dwLocalScopeId, dwLocalPort, ucRemoteAddr[16], dwRemoteScopeId, dwRemotePort, dwState, dwOwningPid
			r.LocalIP = net.IP(append([]byte(nil), row[0:16]...))
			r.LocalPort = int(row[20])<<8 | int(row[21])
			r.RemoteIP = net.IP(append([]byte(nil), row[24:40]...))
			r.RemotePort = int(row[44])<<8 | int(row[45])
			r.PID = binary.LittleEndian.Uint32(row[52:56])
		} else {
			// dwState, dwLocalAddr, dwLocalPort, dwRemoteAddr, dwRemotePort, dwOwningPid
			r.LocalIP = net.IP(append([]byte(nil), row[4:8]...))
			r.LocalPort = int(row[8])<<8 | int(row[9])
			r.RemoteIP = net.IP(append([]byte(nil), row[12:16]...))
			r.RemotePort = int(row[16])<<8 | int(row[17])
			r.PID = binary.LittleEndian.Uint32(row[20:24])
		}
		out = append(out, r)
	}
	return out, nil
}

// ServerSide finds the process that accepted the connection from local to remote: the row whose local end is remote
// and whose remote end is local. IPv4 and its IPv6-mapped form are the same address (a dual-stack listener).
func ServerSide(rows []Row, local, remote *net.TCPAddr) (uint32, bool) {
	for _, r := range rows {
		if r.LocalPort == remote.Port && r.RemotePort == local.Port && r.LocalIP.Equal(remote.IP) && r.RemoteIP.Equal(local.IP) {
			return r.PID, true
		}
	}
	return 0, false
}

// ListenerOf finds the process listening at addr: a socket bound to that address first (the kernel prefers it), then
// one bound to every address.
func ListenerOf(rows []Row, addr *net.TCPAddr) (uint32, bool) {
	for _, r := range rows {
		if r.LocalPort == addr.Port && r.LocalIP.Equal(addr.IP) {
			return r.PID, true
		}
	}
	for _, r := range rows {
		if r.LocalPort == addr.Port && r.LocalIP.IsUnspecified() {
			return r.PID, true
		}
	}
	return 0, false
}
