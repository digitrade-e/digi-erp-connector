package tcptable

import (
	"encoding/binary"
	"net"
	"testing"
)

func row4(local net.IP, lport int, remote net.IP, rport int, pid uint32) []byte {
	b := make([]byte, RowSize4)
	copy(b[4:8], local.To4())
	b[8], b[9] = byte(lport>>8), byte(lport)
	copy(b[12:16], remote.To4())
	b[16], b[17] = byte(rport>>8), byte(rport)
	binary.LittleEndian.PutUint32(b[20:24], pid)
	return b
}

func row6(local net.IP, lport int, remote net.IP, rport int, pid uint32) []byte {
	b := make([]byte, RowSize6)
	copy(b[0:16], local.To16())
	b[20], b[21] = byte(lport>>8), byte(lport)
	copy(b[24:40], remote.To16())
	b[44], b[45] = byte(rport>>8), byte(rport)
	binary.LittleEndian.PutUint32(b[52:56], pid)
	return b
}

func table(rows ...[]byte) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, uint32(len(rows)))
	for _, r := range rows {
		b = append(b, r...)
	}
	return b
}

var lo = net.IPv4(127, 0, 0, 1)

// The connection table has both ends of a loopback connection: the link's socket and the one the daemon accepted.
func TestServerSideFindsTheProcessThatAccepted(t *testing.T) {
	rows, err := Parse(table(
		row4(lo, 51000, lo, 8082, 100), // the link's end
		row4(lo, 8082, lo, 51000, 900), // the daemon's end
		row4(lo, 8082, lo, 51001, 901), // another connection
	), AFInet)
	if err != nil {
		t.Fatal(err)
	}
	local, remote := &net.TCPAddr{IP: lo, Port: 51000}, &net.TCPAddr{IP: lo, Port: 8082}
	if pid, ok := ServerSide(rows, local, remote); !ok || pid != 900 {
		t.Fatalf("got %d %v", pid, ok)
	}
	if _, ok := ServerSide(rows, &net.TCPAddr{IP: lo, Port: 52000}, remote); ok {
		t.Fatal("a connection that is not in the table was found")
	}
	// a dual-stack listener's connection: its end in the IPv6 table, in IPv4-mapped form
	rows6, err := Parse(table(row6(lo, 8082, lo, 51000, 902)), AFInet6)
	if err != nil {
		t.Fatal(err)
	}
	if pid, ok := ServerSide(rows6, local, remote); !ok || pid != 902 {
		t.Fatalf("mapped: got %d %v", pid, ok)
	}
}

func TestListenerOfPrefersTheBoundAddress(t *testing.T) {
	rows, err := Parse(table(
		row4(net.IPv4zero, 8082, net.IPv4zero, 0, 900),
		row4(lo, 8082, net.IPv4zero, 0, 666),
		row4(net.IPv4zero, 9000, net.IPv4zero, 0, 7),
	), AFInet)
	if err != nil {
		t.Fatal(err)
	}
	if pid, ok := ListenerOf(rows, &net.TCPAddr{IP: lo, Port: 8082}); !ok || pid != 666 {
		t.Fatalf("got %d %v", pid, ok)
	}
	if pid, ok := ListenerOf(rows, &net.TCPAddr{IP: net.IPv4(10, 0, 0, 5), Port: 8082}); !ok || pid != 900 {
		t.Fatalf("wildcard: got %d %v", pid, ok)
	}
	if _, ok := ListenerOf(rows, &net.TCPAddr{IP: lo, Port: 8083}); ok {
		t.Fatal("no listener there")
	}
}

func TestParseRefusesAShortTable(t *testing.T) {
	b := table(row4(lo, 1, lo, 2, 3))
	if _, err := Parse(b[:20], AFInet); err == nil {
		t.Fatal("a table shorter than its count was read")
	}
	binary.LittleEndian.PutUint32(b, 0xFFFFFFFF)
	if _, err := Parse(b, AFInet); err == nil {
		t.Fatal("a huge count was read")
	}
}
