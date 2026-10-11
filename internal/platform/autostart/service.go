package autostart

import (
	"context"
	"time"
)

// StopTimeout bounds a stop of the daemon: the HTTP shutdown (a receipt being
// written may take up to 120 s) and then draining the order queue. The Windows
// service reports StopPending with a WaitHint a little past it, and the
// installer waits up to 180 s for STOPPED before replacing the binaries.
const StopTimeout = 150 * time.Second

// stopWaitHint is what the service tells the SCM to expect while stopping.
const stopWaitHint = StopTimeout + 10*time.Second

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
