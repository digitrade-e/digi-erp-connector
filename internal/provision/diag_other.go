//go:build !windows

package provision

import (
	"errors"
	"syscall"
	"time"
)

// platformEvents: the Event Log is Windows only.
func platformEvents(string, time.Time, int) ([]EventRecord, error) {
	return nil, errors.New("the Event Log is read on Windows only")
}

// diskSpace is the free and total bytes of the file system holding path.
func diskSpace(path string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return st.Bavail * uint64(st.Bsize), st.Blocks * uint64(st.Bsize), nil
}
