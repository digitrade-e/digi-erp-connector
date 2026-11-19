//go:build !windows

package link

import "os"

func openEventSink(string) eventSink { return nil }

// MachineFacts off Windows: the host name only, and no role (installers are refused: plan 090 D3).
func MachineFacts() (host, domain string, role int, roleKnown bool) {
	host, _ = os.Hostname()
	return host, "", 0, false
}

// PlatformPreflight adds nothing off Windows: no elevation or OS check.
func PlatformPreflight(*PreflightEnv) {}
