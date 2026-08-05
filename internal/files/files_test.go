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

	// Compare against the *canonical* base. ResolveFilePath deliberately returns
	// the symlink-resolved path (that re-resolution is the traversal defence),
	// and on Windows t.TempDir() can hand back a short 8.3 path — GitHub's
	// runners return C:\Users\RUNNER~1\... which resolves to
	// C:\Users\runneradmin\... . Comparing the resolved output to the raw base
	// only happens to pass where %TEMP% is already a long path.
	canonicalBase, err := canonicalizePath(base)
	if err != nil {
		t.Fatalf("canonicalize base: %v", err)
	}
	if !pathEqual(filepath.Dir(full), canonicalBase) {
		t.Fatalf("expected file directly under %s, got %s", canonicalBase, full)
	}
}

// A folder given in a form that canonicalises to something else — a short 8.3
// path, a differently-cased drive letter — must still be recognised as allowed,
// because the allow-list check compares canonical forms on both sides.
func TestResolveFilePathAcceptsNonCanonicalFolderInput(t *testing.T) {
	base := t.TempDir()
	allowed, err := BuildAllowedFolders([]string{base})
	if err != nil {
		t.Fatalf("build allowed folders: %v", err)
	}

	canonicalBase, err := canonicalizePath(base)
	if err != nil {
		t.Fatalf("canonicalize base: %v", err)
	}

	// The canonical form of the same directory must resolve identically to the
	// form the caller happened to supply.
	fromRaw, err := ResolveFilePath(allowed, base, "image.jpg")
	if err != nil {
		t.Fatalf("raw base rejected: %v", err)
	}
	fromCanonical, err := ResolveFilePath(allowed, canonicalBase, "image.jpg")
	if err != nil {
		t.Fatalf("canonical base rejected: %v", err)
	}
	if !pathEqual(fromRaw, fromCanonical) {
		t.Errorf("same folder in two forms resolved differently: %s vs %s", fromRaw, fromCanonical)
	}
}
