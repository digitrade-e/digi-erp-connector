package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
)

type recLogger struct {
	mu     sync.Mutex
	errors []string
}

func (*recLogger) Info(string) {}
func (l *recLogger) Error(msg string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errors = append(l.errors, fmt.Sprintf("%s: %v", msg, err))
}
func (*recLogger) Warn(string)    {}
func (*recLogger) Success(string) {}
func (*recLogger) Close() error   { return nil }

// Before a save that creates config.yaml, the data directory is protected
// (R2-2: so secrets\*.bin and the atomic-write temps are created inside the
// protected tree); after every save config.yaml gets the inherited DACL. A
// failure is logged and becomes a "Saved, but…" warning — the daemon re-applies
// the DACL at its next start, failing closed on the directory.
func TestProtectBeforeAndAfterSave(t *testing.T) {
	dir := t.TempDir()
	const cfg = `C:\ProgramData\digi-erp-connector\config.yaml`

	t.Run("ok", func(t *testing.T) {
		dirs, files := stubGUIProtect(t, nil, nil)
		log := &recLogger{}
		if w := protectDirBeforeSave(dir, log); w != "" {
			t.Errorf("before-save warning %q", w)
		}
		if w := protectFileAfterSave(cfg, log); w != "" {
			t.Errorf("after-save warning %q", w)
		}
		if len(*dirs) != 1 || (*dirs)[0] != dir || len(*files) != 1 || (*files)[0] != cfg {
			t.Errorf("ProtectTree %v, ProtectFile %v", *dirs, *files)
		}
		if len(log.errors) != 0 {
			t.Errorf("logged %v", log.errors)
		}
	})
	t.Run("the directory fails", func(t *testing.T) {
		stubGUIProtect(t, errors.New("access denied"), nil)
		log := &recLogger{}
		w := protectDirBeforeSave(dir, log)
		if !strings.Contains(w, "data directory") || !strings.Contains(w, "access denied") {
			t.Errorf("warning %q; want one naming the data directory and the error", w)
		}
		if len(log.errors) != 1 {
			t.Errorf("logged %v", log.errors)
		}
	})
	t.Run("config.yaml fails", func(t *testing.T) {
		stubGUIProtect(t, nil, errors.New("not the owner"))
		log := &recLogger{}
		w := protectFileAfterSave(cfg, log)
		if !strings.HasPrefix(w, "Saved, but") || !strings.Contains(w, "config.yaml") {
			t.Errorf("warning %q; want a Saved-but warning naming config.yaml", w)
		}
		if len(log.errors) != 1 || !strings.Contains(log.errors[0], "not the owner") {
			t.Errorf("logged %v", log.errors)
		}
	})
}

// R2-3 for the GUI's own log files: one that is not exactly ours (wrong ACL,
// untrusted owner, a link) is moved aside before it is opened; if it cannot
// be moved, the GUI does not log into it.
func TestPrepareLog(t *testing.T) {
	stub := func(t *testing.T, trusted bool, trustErr, moveErr error) *[]string {
		var moved []string
		oldT, oldM := guiFileTrustedFn, guiMoveAsideFn
		guiFileTrustedFn = func(string) (bool, error) { return trusted, trustErr }
		guiMoveAsideFn = func(p string) (string, error) { moved = append(moved, p); return p + ".old", moveErr }
		t.Cleanup(func() { guiFileTrustedFn, guiMoveAsideFn = oldT, oldM })
		return &moved
	}
	const p = `C:\ProgramData\digi-erp-connector\ui.log`
	if moved := stub(t, true, nil, nil); !prepareLog(p) || len(*moved) != 0 {
		t.Errorf("a trusted log: usable=%v moved=%v; want usable, not moved", prepareLog(p), *moved)
	}
	if moved := stub(t, false, nil, nil); !prepareLog(p) || len(*moved) != 1 {
		t.Errorf("an untrusted log: moved=%v; want moved aside, then usable", *moved)
	}
	if stub(t, false, nil, errors.New("in use")); prepareLog(p) {
		t.Error("an untrusted log that could not be moved is still used")
	}
	// Cannot even be checked (not elevated): the open decides, and fails.
	if moved := stub(t, false, errors.New("access denied"), nil); !prepareLog(p) || len(*moved) != 0 {
		t.Errorf("an uncheckable log: moved=%v; want left to the open", *moved)
	}
}

// stubGUIProtect replaces the GUI's dataacl calls and records their paths.
func stubGUIProtect(t *testing.T, dirErr, fileErr error) (*[]string, *[]string) {
	t.Helper()
	var dirs, files []string
	oldDir, oldFile := guiProtectTreeFn, guiProtectFileFn
	guiProtectTreeFn = func(p string, _ dataacl.TreeOptions) (dataacl.Report, error) {
		dirs = append(dirs, p)
		return dataacl.Report{}, dirErr
	}
	guiProtectFileFn = func(p string) error { files = append(files, p); return fileErr }
	t.Cleanup(func() { guiProtectTreeFn, guiProtectFileFn = oldDir, oldFile })
	return &dirs, &files
}

// "The data directory was missing at launch" holds for one save only: a later
// save in the same session must not re-apply the directory's DACL (which
// would add the operator's own account to it).
func TestDataDirMissingAtLaunchIsOneShot(t *testing.T) {
	dataDirMissingAtLaunch.Store(true)
	t.Cleanup(func() { dataDirMissingAtLaunch.Store(false) })
	if !takeDataDirMissingAtLaunch() {
		t.Fatal("first take = false; want true")
	}
	if takeDataDirMissingAtLaunch() {
		t.Error("second take = true; want false")
	}
}

// Per save: protect the directory when it was missing at launch, or when
// config.yaml does not exist yet (this save creates it).
func TestSaveProtectsTheDirectory(t *testing.T) {
	present := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(present, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(t.TempDir(), "config.yaml")
	for _, tc := range []struct {
		launch bool
		cfg    string
		want   bool
	}{
		{false, present, false},
		{false, absent, true},
		{true, present, true},
		{true, absent, true},
	} {
		dataDirMissingAtLaunch.Store(tc.launch)
		if got := saveProtectsTheDirectory(tc.cfg); got != tc.want {
			t.Errorf("launch=%v cfg exists=%v: %v; want %v", tc.launch, tc.cfg == present, got, tc.want)
		}
		// Taken by every save, whatever else decided: a later save never
		// re-applies the directory's DACL on the launch's account.
		if dataDirMissingAtLaunch.Load() {
			t.Errorf("launch=%v cfg exists=%v: the one-shot was not consumed", tc.launch, tc.cfg == present)
		}
	}
	dataDirMissingAtLaunch.Store(false)
}

func TestDataDirMissing(t *testing.T) {
	if dataDirMissing(t.TempDir()) {
		t.Error("an existing directory reported missing")
	}
	if !dataDirMissing(filepath.Join(t.TempDir(), "nope")) {
		t.Error("a missing directory reported present")
	}
}
