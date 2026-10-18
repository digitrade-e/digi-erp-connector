package main

import (
	"runtime"
	"sync"

	"github.com/digitrade-e/digi-erp-connector/internal/logger"
)

const isWindows = runtime.GOOS == "windows"

// earlyLog is Start's log until server.log may be opened — which is only
// after the data directory is protected (an earlier open would keep appending
// to a file object someone else may hold). It writes to stderr (nothing, under
// the service manager) and keeps every line for replayInto.
type earlyLog struct {
	mu     sync.Mutex
	lines  []func(logger.LoggerService)
	stderr logger.LoggerService
}

func newEarlyLog() *earlyLog { return &earlyLog{stderr: logger.NewStderr()} }

func (e *earlyLog) add(f func(logger.LoggerService)) {
	e.mu.Lock()
	e.lines = append(e.lines, f)
	e.mu.Unlock()
	f(e.stderr)
}

func (e *earlyLog) Info(m string)    { e.add(func(l logger.LoggerService) { l.Info(m) }) }
func (e *earlyLog) Warn(m string)    { e.add(func(l logger.LoggerService) { l.Warn(m) }) }
func (e *earlyLog) Success(m string) { e.add(func(l logger.LoggerService) { l.Success(m) }) }
func (e *earlyLog) Error(m string, err error) {
	e.add(func(l logger.LoggerService) { l.Error(m, err) })
}
func (e *earlyLog) Close() error { return nil }

// replayInto writes every line logged so far into l, in order.
func (e *earlyLog) replayInto(l logger.LoggerService) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, f := range e.lines {
		f(l)
	}
	e.lines = nil
}
