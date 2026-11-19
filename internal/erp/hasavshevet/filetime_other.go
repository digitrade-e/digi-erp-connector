//go:build !windows

package hasavshevet

import (
	"os"
	"time"
)

// fileCreatedAt has no creation time to read off Windows: the write time
// stands in (tests only; the importer runs on Windows).
func fileCreatedAt(st os.FileInfo) time.Time { return st.ModTime() }
