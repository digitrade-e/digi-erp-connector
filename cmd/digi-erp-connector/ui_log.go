package main

import (
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/paths"
)

type uiLogger struct {
	logger *log.Logger
	file   *os.File
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
	}
}

func (l *uiLogger) Printf(format string, args ...any) {
	if l == nil || l.logger == nil {
		return
	}
	l.logger.Printf(format, args...)
}

func (l *uiLogger) Writer() io.Writer {
	if l == nil || l.file == nil {
		return io.Discard
	}
	return l.file
}

func (l *uiLogger) Close() {
	if l == nil || l.file == nil {
		return
	}
	_ = l.file.Close()
}
