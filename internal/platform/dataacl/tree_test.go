package dataacl

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// Anything that redirects a path — a symlink, a junction or another
// name-surrogate reparse point (Go reports those as ModeIrregular on Windows)
// — stops ProtectTree: receipt data written through it would land outside the
// protected tree.
func TestRefusedMode(t *testing.T) {
	for _, tc := range []struct {
		m    fs.FileMode
		want bool
	}{
		{0o644, false},
		{fs.ModeDir | 0o755, false},
		{fs.ModeSymlink | 0o777, true},
		{fs.ModeIrregular, true},
		{fs.ModeDir | fs.ModeIrregular, true},
	} {
		if got := refusedMode(tc.m); got != tc.want {
			t.Errorf("refusedMode(%v) = %v; want %v", tc.m, got, tc.want)
		}
	}
}

func TestOwnAsSystem(t *testing.T) {
	if !ownAsSystem(SIDLocalSystem) || ownAsSystem(SIDAdministrators) || ownAsSystem("S-1-5-21-1-2-3-1001") {
		t.Error("ownAsSystem: only the SYSTEM token user makes SYSTEM the owner")
	}
}

func TestScanTree_ListsEveryEntryButTheRoot(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"receipts/index", "history/receipts/1000", "secrets"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "receipts", "index", "k.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := scanTree(root)
	if err != nil {
		t.Fatalf("scanTree: %v", err)
	}
	// receipts, receipts/index, history, history/receipts, history/receipts/1000,
	// secrets and the one file.
	if len(got) != 7 {
		t.Errorf("scanTree = %v; want 7 entries", got)
	}
	for _, p := range got {
		if p == root {
			t.Error("scanTree listed the root")
		}
	}
}

func TestScanTree_RefusesASymlinkInsideOrAsTheRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "receipts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "receipts", "index")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	if _, err := scanTree(root); !errors.Is(err, ErrReparsePoint) {
		t.Errorf("scanTree with a symlink inside = %v; want ErrReparsePoint", err)
	}

	link := filepath.Join(t.TempDir(), "data")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := scanTree(link); !errors.Is(err, ErrReparsePoint) {
		t.Errorf("scanTree on a symlinked root = %v; want ErrReparsePoint", err)
	}
	if _, err := scanTree(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("scanTree on a missing root = nil; want an error")
	}
}
