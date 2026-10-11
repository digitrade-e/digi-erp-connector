package main

// The decisions behind the GUI's Stop and Restart buttons, kept free of walk
// and the Windows service API so they are tested on every platform.

import (
	"errors"
	"strconv"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/autostart"
)

// serviceStopTimeout is how long the GUI waits for the daemon to stop. The
// daemon's stop can legitimately take autostart.StopTimeout (a receipt in
// flight, then the queued orders drained), so the wait outlasts it.
const serviceStopTimeout = autostart.ServiceStopWait

// stopBudgetText is the stop budget for the operator: "up to ~2.5 min".
func stopBudgetText() string {
	return "up to ~" + strconv.FormatFloat(autostart.StopTimeout.Minutes(), 'f', -1, 64) + " min"
}

func stoppingStatus() string {
	return "Stopping server... it finishes a receipt in progress and any queued orders first, which can take " + stopBudgetText() + "."
}

func restartingStatus() string {
	return "Restarting server... the stop finishes a receipt in progress and any queued orders first, which can take " + stopBudgetText() + "."
}

// stopServiceMessage is the status after a stop. A timeout is not a failure:
// the service is still stopping, and saying "failed" would invite a retry.
func stopServiceMessage(err error) string {
	switch {
	case err == nil:
		return "Server service stopped."
	case errors.Is(err, autostart.ErrServiceStateTimeout):
		return "The server is still stopping (it may be finishing queued orders). Check the service status before starting it again."
	default:
		return "Failed to stop server service: " + err.Error()
	}
}

// restartService stops, then starts only if the stop completed. Starting a
// service that is still STOP_PENDING "succeeds" as already running and leaves
// it down once the stop finishes, so a stop that timed out or failed ends the
// restart and is reported as it is. (Stopping a service that is already
// stopped is not an error: StopWindowsService returns nil for it.)
func restartService(stop, start func() error) string {
	if err := stop(); err != nil {
		if errors.Is(err, autostart.ErrServiceStateTimeout) {
			return "Restart not completed: the server is still stopping (it may be finishing queued orders), so it was not started again. Start it once it has stopped."
		}
		return "Restart failed: could not stop server service (" + err.Error() + "); it was not started again."
	}
	if err := start(); err != nil {
		return "Failed to restart server service: stopped, but the start failed: " + err.Error()
	}
	return "Server service restarted."
}
