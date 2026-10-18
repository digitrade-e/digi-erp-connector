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
