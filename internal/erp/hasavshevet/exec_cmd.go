package hasavshevet

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// receiptBatWaitDelay bounds how long a receipt BAT run may outlive its
// context. On Windows the BAT runs under cmd.exe, and killing cmd.exe when the
// deadline passes does not kill the importer it started: that process keeps the
// output pipe open, and reading it would block until the importer exits —
// turning the 90 s cap into no cap at all. WaitDelay closes the pipe and
// returns once this much time has passed after the kill.
const receiptBatWaitDelay = 5 * time.Second

// runCommand runs name with args in dir (empty: the current directory) and
// returns its exit code, its combined output trimmed, and any exec error.
// waitDelay is exec.Cmd.WaitDelay; zero keeps os/exec's default (wait for the
// pipes however long that takes), which is what the order importer has always
// done.
func runCommand(ctx context.Context, waitDelay time.Duration, dir, name string, args ...string) (int, string, error) {
	return runCommandCancel(ctx, waitDelay, nil, dir, name, args...)
}

// runCommandCancel is runCommand with cancel, when non-nil, building the
// exec.Cmd.Cancel used when ctx ends (nil: os/exec's Kill of the process it
// started). treeCancel (exec_windows.go) kills the whole process tree: the
// order importer's timeout needs it, since killing cmd.exe leaves the has.exe
// it started running — reading the shared IMOVEIN.doc the next order rewrites.
func runCommandCancel(ctx context.Context, waitDelay time.Duration, cancel func(*exec.Cmd) func() error, dir, name string, args ...string) (int, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.WaitDelay = waitDelay
	if cancel != nil {
		cmd.Cancel = cancel(cmd)
	}

	out, err := cmd.CombinedOutput()
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	return exitCode, strings.TrimSpace(string(out)), err
}
