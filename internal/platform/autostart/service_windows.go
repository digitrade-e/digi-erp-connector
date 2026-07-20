//go:build windows

package autostart

import (
	"context"
	"errors"
	"net/http"
	"time"

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
	status <- svc.Status{State: svc.StartPending}

	if err := h.app.Start(); err != nil {
		status <- svc.Status{State: svc.Stopped}
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
				status <- svc.Status{State: svc.StopPending}
				h.stopApp()
				status <- svc.Status{State: svc.Stopped}
				return false, 0
			}
		case err := <-h.app.Errors():
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				h.logError("server stopped", err)
			}
			status <- svc.Status{State: svc.StopPending}
			h.stopApp()
			status <- svc.Status{State: svc.Stopped}
			return false, 1
		}
	}
}

func (h *serviceHandler) stopApp() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
