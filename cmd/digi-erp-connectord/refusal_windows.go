//go:build windows

package main

import (
	"strings"

	"golang.org/x/sys/windows/svc/eventlog"
)

// reportRefusal writes why the daemon refused to start to the Windows
// Application Event Log (source digi-erp-connectord) — never to a file in the
// data directory, which is what could not be trusted. It registers the
// source on first use (EventCreate.exe messages; it is SYSTEM) and works
// unregistered too.
func reportRefusal(lines []string, err error) {
	_ = eventlog.InstallAsEventCreate(windowsServiceName, eventlog.Error|eventlog.Warning|eventlog.Info)
	l, e := eventlog.Open(windowsServiceName)
	if e != nil {
		return
	}
	defer l.Close()
	text := "digi-erp-connectord refused to start: " + err.Error() +
		"\r\n\r\nSee \"The service will not start\" in docs/troubleshooting.md.\r\n\r\n" + strings.Join(lines, "\r\n")
	if len(text) > 30000 {
		text = text[:30000]
	}
	_ = l.Error(1, text)
}
