//go:build !windows

package link

import "net"

// DaemonOwnerCheck off Windows: none (the link ships for Windows; elsewhere the data path keeps its earlier behaviour).
func DaemonOwnerCheck() func(net.Conn) error { return nil }
