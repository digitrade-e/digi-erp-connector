// Package atomicfile replaces file contents without ever leaving a partial or
// missing file on disk.
//
// Every file this connector owns is read at startup and rewritten while the
// service is live — config.yaml, queries.json, secrets/*.bin. A crash or power
// loss midway through a plain os.WriteFile would leave a truncated config that
// stops the daemon from starting, so writes go to a temp file in the same
// directory, are flushed to disk, and are then renamed over the target. Rename
// within a directory is atomic on both Windows and Unix: readers see either the
// old contents or the new ones.
package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

// dirPerm is used when the target's directory does not exist yet.
const dirPerm = 0o755

// The Windows errors a rename over a file someone else holds open returns.
// Go's os.Open does not share delete access, so a reader in this process (an
// index lookup), Defender or a backup agent reading the target makes the
// rename fail until it lets go. They are compared by number, and only on
// Windows: elsewhere these numbers mean other things.
const (
	errorAccessDenied     syscall.Errno = 5  // ERROR_ACCESS_DENIED
	errorSharingViolation syscall.Errno = 32 // ERROR_SHARING_VIOLATION
)

// renameAttempts bounds the retries of such a rename: with renameBackoff
// doubling between attempts it waits at most about 0.8 s in all.
const renameAttempts = 6

// Seams for tests.
var (
	osRename      = os.Rename
	renameGOOS    = runtime.GOOS
	renameBackoff = 25 * time.Millisecond
)

// Write atomically replaces the file at path with data.
//
// perm applies to the new file. On Windows only the read-only bit is
// meaningful, which is why callers must not rely on it for confidentiality —
// the data directory's ACLs do that job.
func Write(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("atomicfile: create directory %s: %w", dir, err)
	}

	// The temp file must share the target's directory: rename is only atomic
	// within a filesystem, and a temp dir may well be on another one.
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("atomicfile: create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()

	if err := writeAndClose(tmp, data, perm); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("atomicfile: write %s: %w", tmpName, err)
	}

	// No Remove of the target first: os.Rename replaces an existing file on
	// both Unix and Windows, and removing it would open a window in which the
	// file does not exist at all.
	if err := renameRetrying(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("atomicfile: replace %s: %w", path, err)
	}

	return nil
}

// renameRetrying is os.Rename, retried briefly while Windows reports the
// target held open (see errorSharingViolation). Any other failure — or the
// same one once the attempts run out — is returned at once, as before.
func renameRetrying(oldpath, newpath string) error {
	delay := renameBackoff
	var err error
	for attempt := 1; ; attempt++ {
		err = osRename(oldpath, newpath)
		if err == nil || attempt == renameAttempts || !transientRenameError(renameGOOS, err) {
			return err
		}
		time.Sleep(delay)
		delay *= 2
	}
}

func transientRenameError(goos string, err error) bool {
	return goos == "windows" && (errors.Is(err, errorAccessDenied) || errors.Is(err, errorSharingViolation))
}

// writeAndClose writes data, flushes it to the device and closes the file. It
// always closes tmp, so the caller only has to remove it on error.
func writeAndClose(tmp *os.File, data []byte, perm os.FileMode) error {
	// Chmod before the contents land so the file is never briefly readable
	// with wider permissions than intended.
	if err := tmp.Chmod(perm); err != nil {
		// Not fatal: some filesystems (and Windows) do not support this.
		_ = err
	}

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	// Sync before rename: without it the rename can be durable while the
	// contents are not, which is exactly the truncated-file case we are
	// avoiding.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	return tmp.Close()
}
