package autostart

import (
	"testing"
	"time"
)

// EnsureWindowsServiceRecovery writes only when the settings differ from the
// plan, so a second provisioning run changes nothing.
func TestRecoveryMatches(t *testing.T) {
	plan := recoveryActions()
	if !recoveryMatches(plan, recoveryResetPeriod, true) {
		t.Fatal("the plan itself does not match")
	}
	shorter := append([]recoveryAction(nil), plan[:2]...)
	slower := append([]recoveryAction(nil), plan...)
	slower[0].delay = 2 * time.Minute
	noAction := append([]recoveryAction(nil), plan...)
	noAction[2].restart = false
	for name, tc := range map[string]struct {
		actions  []recoveryAction
		reset    time.Duration
		nonCrash bool
	}{
		"crashes only":       {plan, recoveryResetPeriod, false},
		"another reset":      {plan, time.Hour, true},
		"fewer actions":      {shorter, recoveryResetPeriod, true},
		"another delay":      {slower, recoveryResetPeriod, true},
		"a no-action step":   {noAction, recoveryResetPeriod, true},
		"nothing configured": {nil, 0, false},
	} {
		if recoveryMatches(tc.actions, tc.reset, tc.nonCrash) {
			t.Errorf("%s: matched the plan", name)
		}
	}
}
