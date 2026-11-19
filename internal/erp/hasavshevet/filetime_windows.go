//go:build windows

package hasavshevet

import (
	"os"
	"syscall"
	"time"
)

// fileCreatedAt is when the file was created. Masofon makes <bat>.IsWorking
// with CopyFile, which keeps the BAT's write time: only the creation time
// says when the marker was made.
func fileCreatedAt(st os.FileInfo) time.Time {
	if a, ok := st.Sys().(*syscall.Win32FileAttributeData); ok {
		return time.Unix(0, a.CreationTime.Nanoseconds())
	}
	return st.ModTime()
}
