//go:build windows

package link

import (
	"os"

	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc/eventlog"
)

type winEvents struct{ l *eventlog.Log }

// openEventSink writes to the Application log under source; the installer
// registers the source, and without it Windows still records the text.
func openEventSink(source string) eventSink {
	l, err := eventlog.Open(source)
	if err != nil {
		return nil
	}
	return winEvents{l: l}
}

func (e winEvents) write(level, msg string) {
	switch level {
	case "ERROR":
		_ = e.l.Error(3, msg)
	case "WARN":
		_ = e.l.Warning(2, msg)
	default:
		_ = e.l.Info(1, msg)
	}
}

func (e winEvents) close() { _ = e.l.Close() }

// MachineFacts are what a human sees before confirming a pairing: the host,
// its DNS domain, and whether it is a domain controller (role 4, NTDS present)
// or a member (3) or standalone (2) server. digi-mcp's discover reads the exact
// role later.
func MachineFacts() (string, string, int) {
	host, _ := os.Hostname()
	domain := ""
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\Tcpip\Parameters`, registry.QUERY_VALUE); err == nil {
		domain, _, _ = k.GetStringValue("Domain")
		_ = k.Close()
	}
	role := 2
	if domain != "" {
		role = 3
	}
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\NTDS`, registry.QUERY_VALUE); err == nil {
		_ = k.Close()
		role = 4
	}
	return host, domain, role
}
