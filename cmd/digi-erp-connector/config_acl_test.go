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

// The elevated GUI gives what it saves the daemon's protected DACL: always
// config.yaml, and the data directory too when this save created it. A
// failure is logged and surfaced as a warning — config.yaml is saved by then,
// and the daemon re-applies the DACL at its next start, failing closed on the
// directory.
func TestProtectSavedConfig(t *testing.T) {
	const dir, cfg = `C:\ProgramData\digi-erp-connector`, `C:\ProgramData\digi-erp-connector\config.yaml`
	stub := func(t *testing.T, dirErr, fileErr error) (*[]string, *[]string) {
		return stubGUIProtect(t, dirErr, fileErr)
	}

	t.Run("existing directory: config.yaml only", func(t *testing.T) {
		dirs, files := stub(t, nil, nil)
		log := &recLogger{}
		if w := protectSavedConfig(dir, cfg, false, log); w != "" {
			t.Errorf("warning %q; want none", w)
		}
		if len(*dirs) != 0 || len(*files) != 1 || (*files)[0] != cfg {
			t.Errorf("ProtectDir %v, ProtectFile %v; want only config.yaml", *dirs, *files)
		}
		if len(log.errors) != 0 {
			t.Errorf("logged %v", log.errors)
		}
	})

	t.Run("directory created by this save: both", func(t *testing.T) {
		dirs, files := stub(t, nil, nil)
		if w := protectSavedConfig(dir, cfg, true, &recLogger{}); w != "" {
			t.Errorf("warning %q; want none", w)
		}
		if len(*dirs) != 1 || (*dirs)[0] != dir || len(*files) != 1 {
			t.Errorf("ProtectDir %v, ProtectFile %v; want both", *dirs, *files)
		}
	})

	t.Run("directory fails: logged, warned, config.yaml still protected", func(t *testing.T) {
		_, files := stub(t, errors.New("access denied"), nil)
		log := &recLogger{}
		w := protectSavedConfig(dir, cfg, true, log)
		if !strings.Contains(w, "Saved") || !strings.Contains(w, "data directory") {
			t.Errorf("warning %q; want a saved-but warning naming the data directory", w)
		}
		if len(log.errors) != 1 || !strings.Contains(log.errors[0], "access denied") {
			t.Errorf("logged %v; want the directory's error", log.errors)
		}
		if len(*files) != 1 {
			t.Errorf("config.yaml was not protected after the directory failed: %v", *files)
		}
	})

	t.Run("config.yaml fails: logged and warned", func(t *testing.T) {
		stub(t, nil, errors.New("not the owner"))
		log := &recLogger{}
		w := protectSavedConfig(dir, cfg, false, log)
		if !strings.Contains(w, "config.yaml") {
			t.Errorf("warning %q; want one naming config.yaml", w)
		}
		if len(log.errors) != 1 || !strings.Contains(log.errors[0], "not the owner") {
			t.Errorf("logged %v", log.errors)
		}
	})
}

// stubGUIProtect replaces the GUI's dataacl calls and records their paths.
func stubGUIProtect(t *testing.T, dirErr, fileErr error) (*[]string, *[]string) {
	t.Helper()
	var dirs, files []string
	oldDir, oldFile := guiProtectTreeFn, guiProtectFileFn
	guiProtectTreeFn = func(p string) (dataacl.Report, error) { dirs = append(dirs, p); return dataacl.Report{}, dirErr }
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
