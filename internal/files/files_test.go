package files

import (
	"path/filepath"
	"testing"
)

func TestResolveFilePath(t *testing.T) {
	base := t.TempDir()
	allowed, err := BuildAllowedFolders([]string{base})
	if err != nil {
		t.Fatalf("build allowed folders: %v", err)
	}

	_, err = ResolveFilePath(allowed, base, "../outside.txt")
	if err == nil {
		t.Fatalf("expected traversal error")
	}

	_, err = ResolveFilePath(allowed, base, "")
	if err == nil {
		t.Fatalf("expected empty filename error")
	}

	_, err = ResolveFilePath(allowed, filepath.Join(base, "sub"), "file.txt")
	if err == nil {
		t.Fatalf("expected folder not allowed")
	}

	full, err := ResolveFilePath(allowed, base, "image.jpg")
	if err != nil {
		t.Fatalf("expected valid path, got %v", err)
	}
	if filepath.Dir(full) != filepath.Clean(base) {
		t.Fatalf("expected file under base, got %s", full)
	}
}
