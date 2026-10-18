//go:build windows

//go:generate rsrc -manifest app.manifest -o rsrc.syso

// Command digi-erp-connector is the Windows configuration UI for the connector.
// It edits config.yaml, stores the DB password through DPAPI, and controls the
// digi-erp-connectord service. It serves no HTTP traffic itself — the daemon
// does that.
//
// The window and its behaviour live in form.go, form_config.go, actions.go and
// service_control.go.
package main

import (
	"fmt"
	"log"
	"os"
	"runtime"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/logger"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/paths"
)

func main() {
	// walk requires all UI work on one OS thread.
	runtime.LockOSThread()

	// Before any log is opened in it, the data directory is created and walked
	// (gui_logs.go): a log the walk had to reset, or one not exactly ours, is
	// moved aside so a fresh file is opened; a directory the walk refuses gets
	// no log, and saving is refused until it is fixed.
	uiPath, _ := paths.UILogFilePath()
	serverPath, _ := paths.LoggerFilePath()
	early := &lineLogger{}
	logsOK, protectErr := prepareGUIDataDir(paths.DataDir(), []string{uiPath, serverPath}, early)
	uiLogOK, serverLogOK := logsOK[uiPath], logsOK[serverPath]

	uiLog := newUILogger(uiLogOK)
	defer uiLog.Close()
	heldLogs[uiPath] = uiLog.reopenFresh
	for _, line := range early.lines {
		uiLog.Printf("%s", line)
	}

	// The GUI has no console when launched from the shortcut, so a panic would
	// otherwise vanish. Log it and tell the operator where to look.
	defer func() {
		if rec := recover(); rec != nil {
			uiLog.Printf("panic: %v", rec)
			uiStartupAlert(fmt.Errorf("unexpected error; see UI log for details"))
		}
	}()

	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.SetOutput(uiLog.Writer()) // the uiLogger itself, so a reopen redirects it

	uiLog.Printf("startup begin")
	if session := os.Getenv("SESSIONNAME"); session != "" {
		uiLog.Printf("session: %s", session)
	}

	// Refuse to run as a service or in a non-interactive session, where the
	// window could never be shown.
	if err := uiStartupGuard(); err != nil {
		uiLog.Printf("startup blocked: %v", err)
		uiStartupAlert(err)
		return
	}

	if exe, err := os.Executable(); err == nil {
		uiLog.Printf("exe: %s", exe)
	}
	if wd, err := os.Getwd(); err == nil {
		uiLog.Printf("working dir: %s", wd)
	}

	// A missing config is normal on first run and yields defaults; a corrupt one
	// is reported in the window rather than blocking startup, so the operator can
	// see the fields and fix them.
	cfg, cfgErr := config.LoadOrDefault()
	if cfgErr != nil {
		uiLog.Printf("config load error: %v", cfgErr)
	} else {
		uiLog.Printf("config loaded")
	}

	openServerLog := func() (logger.LoggerService, error) { return logger.New(cfg) }
	inner, logErr := logger.LoggerService(nil), error(nil)
	if serverLogOK {
		inner, logErr = openServerLog()
	} else {
		logErr = fmt.Errorf("server.log could not be prepared (see ui.log)")
	}
	if logErr != nil {
		inner = logger.NewStderr()
		inner.Warn("logger init failed; using stderr")
	}
	logSvc := &swapLogger{inner: inner, path: serverPath, open: openServerLog}
	heldLogs[serverPath] = logSvc.reopenFresh
	defer logSvc.Close()

	uiLog.Printf("building window")
	form, err := newMainForm(cfg, logSvc)
	if err != nil {
		uiLog.Printf("window create error: %v", err)
		uiStartupAlert(err)
		return
	}

	// Previously this checked the (always nil) window error instead of the config
	// error, so a corrupt config.yaml was never reported in the UI.
	if cfgErr != nil {
		form.setStatus("Error loading config: " + cfgErr.Error())
	}
	if protectErr != nil {
		form.setStatus("The data directory could not be protected, so saving is refused until it is fixed: " + protectErr.Error())
	}

	uiLog.Printf("run loop")
	form.MainWindow.Run()
	uiLog.Printf("run loop exit")
}
