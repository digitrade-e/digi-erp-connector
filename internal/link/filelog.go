package link

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// FileLog is the link's own log, link.log in the data directory, capped at
// 10 MB (the previous file is kept as link.log.1). On Windows every line also
// goes to the Event Log, source digi-erp-connector-link (plan 083 §3.2: the
// client's IT sees every command there).
type FileLog struct {
	mu    sync.Mutex
	path  string
	f     *os.File
	event eventSink
}

const maxLogSize = 10 << 20

// OpenFileLog opens (or creates) path for appending.
func OpenFileLog(path string) (*FileLog, error) {
	l := &FileLog{path: path, event: openEventSink(ServiceName)}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *FileLog) open() error {
	if fi, err := os.Stat(l.path); err == nil && fi.Size() > maxLogSize {
		_ = os.Rename(l.path, l.path+".1")
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	l.f = f
	return nil
}

func (l *FileLog) write(level, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if fi, err := l.f.Stat(); err == nil && fi.Size() > maxLogSize {
		_ = l.f.Close()
		if err := l.open(); err != nil {
			return
		}
	}
	_, _ = fmt.Fprintf(l.f, "%s [%s] %s\n", time.Now().UTC().Format(time.RFC3339), level, msg)
	if l.event != nil {
		l.event.write(level, msg)
	}
}

func (l *FileLog) Info(msg string)             { l.write("INFO", msg) }
func (l *FileLog) Warn(msg string)             { l.write("WARN", msg) }
func (l *FileLog) Error(msg string, err error) { l.write("ERROR", fmt.Sprintf("%s: %v", msg, err)) }

// Success satisfies the service runner's logger.
func (l *FileLog) Success(msg string) { l.write("INFO", msg) }

// Close closes the file and the Event Log handle.
func (l *FileLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.event != nil {
		l.event.close()
	}
	return l.f.Close()
}

type eventSink interface {
	write(level, msg string)
	close()
}
