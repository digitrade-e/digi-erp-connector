package logger

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/paths"
)

type LoggerService interface {
	Info(msg string)
	Error(msg string, err error)
	Warn(msg string)
	Success(msg string)
	Close() error
}

type service struct {
	logger *log.Logger
	file   *os.File
}

func New(cfg config.Config) (LoggerService, error) {
	logPath, err := filePath()
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, err
	}

	f, err := dataacl.OpenLog(logPath)
	if err != nil {
		return nil, err
	}

	var out io.Writer = f
	if cfg.Debug {
		// File MUST come first: io.MultiWriter aborts on the first error,
		// so if stderr is wrapped instead-of-first, a broken stderr cannot
		// block the file write. The swallowingWriter additionally guarantees
		// that any stderr failure (Windows services receive an invalid stderr
		// handle from the SCM) never propagates up — the log call still
		// succeeds and the file still gets the line.
		out = io.MultiWriter(f, &swallowingWriter{w: os.Stderr})
	}

	return &service{
		logger: log.New(out, "", log.LstdFlags),
		file:   f,
	}, nil
}

// swallowingWriter writes to the underlying writer but always reports
// success. Used to mirror logs to stderr in debug mode without letting
// an invalid stderr (Windows service mode) cause the surrounding
// io.MultiWriter to short-circuit and skip the real file write.
type swallowingWriter struct {
	w io.Writer
}

func (s *swallowingWriter) Write(p []byte) (int, error) {
	if s.w != nil {
		_, _ = s.w.Write(p)
	}
	return len(p), nil
}

// pathOverride, when set (SetFilePath), replaces server.log's path for New and
// NewBootstrap: the daemon logs to a fresh, uniquely named file when it could
// not move a foreign server.log aside.
var pathOverride atomic.Value // string

// SetFilePath makes New and NewBootstrap open path instead of server.log
// ("" restores server.log).
func SetFilePath(path string) { pathOverride.Store(path) }

func filePath() (string, error) {
	if p, _ := pathOverride.Load().(string); p != "" {
		return p, nil
	}
	return paths.LoggerFilePath()
}

// TimedWriter is implemented by a file logger that can write a line with a
// given timestamp — the daemon replays lines logged before server.log could
// be opened with the time they were logged at.
type TimedWriter interface {
	WriteAt(t time.Time, level, msg string)
}

// WriteAt writes one line stamped t (the format LstdFlags gives).
func (s *service) WriteAt(t time.Time, level, msg string) {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return
	}
	fmt.Fprintf(s.logger.Writer(), "%s [%s] %s\n", t.Format("2006/01/02 15:04:05"), level, msg)
}

// IsStderr reports whether l is the stderr logger (NewStderr, or the
// fallback of New/NewBootstrap) rather than a file.
func IsStderr(l LoggerService) bool {
	s, ok := l.(*service)
	return ok && s.file == nil
}

func NewStderr() LoggerService {
	return &service{
		logger: log.New(os.Stderr, "", log.LstdFlags),
	}
}

// NewBootstrap opens server.log directly using the OS-specific logger path,
// without requiring a parsed Config. It exists so the daemon can record
// startup failures (config.Load errors, missing dirs, permission issues)
// even when running as a Windows service where stderr is unavailable.
//
// On any failure (path resolution, mkdir, file open) it falls back to
// stderr — never returns nil.
func NewBootstrap() LoggerService {
	logPath, err := filePath()
	if err != nil || logPath == "" {
		return NewStderr()
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return NewStderr()
	}
	f, err := dataacl.OpenLog(logPath)
	if err != nil {
		return NewStderr()
	}
	return &service{
		logger: log.New(f, "", log.LstdFlags),
		file:   f,
	}
}

func (s *service) Info(msg string) {
	s.write("INFO", msg)
}

func (s *service) Error(msg string, err error) {
	msg = strings.TrimSpace(msg)
	if err != nil {
		if msg == "" {
			msg = err.Error()
		} else {
			msg = msg + ": " + err.Error()
		}
	}
	s.write("ERROR", msg)
}

func (s *service) Warn(msg string) {
	s.write("WARN", msg)
}

func (s *service) Success(msg string) {
	s.write("OK", msg)
}

func (s *service) Close() error {
	if s.file == nil {
		return nil
	}
	return s.file.Close()
}

func (s *service) write(level, msg string) {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return
	}
	s.logger.Printf("[%s] %s", level, msg)
}
