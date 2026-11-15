package link

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
)

// Runner runs digi-erp-connector-provision.exe with argv and stdin and returns
// its stdout (at most max bytes) and exit code.
type Runner interface {
	Run(ctx context.Context, argv []string, stdin []byte, max int) (stdout []byte, exit int, err error)
}

// ExecRunner runs the installed provision.exe. Stderr is discarded: provision
// writes its one redacted envelope to stdout.
type ExecRunner struct {
	Path string
}

func (r ExecRunner) Run(ctx context.Context, argv []string, stdin []byte, max int) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, r.Path, argv...)
	cmd.Stdin = bytes.NewReader(stdin)
	var out limited
	out.max = max
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return out.b.Bytes(), exit.ExitCode(), nil
	}
	if err != nil {
		return nil, -1, err
	}
	return out.b.Bytes(), 0, nil
}

// limited keeps the first max bytes and drops the rest.
type limited struct {
	b   bytes.Buffer
	max int
}

func (l *limited) Write(p []byte) (int, error) {
	if room := l.max - l.b.Len(); room > 0 {
		if len(p) > room {
			l.b.Write(p[:room])
		} else {
			l.b.Write(p)
		}
	}
	return len(p), nil
}
