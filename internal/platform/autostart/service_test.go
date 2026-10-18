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

// fakeStart is a starting service as waitForStartWith sees it, on a fake clock.
type fakeStart struct {
	now      time.Time
	statuses []startStatus // one per poll; the last repeats
	polls    int
}

func (f *fakeStart) query() (startStatus, error) {
	i := f.polls
	if i >= len(f.statuses) {
		i = len(f.statuses) - 1
	}
	f.polls++
	return f.statuses[i], nil
}
func (f *fakeStart) clock() time.Time      { return f.now }
func (f *fakeStart) sleep(d time.Duration) { f.now = f.now.Add(d) }

func pending(cp uint32, hint time.Duration) startStatus {
	return startStatus{state: startPending, checkpoint: cp, waitHint: hint}
}

// The wait follows checkpoints: a service that keeps reporting progress is
// waited for past the 30 s floor, one that stops reporting times out 30 s
// (or its wait hint) after its last checkpoint, one that stops ends the wait
// with its exit codes, and nothing waits past startWaitCap.
func TestWaitForStartWith(t *testing.T) {
	t.Run("running after a long, progressing start", func(t *testing.T) {
		var ss []startStatus
		for cp := uint32(0); cp < 300; cp++ { // 300 polls × 300 ms = 90 s
			ss = append(ss, pending(cp, 30*time.Second))
		}
		ss = append(ss, startStatus{state: startRunning})
		f := &fakeStart{now: time.Unix(0, 0), statuses: ss}
		if err := waitForStartWith(f.query, f.clock, f.sleep); err != nil {
			t.Fatalf("waitForStartWith = %v; want running", err)
		}
	})
	t.Run("no progress times out after the floor", func(t *testing.T) {
		f := &fakeStart{now: time.Unix(0, 0), statuses: []startStatus{pending(1, time.Second)}}
		start := f.now
		err := waitForStartWith(f.query, f.clock, f.sleep)
		if !errors.Is(err, ErrServiceStateTimeout) {
			t.Fatalf("waitForStartWith = %v; want a timeout", err)
		}
		if d := f.now.Sub(start); d < startWaitFloor || d > startWaitFloor+time.Second {
			t.Errorf("timed out after %v; want about %v", d, startWaitFloor)
		}
	})
	t.Run("stopped carries the exit codes", func(t *testing.T) {
		f := &fakeStart{now: time.Unix(0, 0), statuses: []startStatus{pending(1, time.Second),
			{state: startStopped, win32: 1066, specific: ExitDataDirRefused}}}
		var s *ServiceStoppedError
		if err := waitForStartWith(f.query, f.clock, f.sleep); !errors.As(err, &s) || s.ServiceSpecificExitCode != ExitDataDirRefused {
			t.Fatalf("waitForStartWith = %v; want ServiceStoppedError with code %d", err, ExitDataDirRefused)
		}
	})
	t.Run("the cap holds whatever the checkpoints say", func(t *testing.T) {
		f := &fakeStart{now: time.Unix(0, 0)}
		for cp := uint32(0); cp < 5000; cp++ {
			f.statuses = append(f.statuses, pending(cp, time.Hour))
		}
		start := f.now
		if err := waitForStartWith(f.query, f.clock, f.sleep); !errors.Is(err, ErrServiceStateTimeout) {
			t.Fatalf("waitForStartWith = %v; want a timeout at the cap", err)
		}
		if d := f.now.Sub(start); d < startWaitCap || d > startWaitCap+time.Second {
			t.Errorf("gave up after %v; want about %v", d, startWaitCap)
		}
	})
}

// R4-2b: the service manager keeps restarting the daemon (1 min, 5 min, then
// every 30 min) — a refusal that clears (a domain controller back) is picked
// up without anyone noticing it gave up.
func TestRecoveryPlan(t *testing.T) {
	want := []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute}
	if len(recoveryDelays) != len(want) {
		t.Fatalf("recovery delays %v; want %v", recoveryDelays, want)
	}
	for i := range want {
		if recoveryDelays[i] != want[i] {
			t.Errorf("recovery delay %d = %v; want %v", i, recoveryDelays[i], want[i])
		}
	}
	if recoveryResetPeriod != 24*time.Hour {
		t.Errorf("reset period %v", recoveryResetPeriod)
	}
}

// The action list setRecovery hands the SCM: every action a restart, at the
// planned delays — never "no action" in between or at the end.
func TestRecoveryActions(t *testing.T) {
	got := recoveryActions()
	if len(got) != len(recoveryDelays) {
		t.Fatalf("actions %+v; want %d", got, len(recoveryDelays))
	}
	for i, a := range got {
		if !a.restart || a.delay != recoveryDelays[i] {
			t.Errorf("action %d = %+v; want a restart after %v", i, a, recoveryDelays[i])
		}
	}
}
