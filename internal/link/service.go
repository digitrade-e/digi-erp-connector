package link

import (
	"context"
	"sync"

	"github.com/digitrade-e/digi-erp-connector/internal/formatfloor"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/autostart"
)

// App runs the link as a service (autostart.ServiceApp).
type App struct {
	Machine Machine
	Log     *FileLog

	cancel context.CancelFunc
	done   chan struct{}
	errs   chan error
	once   sync.Once
}

// Start builds the configuration and starts the link. A box not paired yet
// still starts: it waits for a pairing code in link.yaml.
func (a *App) Start() error {
	a.errs = make(chan error, 1)
	if err := a.Machine.Protect(); err != nil {
		a.Log.Error("link: cannot start", err)
		return err
	}
	// Plan 088 D11: every build raises DataDir\format-floor at start (the daemon refuses to start when it cannot; the
	// link stays up whatever, so the box can still be reached).
	if _, err := formatfloor.Raise(a.Machine.DataDir, formatfloor.RollbackFloor); err != nil {
		a.Log.Error("link: recording the format floor in DataDir\\format-floor", err)
	}
	cfg, err := a.Machine.Config()
	if err != nil {
		a.Log.Error("link: cannot start", err)
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel, a.done = cancel, make(chan struct{})
	l := New(cfg)
	a.Log.Info("link: started, build " + a.Machine.Build + ", fingerprint " + cfg.Keys.Public().Fingerprint())
	go func() {
		defer close(a.done)
		_ = l.Run(ctx)
	}()
	return nil
}

// Stop ends the link. A command already running keeps running to its end:
// provision.exe is not killed with the link (plan 083 §8 A10).
func (a *App) Stop(ctx context.Context) {
	a.once.Do(func() {
		if a.cancel == nil {
			return
		}
		a.cancel()
		select {
		case <-a.done:
		case <-ctx.Done():
		}
		a.Log.Info("link: stopped")
	})
}

func (a *App) Errors() <-chan error { return a.errs }

// Logger is the service runner's logger.
func (a *App) Logger() autostart.Logger { return a.Log }
