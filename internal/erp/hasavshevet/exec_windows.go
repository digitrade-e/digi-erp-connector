//go:build windows

package hasavshevet

import (
	"context"
	"os/exec"
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
	cmd := exec.CommandContext(ctx, hasExePath, args...)
	cmd.Dir = workDir

	out, err := cmd.CombinedOutput()
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	return exitCode, strings.TrimSpace(string(out)), err
}

// runBatFile invokes a Masofon-generated BAT file via cmd.exe /C.
// The working directory is set to the BAT file's own directory so that relative
// paths inside the BAT (e.g. -p"digi.bat") resolve correctly.
// Returns the process exit code, combined output, and any exec error.
func runBatFile(ctx context.Context, batPath string) (int, string, error) {
	cmd := exec.CommandContext(ctx, "cmd.exe", "/C", batPath)
	cmd.Dir = filepath.Dir(batPath)

	out, err := cmd.CombinedOutput()
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	return exitCode, strings.TrimSpace(string(out)), err
}
