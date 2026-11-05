package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// withRename swaps the rename seam (and the OS it believes it runs on) for
// one test.
func withRename(t *testing.T, goos string, fn func(oldpath, newpath string) error) {
	t.Helper()
	oldRename, oldGOOS, oldBackoff := osRename, renameGOOS, renameBackoff
	osRename, renameGOOS, renameBackoff = fn, goos, time.Millisecond
	t.Cleanup(func() { osRename, renameGOOS, renameBackoff = oldRename, oldGOOS, oldBackoff })
}

// On Windows a rename over a file another process has open without
// FILE_SHARE_DELETE (a reader, Defender, a backup agent) fails with
// ERROR_ACCESS_DENIED or ERROR_SHARING_VIOLATION until it lets go: Write
// retries those briefly instead of failing the write.
func TestWriteRetriesATransientWindowsRenameFailure(t *testing.T) {
	for _, transient := range []syscall.Errno{errorAccessDenied, errorSharingViolation} {
		path := filepath.Join(t.TempDir(), "entry.json")
		calls := 0
		withRename(t, "windows", func(oldpath, newpath string) error {
			calls++
			if calls < 3 {
				return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: transient}
			}
			return os.Rename(oldpath, newpath)
		})
		if err := Write(path, []byte("new"), 0o600); err != nil {
			t.Fatalf("Write with errno %d twice: %v", transient, err)
		}
		if calls != 3 {
			t.Errorf("rename called %d times; want 3", calls)
		}
		if got, _ := os.ReadFile(path); string(got) != "new" {
			t.Errorf("contents = %q", got)
		}
		assertNoTemp(t, filepath.Dir(path))
	}
}

// Nothing else is retried: another error, the same numbers off Windows (they
// mean other things there), or a failure that outlasts the retries.
func TestWriteDoesNotRetryOtherRenameFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		goos      string
		err       error
		wantCalls int
	}{
		{"other error on windows", "windows", syscall.Errno(2), 1},
		{"same errno off windows", "linux", errorSharingViolation, 1},
		{"held for good", "windows", errorSharingViolation, renameAttempts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			calls := 0
			withRename(t, tc.goos, func(oldpath, newpath string) error {
				calls++
				return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: tc.err}
			})
			err := Write(filepath.Join(dir, "entry.json"), []byte("x"), 0o600)
			if err == nil || !errors.Is(err, tc.err) {
				t.Fatalf("Write = %v; want the rename error", err)
			}
			if calls != tc.wantCalls {
				t.Errorf("rename called %d times; want %d", calls, tc.wantCalls)
			}
			assertNoTemp(t, dir)
		})
	}
}

// The real thing, on Windows: a reader holds the target open while Write
// replaces it, and lets go shortly after.
func TestWriteOverAFileHeldOpenOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows sharing semantics")
	}
	path := filepath.Join(t.TempDir(), "entry.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = f.Close()
		close(released)
	}()
	if err := Write(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("Write over a briefly held file: %v", err)
	}
	<-released
	if got, _ := os.ReadFile(path); string(got) != "new" {
		t.Errorf("contents = %q; want new", got)
	}
}

func assertNoTemp(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "entry.json" {
			t.Errorf("left behind: %s", e.Name())
		}
	}
}
