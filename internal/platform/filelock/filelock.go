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
//
// The file is never followed and never written: a symlink under its name is
// refused (O_NOFOLLOW on Unix; an Lstat check on both, which also refuses a
// directory or device), so a link planted there cannot point the open — or a
// create — at another file. Its contents are left as they are.
func Lock(path string) (*Held, error) {
	if fi, err := os.Lstat(path); err == nil && !fi.Mode().IsRegular() {
		return nil, &os.PathError{Op: "lock", Path: path, Err: errNotRegular}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|openNoFollow, 0o600)
	if err != nil {
		return nil, err
	}
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, &os.PathError{Op: "lock", Path: path, Err: errNotRegular}
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Held{f: f}, nil
}

var errNotRegular = errors.New("not a regular file (a symlink or something else is in its place)")

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
