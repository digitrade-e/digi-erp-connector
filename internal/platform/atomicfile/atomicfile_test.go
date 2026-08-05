package atomicfile

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWriteCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.txt")

	if err := Write(path, []byte("hello"), 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("contents = %q, want %q", got, "hello")
	}
}

func TestWriteReplacesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.txt")
	if err := os.WriteFile(path, []byte("old contents, longer than the new"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := Write(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, _ := os.ReadFile(path)
	if string(got) != "new" {
		t.Errorf("contents = %q, want %q — the old bytes must not survive", got, "new")
	}
}

// A failed write must not litter the directory with temp files, otherwise the
// data dir slowly fills with .tmp-* debris.
func TestWriteLeavesNoTempFilesBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queries.json")

	for i := 0; i < 3; i++ {
		if err := Write(path, []byte("{}"), 0o600); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want exactly the target file", len(entries))
	}
}

func TestWriteCreatesMissingDirectories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "config.yaml")

	if err := Write(path, []byte("erp: hasavshevet"), 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

// The permissions matter for config.yaml (bearer token) and secrets/*.bin.
// Windows only models the read-only bit, so this is a Unix assertion.
func TestWritePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not modelled on Windows")
	}
	path := filepath.Join(t.TempDir(), "secret.bin")

	if err := Write(path, []byte("s3cret"), 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions = %o, want 600", perm)
	}
}

// The whole point of the package: a failure must be reported, never swallowed.
// (secrets.Set used to return a nil error when the rename failed.)
func TestWriteReportsFailure(t *testing.T) {
	dir := t.TempDir()
	// A directory where the file should be makes the rename fail.
	path := filepath.Join(dir, "target")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	err := Write(path, []byte("data"), 0o600)
	if err == nil {
		t.Fatal("Write returned nil when the target could not be replaced")
	}
	if !strings.Contains(err.Error(), "atomicfile:") {
		t.Errorf("error %q should identify its source", err)
	}
}

func TestWriteEmptyData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.json")

	if err := Write(path, nil, 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("size = %d, want 0", info.Size())
	}
}
