package filelock

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestLock_SecondHolderIsRefusedUntilTheFirstLetsGo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writer.lock")
	a, err := Lock(path)
	if err != nil {
		t.Fatalf("first Lock: %v", err)
	}
	if b, err := Lock(path); !errors.Is(err, ErrLocked) {
		if b != nil {
			_ = b.Unlock()
		}
		t.Fatalf("second Lock = %v; want ErrLocked", err)
	}
	if err := a.Unlock(); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	c, err := Lock(path)
	if err != nil {
		t.Fatalf("Lock after Unlock: %v", err)
	}
	_ = c.Unlock()
}

// A lock file left behind by a process that died is not a lock: the OS lock
// went with the process. (O_EXCL alone would refuse forever here.)
func TestLock_AStaleFileIsNotALock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writer.lock")
	if err := os.WriteFile(path, []byte("12345\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Lock(path)
	if err != nil {
		t.Fatalf("Lock over a stale file: %v", err)
	}
	_ = l.Unlock()
}

func TestLock_UnlockTwiceIsHarmless(t *testing.T) {
	l, err := Lock(filepath.Join(t.TempDir(), "writer.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := l.Unlock(); err != nil {
		t.Fatalf("second Unlock: %v", err)
	}
}

// Two processes, as two connectors on one data directory would be.
func TestLock_AnotherProcessHoldingItRefusesThisOne(t *testing.T) {
	if path := os.Getenv("FILELOCK_HELPER_PATH"); path != "" {
		holdForParent(path)
		return
	}
	path := filepath.Join(t.TempDir(), "writer.lock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestLock_AnotherProcessHoldingItRefusesThisOne$")
	cmd.Env = append(os.Environ(), "FILELOCK_HELPER_PATH="+path)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "held\n" {
		_ = cmd.Process.Kill()
		t.Fatalf("helper said %q, %v", line, err)
	}
	if l, err := Lock(path); !errors.Is(err, ErrLocked) {
		if l != nil {
			_ = l.Unlock()
		}
		_ = cmd.Process.Kill()
		t.Fatalf("Lock while another process holds it = %v; want ErrLocked", err)
	}
	_ = stdin.Close() // the helper exits; the OS drops its lock
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("helper did not exit")
	}
	l, err := Lock(path)
	if err != nil {
		t.Fatalf("Lock after the other process exited: %v", err)
	}
	_ = l.Unlock()
}

func holdForParent(path string) {
	l, err := Lock(path)
	if err != nil {
		os.Stdout.WriteString("error " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Stdout.WriteString("held\n")
	_, _ = io.Copy(io.Discard, os.Stdin) // until the parent closes stdin
	_ = l
	os.Exit(0) // exit without Unlock: the OS must release it
}

// The lock file is opened, never followed: a symlink planted under its name —
// to a file the service can write — must leave the target untouched, and a
// dangling one must not create its target. Lock refuses both.
func TestLock_NeverFollowsASymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("precious\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "writer.lock")
	if err := os.Symlink(victim, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	if l, err := Lock(link); err == nil {
		_ = l.Unlock()
		t.Error("Lock followed a symlink")
	}
	if got, _ := os.ReadFile(victim); string(got) != "precious\n" {
		t.Errorf("the symlink's target was changed to %q", got)
	}

	dangling := filepath.Join(dir, "other.lock")
	target := filepath.Join(dir, "created-elsewhere")
	if err := os.Symlink(target, dangling); err != nil {
		t.Fatal(err)
	}
	if l, err := Lock(dangling); err == nil {
		_ = l.Unlock()
		t.Error("Lock followed a dangling symlink")
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a dangling symlink's target was created (%v)", err)
	}
}

// An existing lock file's contents are not the lock's business: it is never
// rewritten.
func TestLock_LeavesTheFileContentsAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writer.lock")
	if err := os.WriteFile(path, []byte("left by someone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	// Read after Unlock: on Windows the byte-range lock also refuses reads
	// through other handles while it is held.
	if err := l.Unlock(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "left by someone\n" {
		t.Errorf("Lock rewrote the file to %q (%v)", got, err)
	}
}
