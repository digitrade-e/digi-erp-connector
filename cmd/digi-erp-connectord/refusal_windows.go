//go:build windows

package main

import (
	"time"

	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc/eventlog"
)

// The last refusal written, outside the data directory (which is what could
// not be trusted): HKLM\SOFTWARE\digi-erp-connector, value LastRefusal.
var (
	refusalStateKey   = `SOFTWARE\digi-erp-connector`
	refusalStateValue = "LastRefusal"
)

func loadRefusalState() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, refusalStateKey, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, _ := k.GetStringValue(refusalStateValue)
	return v
}

func saveRefusalState(v string) {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, refusalStateKey, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	_ = k.SetStringValue(refusalStateValue, v)
}

// clearRefusalState runs after a start got past the data directory, so the
// next refusal — even for the same reason — is written at once.
func clearRefusalState() {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, refusalStateKey, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	_ = k.DeleteValue(refusalStateValue)
}

// reportRefusal writes why the daemon refused to start to the Windows
// Application Event Log (source digi-erp-connectord) — never to a file in the
// data directory, which is what could not be trusted — when the reason is new
// or was last written a day ago (refusalDue). It registers the source on
// first use (EventCreate.exe messages; it is SYSTEM) and works unregistered.
func reportRefusal(lines []string, err error) {
	due, next := refusalDue(loadRefusalState(), err.Error(), time.Now())
	if !due {
		return
	}
	_ = eventlog.InstallAsEventCreate(windowsServiceName, eventlog.Error|eventlog.Warning|eventlog.Info)
	l, e := eventlog.Open(windowsServiceName)
	if e != nil {
		return
	}
	defer l.Close()
	if l.Error(1, refusalText(lines, err)) == nil {
		saveRefusalState(next)
	}
}
