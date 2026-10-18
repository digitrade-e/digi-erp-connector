package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
)

// stubLaunch replaces the walk, the trust check and the move-aside, and
// records the order of calls.
func stubLaunch(t *testing.T, rep dataacl.Report, walkErr error, trusted map[string]bool, moveErr error) *[]string {
	t.Helper()
	var calls []string
	oldT, oldTr, oldM := guiProtectTreeFn, guiFileTrustedFn, guiMoveAsideFn
	guiProtectTreeFn = func(p string, opt dataacl.TreeOptions) (dataacl.Report, error) {
		calls = append(calls, "walk")
		if !opt.ClaimForAdministrators {
			t.Error("the GUI's walk does not hand what it created to Administrators")
		}
		return rep, walkErr
	}
	guiFileTrustedFn = func(p string) (bool, error) { return trusted[p], nil }
	guiMoveAsideFn = func(p string) (string, error) { calls = append(calls, "move:"+p); return p + ".old", moveErr }
	t.Cleanup(func() { guiProtectTreeFn, guiFileTrustedFn, guiMoveAsideFn = oldT, oldTr, oldM })
	return &calls
}

// R3-3: at launch the data directory is created and walked BEFORE any log is
// opened in it; a log the walk reset (or one not exactly ours) is moved aside
// so a fresh file object is opened; a directory the walk refuses gets no log
// at all.
func TestPrepareGUIDataDir(t *testing.T) {
	dir := t.TempDir()
	ui, srv := dir+`\ui.log`, dir+`\server.log`
	all := map[string]bool{ui: true, srv: true}

	t.Run("steady state", func(t *testing.T) {
		calls := stubLaunch(t, dataacl.Report{}, nil, all, nil)
		ok, err := prepareGUIDataDir(dir, []string{ui, srv}, &recLogger{})
		if err != nil || !ok[ui] || !ok[srv] || strings.Join(*calls, ",") != "walk" {
			t.Errorf("ok=%v err=%v calls=%v; want both usable, just the walk", ok, err, *calls)
		}
	})
	t.Run("a log the walk reset is moved aside", func(t *testing.T) {
		calls := stubLaunch(t, dataacl.Report{Reset: 1, ResetPaths: []string{ui}}, nil, all, nil)
		ok, err := prepareGUIDataDir(dir, []string{ui, srv}, &recLogger{})
		if err != nil || !ok[ui] || !ok[srv] || strings.Join(*calls, ",") != "walk,move:"+ui {
			t.Errorf("ok=%v err=%v calls=%v; want ui.log moved after the walk", ok, err, *calls)
		}
	})
	t.Run("a log not ours is moved aside", func(t *testing.T) {
		calls := stubLaunch(t, dataacl.Report{}, nil, map[string]bool{ui: true}, nil)
		ok, _ := prepareGUIDataDir(dir, []string{ui, srv}, &recLogger{})
		if !ok[srv] || strings.Join(*calls, ",") != "walk,move:"+srv {
			t.Errorf("ok=%v calls=%v; want server.log moved", ok, *calls)
		}
	})
	t.Run("a log that cannot be moved is not used", func(t *testing.T) {
		stubLaunch(t, dataacl.Report{Reset: 1, ResetPaths: []string{srv}}, nil, all, errors.New("in use"))
		ok, _ := prepareGUIDataDir(dir, []string{ui, srv}, &recLogger{})
		if !ok[ui] || ok[srv] {
			t.Errorf("ok=%v; want ui.log usable, server.log not", ok)
		}
	})
	t.Run("a refused directory gets no log", func(t *testing.T) {
		calls := stubLaunch(t, dataacl.Report{}, dataacl.ErrReparsePoint, all, nil)
		ok, err := prepareGUIDataDir(dir, []string{ui, srv}, &recLogger{})
		if !errors.Is(err, dataacl.ErrReparsePoint) || ok[ui] || ok[srv] || len(*calls) != 1 {
			t.Errorf("ok=%v err=%v calls=%v; want no log, the walk's error", ok, err, *calls)
		}
	})
}

// A save whose walk reset a log the GUI holds open reopens it fresh.
func TestReopenHeldLogs(t *testing.T) {
	const ui, srv = `C:\d\ui.log`, `C:\d\server.log`
	var reopened []string
	old := heldLogs
	heldLogs = map[string]func(){
		ui:  func() { reopened = append(reopened, ui) },
		srv: func() { reopened = append(reopened, srv) },
	}
	t.Cleanup(func() { heldLogs = old })
	reopenHeldLogs(dataacl.Report{ResetPaths: []string{`C:\d\config.yaml`, srv}})
	if strings.Join(reopened, ",") != srv {
		t.Errorf("reopened %v; want only server.log", reopened)
	}
}
