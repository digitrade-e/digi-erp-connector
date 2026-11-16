//go:build windows

package link

import (
	"errors"
	"net"
	"os"
	"testing"
)

// The TCP table names this test's own process as the owner of a listener it opened, whether the connection was
// accepted yet or not, IPv4 and dual-stack — the check the data path makes against the daemon's service (review B1).
func TestOwnerCheckReadsTheTCPTable(t *testing.T) {
	self := uint32(os.Getpid())
	for _, listen := range []string{"127.0.0.1:0", ":0"} {
		ln, err := net.Listen("tcp", listen)
		if err != nil {
			t.Fatal(err)
		}
		_, port, _ := net.SplitHostPort(ln.Addr().String())
		c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", port))
		if err != nil {
			t.Fatal(err)
		}
		if got, err := peerPID(c); err != nil || got != self {
			t.Fatalf("%s, not accepted yet: pid %d (%v), want %d", listen, got, err, self)
		}
		acc, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		if got, err := peerPID(c); err != nil || got != self {
			t.Fatalf("%s, accepted: pid %d (%v), want %d", listen, got, err, self)
		}
		if err := ownerCheck(func() (uint32, error) { return self, nil })(c); err != nil {
			t.Fatalf("%s: the owner was refused: %v", listen, err)
		}
		if err := ownerCheck(func() (uint32, error) { return self + 4, nil })(c); !errors.Is(err, errDaemonOwner) {
			t.Fatalf("%s: another process was accepted: %v", listen, err)
		}
		if err := ownerCheck(func() (uint32, error) { return 0, nil })(c); !errors.Is(err, errDaemonOwner) {
			t.Fatalf("%s: a stopped service was accepted: %v", listen, err)
		}
		_ = acc.Close()
		_ = c.Close()
		_ = ln.Close()
	}
}
