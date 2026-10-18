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
