//go:build windows

package link

import (
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Review m5, on a real process: killProcess ends a process only through the handle its image was checked on — a wrong
// image leaves it running, the right one ends it.
func TestKillProcessChecksTheImageOnItsOwnHandle(t *testing.T) {
	ping, err := exec.LookPath("ping.exe")
	if err != nil {
		t.Skip("no ping.exe")
	}
	cmd := exec.Command(ping, "-n", "60", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	pid := uint32(cmd.Process.Pid)
	img := imagePath(pid)
	if !samePath(img, ping) && !samePath(filepath.Base(img), filepath.Base(ping)) {
		t.Fatalf("image %q, started %q", img, ping)
	}
	if err := killProcess(pid, filepath.Join(filepath.Dir(img), "digi-erp-connector.exe")); !errors.Is(err, errNotThatProcess) {
		t.Fatalf("another image: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		t.Fatal("a process whose image is not the one named was ended")
	case <-time.After(300 * time.Millisecond):
	}
	if err := killProcess(pid, img); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the process named by its image was not ended")
	}
}
