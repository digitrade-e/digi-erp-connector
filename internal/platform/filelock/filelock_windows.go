//go:build windows

package filelock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// The whole file is locked as one byte range of maximal length.
const allBytes = ^uint32(0)

// openNoFollow is 0: Windows has no open flag that refuses a symlink (and
// creating one needs privileges there). Lock's Lstat check and the Stat of the
// opened handle apply instead.
const openNoFollow = 0

func lockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, allBytes, allBytes, ol)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, windows.ERROR_LOCK_VIOLATION), errors.Is(err, windows.ERROR_IO_PENDING):
		return ErrLocked
	default:
		return &os.PathError{Op: "LockFileEx", Path: f.Name(), Err: err}
	}
}

func unlockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, allBytes, allBytes, ol)
}
