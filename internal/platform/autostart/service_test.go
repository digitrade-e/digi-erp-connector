package autostart

import (
	"errors"
	"fmt"
	"testing"
)

// Whoever waits for the service to stop must wait longer than the service can
// legitimately take: the daemon's whole stop budget, and the wait hint it gives
// the SCM. A shorter wait reports a timeout while the service is still stopping
// (the GUI's old 20 s did, against a 150 s stop).
func TestServiceStopWaitCoversTheWholeStop(t *testing.T) {
	if ServiceStopWait <= StopTimeout {
		t.Errorf("ServiceStopWait %v <= StopTimeout %v", ServiceStopWait, StopTimeout)
	}
	if ServiceStopWait <= stopWaitHint {
		t.Errorf("ServiceStopWait %v <= the wait hint %v the service reports", ServiceStopWait, stopWaitHint)
	}
}

func TestErrServiceStateTimeoutSurvivesWrapping(t *testing.T) {
	err := fmt.Errorf("stop: %w", serviceStateTimeout(1, 3))
	if !errors.Is(err, ErrServiceStateTimeout) {
		t.Fatalf("%v is not ErrServiceStateTimeout", err)
	}
}
