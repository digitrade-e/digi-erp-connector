package autostart

import (
	"errors"
	"fmt"
	"testing"
	"time"
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

// Each call reports the next start checkpoint, so the SCM keeps waiting while
// a long start (the data-directory walk) makes progress.
func TestStartCheckpoints(t *testing.T) {
	var got []uint32
	next := newStartCheckpoints(func(cp uint32) { got = append(got, cp) })
	next()
	next()
	next()
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Errorf("checkpoints %v; want 1 2 3", got)
	}
	if startWaitHint < 10*time.Second || startWaitHint > 2*time.Minute {
		t.Errorf("startWaitHint %v", startWaitHint)
	}
}

// The GUI's wait for a starting service follows the service's checkpoints:
// each new checkpoint moves the deadline to the wait hint from now (at least
// the 30 s floor); a service that stops while starting ends the wait with its
// exit codes.
func TestNextStartDeadline(t *testing.T) {
	now := time.Unix(1000, 0)
	d := now.Add(30 * time.Second)
	if got := nextStartDeadline(d, 0, startSnapshot{checkpoint: 0, waitHint: 30 * time.Second}, now); !got.Equal(d) {
		t.Errorf("same checkpoint moved the deadline to %v", got)
	}
	if got := nextStartDeadline(d, 3, startSnapshot{checkpoint: 4, waitHint: 45 * time.Second}, now.Add(25*time.Second)); !got.Equal(now.Add(70 * time.Second)) {
		t.Errorf("a new checkpoint gave deadline %v; want now+45 s", got)
	}
	if got := nextStartDeadline(d, 3, startSnapshot{checkpoint: 4, waitHint: time.Second}, now); !got.Equal(now.Add(startWaitFloor)) {
		t.Errorf("a short wait hint gave deadline %v; want the floor", got)
	}
}

func TestServiceStoppedError(t *testing.T) {
	err := error(&ServiceStoppedError{Win32ExitCode: 1066, ServiceSpecificExitCode: ExitDataDirRefused})
	var s *ServiceStoppedError
	if !errors.As(err, &s) || s.ServiceSpecificExitCode != ExitDataDirRefused || err.Error() == "" {
		t.Errorf("ServiceStoppedError = %v", err)
	}
}
