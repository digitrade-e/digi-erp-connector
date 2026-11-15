//go:build !windows

package link

import "os"

func openEventSink(string) eventSink { return nil }

// MachineFacts off Windows: the host name only.
func MachineFacts() (string, string, int) {
	host, _ := os.Hostname()
	return host, "", 0
}
