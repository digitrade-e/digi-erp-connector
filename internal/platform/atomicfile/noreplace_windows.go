//go:build windows

package atomicfile

import (
	"time"

	"golang.org/x/sys/windows"
)

// publishNoReplace renames oldpath to newpath unless newpath exists: one
// MoveFileEx without MOVEFILE_REPLACE_EXISTING, atomic within a volume. An
// existing newpath is ERROR_ALREADY_EXISTS / ERROR_FILE_EXISTS, which
// errors.Is(err, fs.ErrExist) matches. The rename is retried briefly while the
// temp file is held (Defender scanning it), like renameRetrying.
func publishNoReplace(oldpath, newpath string) error {
	from, err := windows.UTF16PtrFromString(oldpath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(newpath)
	if err != nil {
		return err
	}
	delay := renameBackoff
	for attempt := 1; ; attempt++ {
		err = windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH)
		if err == nil || attempt == renameAttempts || !transientRenameError("windows", err) {
			return err
		}
		time.Sleep(delay)
		delay *= 2
	}
}
