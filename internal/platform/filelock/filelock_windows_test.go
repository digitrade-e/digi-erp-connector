//go:build windows

package filelock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// The lock is the kernel's byte-range lock, not something only this package
// honours: a handle opened by anyone else that asks LockFileEx for the same
// range is refused while Lock holds it, and granted once it is released.
func TestLock_IsARealLockFileExLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writer.lock")
	held, err := Lock(path)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	other, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		_ = held.Unlock()
		t.Fatalf("open a second handle: %v", err)
	}
	defer other.Close()
	try := func() error {
		return windows.LockFileEx(windows.Handle(other.Fd()),
			windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, allBytes, allBytes, new(windows.Overlapped))
	}
	if err := try(); !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		_ = held.Unlock()
		t.Fatalf("LockFileEx on a second handle while held = %v; want ERROR_LOCK_VIOLATION", err)
	}
	if err := held.Unlock(); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if err := try(); err != nil {
		t.Fatalf("LockFileEx after Unlock = %v; want granted", err)
	}
	_ = windows.UnlockFileEx(windows.Handle(other.Fd()), 0, allBytes, allBytes, new(windows.Overlapped))
}

// Unlike Unix (where a deleted lock file lets a second process lock a fresh
// one), Windows refuses to delete the file while the lock's handle is open:
// os.OpenFile does not grant FILE_SHARE_DELETE.
func TestLock_AHeldLockFileCannotBeDeleted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writer.lock")
	held, err := Lock(path)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if err := os.Remove(path); err == nil {
		_ = held.Unlock()
		t.Fatal("a held lock file was deleted")
	}
	if err := held.Unlock(); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Errorf("remove after Unlock: %v", err)
	}
}
