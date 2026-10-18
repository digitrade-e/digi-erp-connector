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
