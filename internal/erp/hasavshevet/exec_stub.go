//go:build !windows

package hasavshevet

import (
	"context"
	"errors"
)

// runImporter is a no-op on non-Windows platforms.
// Hasavshevet (has.exe) only runs on Windows; on Linux/macOS the files are
// still written for testing or cross-compiled builds.
func runImporter(_ context.Context, _, _, _ string) (int, string, error) {
	return 0, "Hasavshevet execution skipped (non-Windows build)", nil
}

// runBatFile is a no-op on non-Windows platforms.
func runBatFile(_ context.Context, _ string) (int, string, error) {
	return 0, "BAT execution skipped (non-Windows build)", nil
}

// errReceiptImportNotOnWindows is what the receipt runners answer off
// Windows. It is an error, not a quiet exit 0: the writer records the import
// as failed with this reason, never as "ok" for a receipt nobody imported.
var errReceiptImportNotOnWindows = errors.New("the receipt import runs only on Windows; not run")

// runReceiptBatFile cannot run off Windows; it says so (see above).
func runReceiptBatFile(_ context.Context, _ string) (int, string, error) {
	return -1, "", errReceiptImportNotOnWindows
}

// runReceiptImporter cannot run off Windows; it says so (see above).
func runReceiptImporter(_ context.Context, _, _, _ string) (int, string, error) {
	return -1, "", errReceiptImportNotOnWindows
}
