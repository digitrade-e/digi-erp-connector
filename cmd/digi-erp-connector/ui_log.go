package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/paths"
)

type uiLogger struct {
	mu     sync.Mutex
	logger *log.Logger
	file   *os.File
	path   string
}

// newUILogger opens ui.log for appending (dataacl.OpenLog: never through a
// reparse point, never into a hard link), or logs nowhere when usable is false
// (prepareLog could not move a foreign ui.log aside) or the open fails.
func newUILogger(usable bool) *uiLogger {
	logPath, err := paths.UILogFilePath()
	if !usable || err != nil || logPath == "" {
		return &uiLogger{logger: log.New(io.Discard, "", 0)}
	}

	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return &uiLogger{logger: log.New(io.Discard, "", 0)}
	}

	f, err := dataacl.OpenLog(logPath)
	if err != nil {
		return &uiLogger{logger: log.New(io.Discard, "", 0)}
	}

	return &uiLogger{
		logger: log.New(f, "", log.LstdFlags),
		file:   f,
		path:   logPath,
	}
}

// Write makes the uiLogger itself the standard logger's output, so a reopen
// redirects it too.
func (l *uiLogger) Write(p []byte) (int, error) {
	if l == nil {
		return len(p), nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return len(p), nil
	}
	return l.file.Write(p)
}

// reopenFresh closes ui.log, moves it aside and opens a new one (nothing if
// either fails): a save's walk reset it, so the old file object may be held
// by someone else.
func (l *uiLogger) reopenFresh() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return
	}
	_ = l.file.Close()
	l.file = nil
	l.logger.SetOutput(io.Discard)
	if _, err := guiMoveAsideFn(l.path); err != nil {
		return
	}
	if f, err := dataacl.OpenLog(l.path); err == nil {
		l.file = f
		l.logger.SetOutput(f)
	}
}

func (l *uiLogger) Printf(format string, args ...any) {
	if l == nil || l.logger == nil {
		return
	}
	l.mu.Lock()
	lg := l.logger
	l.mu.Unlock()
	lg.Printf(format, args...)
}

func (l *uiLogger) Writer() io.Writer {
	if l == nil {
		return io.Discard
	}
	return l
}

func (l *uiLogger) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
}
