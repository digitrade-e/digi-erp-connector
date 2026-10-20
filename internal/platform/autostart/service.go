package autostart

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// StopTimeout bounds a stop of the daemon: the HTTP shutdown (a receipt being
// written may take up to 120 s) and then draining the order queue. The Windows
// service reports StopPending with a WaitHint a little past it, and the
// installer waits up to 180 s for STOPPED before replacing the binaries.
const StopTimeout = 150 * time.Second

// stopWaitHint is what the service tells the SCM to expect while stopping.
const stopWaitHint = StopTimeout + 10*time.Second

// ServiceStopWait is how long a caller of StopWindowsService (the GUI, and its
// default) waits for STOPPED: the whole stop budget plus a margin past the wait
// hint. Anything shorter reports a timeout while the service is still stopping.
const ServiceStopWait = StopTimeout + 15*time.Second

// ErrServiceStateTimeout means the service did not reach the wanted state in
// time. For a stop that is not a failure: the service is still stopping.
var ErrServiceStateTimeout = errors.New("timed out waiting for the service")

func serviceStateTimeout(want, current uint32) error {
	return fmt.Errorf("%w to reach state %d (current %d)", ErrServiceStateTimeout, want, current)
}

// startWaitHint is how long the SCM is told to wait for the next start
// checkpoint. Each walk progress call (every 64 data-directory entries) sends
// one, so even a large data directory never looks hung.
const startWaitHint = 30 * time.Second

// StartProgressSetter is implemented by an app whose Start can take a while
// (the daemon walks its data directory). The service handler hands it a
// callback that reports the next SERVICE_START_PENDING checkpoint.
type StartProgressSetter interface {
	SetStartProgress(func())
}

// newStartCheckpoints returns a callback that sends checkpoint 1, 2, 3, …
func newStartCheckpoints(send func(checkpoint uint32)) func() {
	var cp uint32
	return func() {
		cp++
		send(cp)
	}
}

// ExitDataDirRefused is the daemon's service-specific exit code when it refused
// to start because its data directory could not be protected (the reason is
// in the Windows Event Log, Application, source digi-erp-connectord). `sc
// query` shows it as SERVICE_EXIT_CODE.
const ExitDataDirRefused uint32 = 2

// ExitCoder is a Start error that carries a service-specific exit code.
type ExitCoder interface {
	ExitCode() uint32
}

// ServiceStoppedError: the service stopped while it was being started.
type ServiceStoppedError struct {
	Win32ExitCode           uint32
	ServiceSpecificExitCode uint32
}

func (e *ServiceStoppedError) Error() string {
	return fmt.Sprintf("the service stopped while starting (exit code %d, service exit code %d)", e.Win32ExitCode, e.ServiceSpecificExitCode)
}

// startWaitFloor is the least a caller waits for a starting service between
// checkpoints.
const startWaitFloor = 30 * time.Second

// startWaitCap bounds a whole start wait, however many checkpoints come.
const startWaitCap = 10 * time.Minute

type startSnapshot struct {
	checkpoint uint32
	waitHint   time.Duration
}

// nextStartDeadline moves the wait's deadline when the service reports a new
// checkpoint: to its wait hint from now, at least startWaitFloor.
func nextStartDeadline(deadline time.Time, prevCheckpoint uint32, cur startSnapshot, now time.Time) time.Time {
	if cur.checkpoint == prevCheckpoint {
		return deadline
	}
	hint := cur.waitHint
	if hint < startWaitFloor {
		hint = startWaitFloor
	}
	return now.Add(hint)
}

// servicePollInterval is how often a wait for a service state polls it.
const servicePollInterval = 300 * time.Millisecond

// startStatus is what waitForStartWith needs of a service's status.
type startStatus struct {
	state      startState
	checkpoint uint32
	waitHint   time.Duration
	win32      uint32
	specific   uint32
}

type startState int

const (
	startPending startState = iota
	startRunning
	startStopped
)

// waitForStartWith waits for a starting service to run, following its
// START_PENDING checkpoints: each new checkpoint moves the deadline to its
// wait hint from now (at least startWaitFloor), and nothing waits past
// startWaitCap. A service that stops instead ends the wait with a
// ServiceStoppedError carrying its exit codes.
func waitForStartWith(query func() (startStatus, error), now func() time.Time, sleep func(time.Duration)) error {
	start := now()
	deadline := start.Add(startWaitFloor)
	var last uint32
	for {
		st, err := query()
		if err != nil {
			return err
		}
		switch st.state {
		case startRunning:
			return nil
		case startStopped:
			return &ServiceStoppedError{Win32ExitCode: st.win32, ServiceSpecificExitCode: st.specific}
		}
		t := now()
		deadline = nextStartDeadline(deadline, last, startSnapshot{checkpoint: st.checkpoint, waitHint: st.waitHint}, t)
		last = st.checkpoint
		if t.After(deadline) || t.Sub(start) >= startWaitCap {
			return serviceStateTimeout(4 /* SERVICE_RUNNING */, 2 /* SERVICE_START_PENDING */)
		}
		sleep(servicePollInterval)
	}
}

// The service manager's restarts after a failure — a crash or a non-zero
// exit such as a refused data directory: 1 min, 5 min, then every 30 min, the
// count reset after a day without failures. Never "no action": a refusal that
// clears by itself (a domain controller back) must be picked up.
var recoveryDelays = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute}

const recoveryResetPeriod = 24 * time.Hour

// recoveryAction is one SCM failure action, portable so the plan is tested
// everywhere: restart (or do nothing) after delay.
type recoveryAction struct {
	restart bool
	delay   time.Duration
}

// recoveryActions is the list setRecovery hands the SCM: a restart at each
// planned delay.
func recoveryActions() []recoveryAction {
	out := make([]recoveryAction, 0, len(recoveryDelays))
	for _, d := range recoveryDelays {
		out = append(out, recoveryAction{restart: true, delay: d})
	}
	return out
}

// recoveryMatches reports whether a service's failure settings are the plan:
// recoveryActions, reset after recoveryResetPeriod, also on non-crash
// failures. EnsureWindowsServiceRecovery writes only when they are not.
func recoveryMatches(actions []recoveryAction, reset time.Duration, nonCrash bool) bool {
	want := recoveryActions()
	if !nonCrash || reset != recoveryResetPeriod || len(actions) != len(want) {
		return false
	}
	for i := range want {
		if actions[i] != want[i] {
			return false
		}
	}
	return true
}

// ServiceInfo is what digi-erp-connector-provision reads of a Windows
// service (WindowsServiceInfo).
type ServiceInfo struct {
	Installed bool `json:"installed"`
	// State is "running", "stopped", "start_pending", "stop_pending",
	// "paused", "continue_pending" or "pause_pending".
	State string `json:"state,omitempty"`
	// StartType is "auto", "manual", "disabled", "boot" or "system".
	StartType    string   `json:"startType,omitempty"`
	Account      string   `json:"account,omitempty"`
	Dependencies []string `json:"dependencies,omitempty"`
	ProcessID    uint32   `json:"processId,omitempty"`
	// ProcessStart is when the running process started (zero when it is not
	// running): a config.yaml written after it is not what the service runs.
	ProcessStart time.Time `json:"processStart,omitempty"`
	// RecoveryPlanned: the failure actions are the plan (recoveryMatches).
	RecoveryPlanned bool `json:"recoveryPlanned"`
}

type ServiceApp interface {
	Start() error
	Stop(ctx context.Context)
	Errors() <-chan error
	Logger() Logger
}

type Logger interface {
	Info(msg string)
	Error(msg string, err error)
}
