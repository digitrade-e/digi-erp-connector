package hasavshevet

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The history copies and the index hold the customer's name, address, phone,
// email, last four digits and codes: on Unix they are the service's alone —
// files 0600, directories 0700 — like the index entries already were. (On
// Windows the data directory's ACLs do this job; the mode bits mean nothing.)
// The herp copies stay readable: Hashavshevet's importer reads them.
func TestReceiptWriter_PersonalDataIsTheServicesAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix mode bits")
	}
	h := newReceiptHarness(t, 1000)
	mustWrite(t, h.writer(), receiptFor(testKey(1)))

	for _, f := range []string{
		filepath.Join(h.dataDir, "history", "receipts", "1000", "KUPAIN_1000.doc"),
		filepath.Join(h.dataDir, "history", "receipts", "1000", "KUPAIN_1000.prm"),
		filepath.Join(h.dataDir, "receipts", "index", testKey(1)+".json"),
	} {
		assertMode(t, f, 0o600)
	}
	for _, d := range []string{
		filepath.Join(h.dataDir, "history"),
		filepath.Join(h.dataDir, "history", "receipts"),
		filepath.Join(h.dataDir, "history", "receipts", "1000"),
		filepath.Join(h.dataDir, "receipts"),
		filepath.Join(h.dataDir, "receipts", "index"),
	} {
		assertMode(t, d, os.ModeDir|0o700)
	}
	assertMode(t, h.docPath(1000), 0o644)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := fi.Mode() & (os.ModeDir | os.ModePerm); got != want {
		t.Errorf("%s is %v; want %v", path, got, want)
	}
}

// On Windows the mode bits mean nothing, and os.Chmod is SetFileAttributes,
// which needs FILE_WRITE_ATTRIBUTES: under a locked-down ACL it fails, and a
// failure there must not turn every receipt into a 500. So the Windows chmod
// is skipped; elsewhere its failure still counts.
func TestPrivateDirChmod_SkippedOnWindowsOnly(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-dir") // os.Chmod fails on it here
	if err := chmodPrivateDirFor("windows")(missing); err != nil {
		t.Errorf("windows: %v; want the chmod skipped", err)
	}
	for _, goos := range []string{"linux", "darwin"} {
		if err := chmodPrivateDirFor(goos)(missing); err == nil {
			t.Errorf("%s: a failing chmod was ignored", goos)
		}
	}
	dir := t.TempDir()
	if runtime.GOOS != "windows" {
		if err := chmodPrivateDirFor(runtime.GOOS)(dir); err != nil {
			t.Fatal(err)
		}
		assertMode(t, dir, os.ModeDir|0o700)
	}
}
