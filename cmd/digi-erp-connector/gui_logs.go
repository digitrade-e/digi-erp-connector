package main

// The GUI's log files live in the data directory, so the GUI walks the
// directory (dataacl.ProtectTree) before it opens any of them, and opens a
// fresh file object for one the walk had to reset or that is not exactly
// ours — never appending to a file someone else may hold open (R3-3). Kept
// free of walk so it is tested on every platform.

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"

	"github.com/digitrade-e/digi-erp-connector/internal/logger"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
)

// newUILoggerFn is a seam for the launch-order test.
var newUILoggerFn = newUILogger

// startGUILogs is the GUI's launch sequence for its logs, in order: walk the
// data directory (prepareGUIDataDir), then open ui.log — only if the walk
// allowed it — register it for a fresh reopen, and write the walk's own lines
// into it. serverOK says whether server.log may be opened once the config is
// loaded (openGUIServerLog).
func startGUILogs(dataDir, uiPath, serverPath string) (ui *uiLogger, serverOK bool, protectErr error) {
	early := &lineLogger{}
	ok, protectErr := prepareGUIDataDir(dataDir, []string{uiPath, serverPath}, early)
	ui = newUILoggerFn(ok[uiPath])
	heldLogs[uiPath] = ui.reopenFresh
	for _, line := range early.lines {
		ui.Printf("%s", line)
	}
	return ui, ok[serverPath], protectErr
}

// openGUIServerLog opens the GUI's server.log logger (open: logger.New with
// the loaded config) when startGUILogs allowed it, stderr otherwise, wrapped
// so a save's walk can reopen it fresh.
func openGUIServerLog(serverOK bool, serverPath string, open func() (logger.LoggerService, error)) *swapLogger {
	var inner logger.LoggerService
	if serverOK {
		if l, err := open(); err == nil {
			inner = l
		}
	}
	if inner == nil {
		inner = logger.NewStderr()
		inner.Warn("server.log could not be opened (see ui.log); using stderr")
	}
	s := &swapLogger{inner: inner, path: serverPath, open: open}
	heldLogs[serverPath] = s.reopenFresh
	return s
}

// heldLogs are the log files the GUI holds open, by path, each with what
// closes it, moves it aside and opens a fresh one. main registers them.
var heldLogs = map[string]func(){}

func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func inReset(path string, rep dataacl.Report) bool {
	for _, p := range rep.ResetPaths {
		if samePath(p, path) {
			return true
		}
	}
	return false
}

// prepareGUIDataDir runs at launch, before any log is opened: it creates the
// data directory if needed and walks it — handing what this (elevated) GUI
// created to Administrators — then decides for each log whether it may be
// opened. A directory the walk refuses gets no log at all (the error says
// why; saving is refused too).
func prepareGUIDataDir(dataDir string, logs []string, log logger.LoggerService) (map[string]bool, error) {
	ok := map[string]bool{}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		log.Error(fmt.Sprintf("could not create the data directory %s", dataDir), err)
		return ok, err
	}
	rep, err := guiProtectTreeFn(dataDir, dataacl.TreeOptions{ClaimForAdministrators: true})
	if err != nil {
		log.Error(fmt.Sprintf("could not protect the data directory %s; the GUI logs nowhere there and refuses to save", dataDir), err)
		return ok, err
	}
	for _, p := range logs {
		if p != "" {
			ok[p] = freshenLog(p, rep)
		}
	}
	return ok, nil
}

// freshenLog moves a log aside when the walk reset it or it is not exactly
// ours (dataacl.FileTrusted), so the next open creates a new file object; it
// reports whether the log may be opened. One that cannot be checked (the GUI
// not elevated) is left to the open, which fails the same way.
func freshenLog(path string, rep dataacl.Report) bool {
	if !inReset(path, rep) {
		trusted, err := guiFileTrustedFn(path)
		if err != nil || trusted {
			return true
		}
	}
	if _, err := guiMoveAsideFn(path); err != nil {
		return false
	}
	return true
}

// reopenHeldLogs reopens fresh every held log a save's walk had to reset.
func reopenHeldLogs(rep dataacl.Report) {
	for p, reopen := range heldLogs {
		if inReset(p, rep) {
			reopen()
		}
	}
}

// lineLogger keeps what prepareGUIDataDir says before ui.log is open.
type lineLogger struct{ lines []string }

func (l *lineLogger) Info(m string) { l.lines = append(l.lines, "INFO "+m) }
func (l *lineLogger) Warn(m string) { l.lines = append(l.lines, "WARN "+m) }
func (l *lineLogger) Success(m string) {
	l.lines = append(l.lines, "OK "+m)
}
func (l *lineLogger) Error(m string, err error) {
	l.lines = append(l.lines, fmt.Sprintf("ERROR %s: %v", m, err))
}
func (l *lineLogger) Close() error { return nil }

// swapLogger is the GUI's server.log logger, reopenable fresh in place.
type swapLogger struct {
	mu    sync.Mutex
	inner logger.LoggerService
	path  string
	open  func() (logger.LoggerService, error)
}

func (s *swapLogger) get() logger.LoggerService {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inner
}

func (s *swapLogger) Info(m string)             { s.get().Info(m) }
func (s *swapLogger) Warn(m string)             { s.get().Warn(m) }
func (s *swapLogger) Success(m string)          { s.get().Success(m) }
func (s *swapLogger) Error(m string, err error) { s.get().Error(m, err) }
func (s *swapLogger) Close() error              { return s.get().Close() }

// reopenFresh closes the file, moves it aside and opens a new one (stderr if
// that fails).
func (s *swapLogger) reopenFresh() {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.inner.Close()
	if _, err := guiMoveAsideFn(s.path); err == nil {
		if l, err := s.open(); err == nil {
			s.inner = l
			return
		}
	}
	s.inner = logger.NewStderr()
}
