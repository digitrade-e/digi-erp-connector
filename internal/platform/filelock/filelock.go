// Package filelock is an exclusive, non-blocking, process-wide lock on a file:
// flock(2) on Unix, LockFileEx on Windows.
//
// The lock is the operating system's, not the file's existence: it goes away
// when its holder unlocks or dies, so a file left behind by a crash never
// locks anyone out (O_CREATE|O_EXCL alone would). Two opens of the same path
// conflict even inside one process.
package filelock

import (
	"errors"
	"os"
	"strconv"
	"sync"
)

// ErrLocked means another holder — another process, or another Lock in this
// one — has the lock.
var ErrLocked = errors.New("file is locked by another holder")

// Held is a lock this process holds. Unlock releases it.
type Held struct {
	mu sync.Mutex
	f  *os.File
}

// Lock takes the exclusive lock on path, creating the file (0600) if needed.
// It never waits: a lock someone else holds is ErrLocked.
func Lock(path string) (*Held, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	// For a person looking: which process holds it. Best effort, never read.
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return &Held{f: f}, nil
}

// Unlock releases the lock. The file stays; a second Unlock does nothing.
func (l *Held) Unlock() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := unlockFile(l.f)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}
