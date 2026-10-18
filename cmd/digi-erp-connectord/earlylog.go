package main

import (
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/logger"
)

const isWindows = runtime.GOOS == "windows"

type earlyEntry struct {
	at    time.Time
	level string
	msg   string
}

// earlyLog is Start's log until server.log may be opened — only after the
// data directory is protected (an earlier open would keep appending to a file
// object someone else may hold, or follow a link). It writes to stderr at
// once (nothing, under the service manager) and keeps every line, with its
// time, for replayInto — or for the refusal report when the start is refused
// and no file log is ever opened.
type earlyLog struct {
	mu      sync.Mutex
	entries []earlyEntry
	stderr  logger.LoggerService
}

func newEarlyLog() *earlyLog { return &earlyLog{stderr: logger.NewStderr()} }

func (e *earlyLog) add(level, msg string, echo func(logger.LoggerService)) {
	e.mu.Lock()
	e.entries = append(e.entries, earlyEntry{at: time.Now(), level: level, msg: strings.TrimSpace(msg)})
	e.mu.Unlock()
	echo(e.stderr)
}

func (e *earlyLog) Info(m string)    { e.add("INFO", m, func(l logger.LoggerService) { l.Info(m) }) }
func (e *earlyLog) Warn(m string)    { e.add("WARN", m, func(l logger.LoggerService) { l.Warn(m) }) }
func (e *earlyLog) Success(m string) { e.add("OK", m, func(l logger.LoggerService) { l.Success(m) }) }
func (e *earlyLog) Error(m string, err error) {
	full := m
	if err != nil {
		full = m + ": " + err.Error()
	}
	e.add("ERROR", full, func(l logger.LoggerService) { l.Error(m, err) })
}
func (e *earlyLog) Close() error { return nil }

// lines is the buffer as text, each line stamped.
func (e *earlyLog) lines() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.entries))
	for _, x := range e.entries {
		out = append(out, x.at.Format("2006/01/02 15:04:05")+" ["+x.level+"] "+x.msg)
	}
	return out
}

// shouldReplay is false for the stderr fallback: the lines are there already.
func shouldReplay(l logger.LoggerService) bool { return !logger.IsStderr(l) }

// replayInto writes every line logged so far into l, in order, with the time
// it was logged at when l can take one (logger.TimedWriter).
func (e *earlyLog) replayInto(l logger.LoggerService) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !shouldReplay(l) {
		e.entries = nil
		return
	}
	tw, timed := l.(logger.TimedWriter)
	for _, x := range e.entries {
		switch {
		case timed:
			tw.WriteAt(x.at, x.level, x.msg)
		case x.level == "ERROR":
			l.Error(x.msg, nil)
		case x.level == "WARN":
			l.Warn(x.msg)
		case x.level == "OK":
			l.Success(x.msg)
		default:
			l.Info(x.msg)
		}
	}
	e.entries = nil
}
