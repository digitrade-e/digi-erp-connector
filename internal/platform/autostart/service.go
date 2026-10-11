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
