package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/autostart"
)

func main() {
	if runAsService() {
		return
	}

	app := &serverApp{}
	if err := app.Start(); err != nil {
		os.Exit(1)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-app.Errors():
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			app.Logger().Error("server stopped", err)
			os.Exit(1)
		}
	case sig := <-sigCh:
		app.Logger().Info(fmt.Sprintf("shutdown signal: %s", sig))
		ctx, cancel := context.WithTimeout(context.Background(), autostart.StopTimeout)
		defer cancel()
		app.Stop(ctx)
		if err := <-app.Errors(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			app.Logger().Error("server stopped", err)
			os.Exit(1)
		}
	}
}
