package atomicfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var tmp []string
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			tmp = append(tmp, e.Name())
		}
	}
	return tmp
}

func TestWriteNew_CreatesTheFileWhole(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "IMOVEIN_7.doc")
	if err := WriteNew(path, []byte("order 7\n"), 0o644); err != nil {
		t.Fatalf("WriteNew: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "order 7\n" {
		t.Fatalf("file = %q, %v", got, err)
	}
	if tmp := leftovers(t, dir); len(tmp) != 0 {
		t.Fatalf("temp files left behind: %v", tmp)
	}
}

// An existing file is never replaced: the call fails with fs.ErrExist, the
// file keeps its bytes, and no temp file is left.
func TestWriteNew_NeverReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "IMOVEIN_7.doc")
	if err := os.WriteFile(path, []byte("someone else's"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := WriteNew(path, []byte("ours"), 0o644)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("WriteNew over an existing file = %v; want fs.ErrExist", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "someone else's" {
		t.Fatalf("the existing file was replaced: %q", got)
	}
	if tmp := leftovers(t, dir); len(tmp) != 0 {
		t.Fatalf("temp files left behind: %v", tmp)
	}
}

// A missing folder is an error, and WriteNew does not create it.
func TestWriteNew_MissingFolderIsAnError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	if err := WriteNew(filepath.Join(dir, "x.doc"), []byte("x"), 0o644); err == nil {
		t.Fatal("WriteNew into a missing folder succeeded")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("WriteNew created the folder (err %v)", err)
	}
}
