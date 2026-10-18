//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/paths"
	"golang.org/x/sys/windows"
)

// persistConfig wired end to end with %PROGRAMDATA% in a temp dir: every save
// protects config.yaml at ConfigFilePath() after it is written; the data
// directory only on the save that creates config.yaml, or the first save after
// a launch that found no data directory — and then BEFORE anything is written
// (R2-2): no config.yaml and no secrets\ yet when ProtectTree runs.
func TestPersistConfig_ProtectsWhatItSaves(t *testing.T) {
	t.Setenv("PROGRAMDATA", t.TempDir())
	dataDirMissingAtLaunch.Store(false)
	t.Cleanup(func() { dataDirMissingAtLaunch.Store(false) })
	cfgPath, err := paths.ConfigFilePath()
	if err != nil {
		t.Fatal(err)
	}
	secretsDir := filepath.Join(paths.DataDir(), "secrets")
	cfg := config.Default()
	cfg.ERP = config.ERPSAP // no stored-procedure setup

	save := func(wantDir bool) {
		t.Helper()
		var order []string
		// Only a save that creates them can show the order: what earlier saves
		// wrote is there already.
		_, cfgBefore := os.Stat(cfgPath)
		_, secBefore := os.Stat(secretsDir)
		oldT, oldF := guiProtectTreeFn, guiProtectFileFn
		guiProtectTreeFn = func(p string, _ dataacl.TreeOptions) (dataacl.Report, error) {
			_, cfgErr := os.Stat(cfgPath)
			_, secErr := os.Stat(secretsDir)
			order = append(order, "tree:"+p)
			if (cfgBefore != nil && cfgErr == nil) || (secBefore != nil && secErr == nil) {
				t.Errorf("ProtectTree ran after this save wrote (config.yaml: %v, secrets: %v)", cfgErr, secErr)
			}
			return dataacl.Report{}, nil
		}
		guiProtectFileFn = func(p string) error {
			if _, err := os.Stat(p); err != nil {
				t.Errorf("ProtectFile ran before config.yaml existed: %v", err)
			}
			order = append(order, "file:"+p)
			return nil
		}
		t.Cleanup(func() { guiProtectTreeFn, guiProtectFileFn = oldT, oldF })

		if _, err := persistConfig(cfg, "a password", &recLogger{}); err != nil {
			t.Fatalf("persistConfig: %v", err)
		}
		want := []string{"file:" + cfgPath}
		if wantDir {
			want = append([]string{"tree:" + paths.DataDir()}, want...)
		}
		if strings.Join(order, "|") != strings.Join(want, "|") {
			t.Errorf("calls %v; want %v", order, want)
		}
	}
	save(true)  // first save: config.yaml did not exist
	save(false) // a later save in the session
	dataDirMissingAtLaunch.Store(true)
	save(true)  // the first save after a launch that found no data directory
	save(false) // …and only that one
}

// The elevated GUI adds no ACE of its own: Administrators covers it, so its
// DACL is the daemon's.
func TestPersistConfig_AddsNoACEForTheOperator(t *testing.T) {
	t.Setenv("PROGRAMDATA", t.TempDir())
	cfg := config.Default()
	cfg.ERP = config.ERPSAP
	if _, err := persistConfig(cfg, "", &recLogger{}); err != nil {
		t.Fatalf("persistConfig: %v", err)
	}
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	me := tu.User.Sid.String()
	cfgPath, _ := paths.ConfigFilePath()
	for _, p := range []string{paths.DataDir(), cfgPath} {
		sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		if s := sd.String(); strings.Contains(s, me) {
			t.Errorf("%s: the operator's own SID %s is in %s", p, me, s)
		}
		if ctl, _, _ := sd.Control(); p == paths.DataDir() && ctl&windows.SE_DACL_PROTECTED == 0 {
			t.Errorf("%s is not protected: %s", p, sd.String())
		}
	}
}
