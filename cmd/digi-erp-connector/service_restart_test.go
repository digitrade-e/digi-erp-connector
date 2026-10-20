package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/autostart"
)

func TestServiceStopTimeoutOutlastsTheDaemonStop(t *testing.T) {
	if serviceStopTimeout <= autostart.StopTimeout {
		t.Fatalf("the GUI waits %v for a stop that may take %v", serviceStopTimeout, autostart.StopTimeout)
	}
}

func TestRestartService(t *testing.T) {
	timeout := fmt.Errorf("wait: %w", autostart.ErrServiceStateTimeout)
	cases := []struct {
		name       string
		stopErr    error
		startErr   error
		wantStart  bool
		wantSubstr []string
	}{
		{name: "stopped then started", wantStart: true, wantSubstr: []string{"restarted"}},
		{name: "stop timed out: no start", stopErr: timeout, wantSubstr: []string{"still stopping", "queued orders", "not started again"}},
		{name: "stop failed: no start", stopErr: errors.New("access denied"), wantSubstr: []string{"access denied", "not started again"}},
		{name: "start failed", startErr: errors.New("boom"), wantStart: true, wantSubstr: []string{"Failed to restart", "boom"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			started := false
			msg := restartService(
				func() error { return tc.stopErr },
				func() error { started = true; return tc.startErr },
			)
			if started != tc.wantStart {
				t.Errorf("start called = %v; want %v", started, tc.wantStart)
			}
			for _, s := range tc.wantSubstr {
				if !strings.Contains(msg, s) {
					t.Errorf("message %q lacks %q", msg, s)
				}
			}
			if tc.startErr == nil && tc.stopErr != nil && strings.Contains(msg, "restarted.") {
				t.Errorf("message %q claims a restart that did not happen", msg)
			}
		})
	}
}

func TestStopServiceMessage(t *testing.T) {
	if msg := stopServiceMessage(nil); msg != "Server service stopped." {
		t.Errorf("success = %q", msg)
	}
	msg := stopServiceMessage(fmt.Errorf("wait: %w", autostart.ErrServiceStateTimeout))
	if !strings.Contains(msg, "still stopping") || strings.Contains(msg, "Failed") {
		t.Errorf("timeout = %q; want a still-stopping message, not a failure", msg)
	}
	if msg := stopServiceMessage(errors.New("access denied")); !strings.Contains(msg, "Failed to stop") {
		t.Errorf("failure = %q", msg)
	}
}

// The status shown while stopping tells the operator the wait can be long, in
// the units of the real budget.
func TestStoppingStatusNamesTheBudget(t *testing.T) {
	if !strings.Contains(stopBudgetText(), "2.5 min") {
		t.Errorf("stopBudgetText() = %q; want it to say 2.5 min for a %v budget", stopBudgetText(), autostart.StopTimeout)
	}
	for _, s := range []string{stoppingStatus(), restartingStatus()} {
		if !strings.Contains(s, "queued orders") || !strings.Contains(s, stopBudgetText()) {
			t.Errorf("status %q does not say it may wait for queued orders (%s)", s, stopBudgetText())
		}
	}
}

// R3-1: a service that refused to start because its data directory could
// not be protected is named as such — not a bare 30 s timeout.
func TestStartFailureMessage(t *testing.T) {
	refused := &autostart.ServiceStoppedError{Win32ExitCode: 1066, ServiceSpecificExitCode: autostart.ExitDataDirRefused}
	if m := startFailureMessage(fmt.Errorf("wait: %w", refused)); !strings.Contains(m, "refused to start") || !strings.Contains(m, "Event Log") {
		t.Errorf("message %q; want it to say the service refused and where to look", m)
	}
	other := &autostart.ServiceStoppedError{Win32ExitCode: 1, ServiceSpecificExitCode: 0}
	if m := startFailureMessage(other); !strings.Contains(m, "stopped") {
		t.Errorf("message %q", m)
	}
	if m := startFailureMessage(errors.New("boom")); !strings.Contains(m, "boom") {
		t.Errorf("message %q", m)
	}
}
