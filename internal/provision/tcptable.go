package provision

import (
	"encoding/binary"
	"errors"
	"net"
)

// The TCP table's address families and row sizes (iphlpapi), as plain
// numbers so the parsing is tested on every platform.
const (
	afInet      = 2  // AF_INET
	afInet6     = 23 // AF_INET6
	tcpRowSize4 = 24 // MIB_TCPROW_OWNER_PID
	tcpRowSize6 = 56 // MIB_TCP6ROW_OWNER_PID
)

// parseTCPTable reads a MIB_TCPTABLE_OWNER_PID or MIB_TCP6TABLE_OWNER_PID:
// a row count, then the rows.
func parseTCPTable(buf []byte, family uint32) ([]Listener, error) {
	if len(buf) < 4 {
		return nil, nil
	}
	n := int(binary.LittleEndian.Uint32(buf))
	size := tcpRowSize4
	if family == afInet6 {
		size = tcpRowSize6
	}
	if 4+n*size > len(buf) {
		return nil, errors.New("the TCP table is shorter than it says")
	}
	out := make([]Listener, 0, n)
	for i := 0; i < n; i++ {
		row := buf[4+i*size : 4+(i+1)*size]
		var l Listener
		if family == afInet {
			// dwState, dwLocalAddr, dwLocalPort, dwRemoteAddr, dwRemotePort, dwOwningPid
			l.Address = net.IP(row[4:8]).String()
			l.Port = int(row[8])<<8 | int(row[9]) // network byte order in the low word
			l.PID = binary.LittleEndian.Uint32(row[20:24])
		} else {
			// ucLocalAddr[16], dwLocalScopeId, dwLocalPort, ucRemoteAddr[16], dwRemoteScopeId, dwRemotePort, dwState, dwOwningPid
			l.Address = net.IP(row[0:16]).String()
			l.Port = int(row[20])<<8 | int(row[21])
			l.PID = binary.LittleEndian.Uint32(row[52:56])
		}
		out = append(out, l)
	}
	return out, nil
}
