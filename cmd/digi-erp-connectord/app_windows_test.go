//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/logger"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/paths"
	"golang.org/x/sys/windows"
)

// stubBootstrap replaces the bootstrap logger and records, at the moment it
// is opened, how many times ProtectTree had run.
func stubBootstrap(t *testing.T, trees *[]string) *[]int {
	t.Helper()
	var opened []int
	old := newBootstrapLog
	newBootstrapLog = func() logger.LoggerService {
		if trees != nil {
			opened = append(opened, len(*trees))
		}
		return &quietLogger{}
	}
	t.Cleanup(func() { newBootstrapLog = old })
	return &opened
}

// B2: the data directory is protected before config.Load — so also when there
// is no config yet. R2-3: and before server.log is opened.
func TestStart_ProtectsTheDataDirBeforeTheLogAndTheConfig(t *testing.T) {
	t.Setenv("PROGRAMDATA", t.TempDir())
	trees, _ := stubProtect(t, dataacl.Report{}, nil, nil)
	opened := stubBootstrap(t, trees)
	err := (&serverApp{}).Start()
	if !errors.Is(err, config.ErrNotFound) {
		t.Fatalf("Start = %v; want ErrNotFound", err)
	}
	if len(*trees) != 1 || (*trees)[0] != paths.DataDir() {
		t.Errorf("ProtectTree called with %v; want once with %s", *trees, paths.DataDir())
	}
	if len(*opened) != 1 || (*opened)[0] != 1 {
		t.Errorf("server.log opened after %v ProtectTree calls; want once, after it", *opened)
	}
}

// The error is Start's: a daemon that could not protect the data directory
// does not start.
func TestStart_RefusesWhenTheDataDirCannotBeProtected(t *testing.T) {
	t.Setenv("PROGRAMDATA", t.TempDir())
	boom := errors.New("boom")
	stubProtect(t, dataacl.Report{}, boom, nil)
	stubBootstrap(t, nil)
	if err := (&serverApp{}).Start(); !errors.Is(err, boom) {
		t.Fatalf("Start = %v; want the protect error", err)
	}
}

// The real thing, first install: no data directory, no config. Start creates
// the directory and protects it.
func TestStart_RealDataDirIsCreatedAndProtected(t *testing.T) {
	t.Setenv("PROGRAMDATA", t.TempDir())
	stubBootstrap(t, nil)
	if err := (&serverApp{}).Start(); !errors.Is(err, config.ErrNotFound) {
		t.Fatalf("Start = %v; want ErrNotFound", err)
	}
	sd, err := windows.GetNamedSecurityInfo(paths.DataDir(), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if ctl, _, _ := sd.Control(); ctl&windows.SE_DACL_PROTECTED == 0 {
		t.Errorf("the data directory is not protected: %s", sd.String())
	}
}

// R2-3: a server.log the walk had to reset (here: an explicit Everyone ACE, as
// a 1.0.17 install or a planter leaves it) is moved aside before the
// bootstrap logger opens a fresh one; a start that resets nothing moves
// nothing.
func TestStart_MovesAResetServerLogAside(t *testing.T) {
	t.Setenv("PROGRAMDATA", t.TempDir())
	logPath, err := paths.LoggerFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("old line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:(A;;FR;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, _ := sd.DACL()
	if err := windows.SetNamedSecurityInfo(logPath, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}

	start := func() {
		t.Helper()
		if err := (&serverApp{}).Start(); !errors.Is(err, config.ErrNotFound) {
			t.Fatalf("Start = %v; want ErrNotFound", err)
		}
	}
	start() // the real bootstrap logger, closed by Start on ErrNotFound
	aside, _ := filepath.Glob(logPath + ".*.old")
	if len(aside) != 1 {
		t.Fatalf("moved-aside logs %v; want one", aside)
	}
	if b, _ := os.ReadFile(aside[0]); string(b) != "old line\n" {
		t.Errorf("the moved-aside log holds %q", b)
	}
	cur, _ := os.ReadFile(logPath)
	if strings.Contains(string(cur), "old line") || !strings.Contains(string(cur), "config not found") {
		t.Errorf("server.log after Start = %q; want a fresh file with this start's lines", cur)
	}

	start()
	if again, _ := filepath.Glob(logPath + ".*.old"); len(again) != 1 {
		t.Errorf("a start that reset nothing moved the log aside again: %v", again)
	}
}

// R3-4: a server.log that is not provably ours and cannot be moved aside
// (a program holds it) does not refuse the start: the daemon logs to a new,
// uniquely named file, writes no marker, and so tries again next start.
func TestFreshServerLog_FallsBackWhenItCannotMoveTheLog(t *testing.T) {
	t.Setenv("PROGRAMDATA", t.TempDir())
	logPath, _ := paths.LoggerFilePath()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("held\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldM, oldS := moveAsideFn, setLogPathFn
	moves := 0
	moveAsideFn = func(string) (string, error) { moves++; return "", errors.New("in use") }
	var alt string
	setLogPathFn = func(p string) { alt = p }
	t.Cleanup(func() { moveAsideFn, setLogPathFn = oldM, oldS })

	if mark := freshServerLog(paths.DataDir(), dataacl.Report{}, &quietLogger{}); mark {
		t.Error("asked to mark a log it did not open fresh")
	}
	if moves < 2 {
		t.Errorf("moved %d times; want retries", moves)
	}
	if ok, _ := filepath.Match(filepath.Join(paths.DataDir(), "server-*.log"), alt); !ok {
		t.Errorf("fallback log %q; want server-<time>.log in the data directory", alt)
	}
}

// A server.log that is exactly ours and is the object the marker names is kept.
func TestFreshServerLog_KeepsItsOwnLog(t *testing.T) {
	t.Setenv("PROGRAMDATA", t.TempDir())
	logPath, _ := paths.LoggerFilePath()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("ours\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := dataacl.ProtectTree(paths.DataDir(), dataacl.TreeOptions{}); err != nil {
		t.Fatal(err)
	}
	markServerLog(paths.DataDir(), &quietLogger{})
	oldM := moveAsideFn
	moves := 0
	moveAsideFn = func(string) (string, error) { moves++; return "", nil }
	t.Cleanup(func() { moveAsideFn = oldM })
	freshServerLog(paths.DataDir(), dataacl.Report{}, &quietLogger{})
	if moves != 0 {
		t.Errorf("our own, marked server.log was moved aside")
	}

	// T4-2: the same trusted, unreset file under a marker naming another
	// object is not provably ours: it is moved aside.
	if err := os.WriteFile(filepath.Join(paths.DataDir(), logIDName), []byte("00000000-0000000000000000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	freshServerLog(paths.DataDir(), dataacl.Report{}, &quietLogger{})
	if moves != 1 {
		t.Errorf("a mis-marked server.log was moved %d times; want once", moves)
	}
}
