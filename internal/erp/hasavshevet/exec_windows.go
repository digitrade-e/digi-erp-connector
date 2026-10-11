//go:build windows

package hasavshevet

import (
	"context"
	"path/filepath"
	"strings"
)

// runImporter executes has.exe with the given parameter file in workDir.
// Returns the process exit code, combined stdout+stderr output, and any exec error.
// The single-worker OrderQueue guarantees only one invocation runs at a time.
func runImporter(ctx context.Context, hasExePath, paramFile, workDir string) (int, string, error) {
	args := []string{}
	if strings.TrimSpace(paramFile) != "" {
		args = append(args, paramFile)
	}
	return runCommand(ctx, 0, workDir, hasExePath, args...)
}

// runBatFile invokes a Masofon-generated BAT file via cmd.exe /C.
// The working directory is set to the BAT file's own directory so that relative
// paths inside the BAT (e.g. -p"digi.bat") resolve correctly.
// Returns the process exit code, combined output, and any exec error.
func runBatFile(ctx context.Context, batPath string) (int, string, error) {
	return runCommand(ctx, 0, filepath.Dir(batPath), "cmd.exe", "/C", batPath)
}

// runReceiptBatFile is runBatFile for the receipt BAT, with a wait delay so its
// timeout holds even when cmd.exe leaves the importer running (exec_cmd.go).
func runReceiptBatFile(ctx context.Context, batPath string) (int, string, error) {
	return runCommand(ctx, receiptBatWaitDelay, filepath.Dir(batPath), "cmd.exe", "/C", batPath)
}
