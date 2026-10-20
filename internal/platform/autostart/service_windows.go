//go:build windows

package autostart

import (
	"context"
	"errors"
	"net/http"

	"golang.org/x/sys/windows/svc"
)

func IsWindowsService() (bool, error) {
	return svc.IsWindowsService()
}

func RunService(name string, app ServiceApp) error {
	return svc.Run(name, &serviceHandler{app: app})
}

type serviceHandler struct {
	app ServiceApp
}

func (h *serviceHandler) Execute(_ []string, r <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	const accepts = svc.AcceptStop | svc.AcceptShutdown
	hint := uint32(startWaitHint.Milliseconds())
	status <- svc.Status{State: svc.StartPending, WaitHint: hint}
	if p, ok := h.app.(StartProgressSetter); ok {
		p.SetStartProgress(newStartCheckpoints(func(cp uint32) {
			status <- svc.Status{State: svc.StartPending, WaitHint: hint, CheckPoint: cp}
		}))
	}

	if err := h.app.Start(); err != nil {
		// No STOPPED of our own here: the framework's final update, from the
		// values returned, is the one that carries the exit code — the SCM's
		// failure actions and the GUI must see that one, not a STOPPED with 0.
		var ec ExitCoder
		if errors.As(err, &ec) {
			return true, ec.ExitCode() // service-specific: `sc query` shows it
		}
		return false, 1
	}

	status <- svc.Status{State: svc.Running, Accepts: accepts}

	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending, WaitHint: uint32(stopWaitHint.Milliseconds())}
				h.stopApp()
				status <- svc.Status{State: svc.Stopped}
				return false, 0
			}
		case err := <-h.app.Errors():
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				h.logError("server stopped", err)
			}
			status <- svc.Status{State: svc.StopPending, WaitHint: uint32(stopWaitHint.Milliseconds())}
			h.stopApp()
			// No STOPPED of our own (as for a failed Start): the framework's
			// final update carries the exit code, so the SCM's failure actions
			// fire for a runtime crash too.
			return false, 1
		}
	}
}

// stopApp gives the app StopTimeout: long enough for a receipt in flight to
// finish and the order queue to drain (see serverApp.Stop).
func (h *serviceHandler) stopApp() {
	ctx, cancel := context.WithTimeout(context.Background(), StopTimeout)
	defer cancel()
	h.app.Stop(ctx)
}

func (h *serviceHandler) logError(msg string, err error) {
	logSvc := h.app.Logger()
	if logSvc == nil {
		return
	}
	logSvc.Error(msg, err)
}
