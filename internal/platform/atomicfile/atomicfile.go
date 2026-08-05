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
	"fmt"
	"os"
	"path/filepath"
)

// dirPerm is used when the target's directory does not exist yet.
const dirPerm = 0o755

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
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("atomicfile: replace %s: %w", path, err)
	}

	return nil
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
