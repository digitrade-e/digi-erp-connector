package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/formatfloor"
)

type floorLog struct {
	quietLogger
	errs, infos []string
}

func (l *floorLog) Error(msg string, err error) { l.errs = append(l.errs, msg) }
func (l *floorLog) Info(msg string)             { l.infos = append(l.infos, msg) }

// Plan 088 D11: the daemon records its RollbackFloor in DataDir\format-floor at start, before it writes anything in its
// formats; one it must raise and cannot is a refused start, an unreadable one is left alone.
func TestTheDaemonRaisesTheFormatFloor(t *testing.T) {
	dir := t.TempDir()
	log := &floorLog{}
	if err := raiseFormatFloor(dir, log); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "format-floor")); strings.TrimSpace(string(b)) != formatfloor.RollbackFloor {
		t.Fatalf("format-floor %q", b)
	}
	// a higher floor (a later build ran here) stays
	_ = os.WriteFile(filepath.Join(dir, "format-floor"), []byte("9.0.0\n"), 0o600)
	if err := raiseFormatFloor(dir, log); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "format-floor")); string(b) != "9.0.0\n" {
		t.Fatalf("lowered to %q", b)
	}
	// unreadable: left as it is, the start goes on, the reason is logged
	_ = os.WriteFile(filepath.Join(dir, "format-floor"), []byte("garbage"), 0o600)
	log = &floorLog{}
	if err := raiseFormatFloor(dir, log); err != nil || len(log.errs) != 1 {
		t.Fatalf("%v %v", err, log.errs)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "format-floor")); string(b) != "garbage" {
		t.Fatal("an unreadable floor was overwritten")
	}
	// a floor it must raise and cannot write: no start
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("a read-only directory does not stop this user's write")
	}
	ro := t.TempDir()
	_ = os.Chmod(ro, 0o500)
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	log = &floorLog{}
	if err := raiseFormatFloor(ro, log); err == nil || len(log.errs) != 1 || !strings.Contains(log.errs[0], "not starting") {
		t.Fatalf("%v %v", err, log.errs)
	}
}
