//go:build !windows

package hasavshevet

import "context"

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
