package main

import (
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/paths"
)

type uiLogger struct {
	logger *log.Logger
	file   *os.File
}

func newUILogger() *uiLogger {
	logPath, err := paths.UILogFilePath()
	if err != nil || logPath == "" {
		return &uiLogger{logger: log.New(io.Discard, "", 0)}
	}

	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return &uiLogger{logger: log.New(io.Discard, "", 0)}
	}

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
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
