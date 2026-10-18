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

// persistConfig wired end to end with %PROGRAMDATA% in a temp dir: EVERY
// save protects the data directory first — before the secret or config.yaml
// is written (R2-2, R3-2) — and config.yaml after it is written.
func TestPersistConfig_ProtectsWhatItSaves(t *testing.T) {
	t.Setenv("PROGRAMDATA", t.TempDir())
	cfgPath, err := paths.ConfigFilePath()
	if err != nil {
		t.Fatal(err)
	}
	secretsDir := filepath.Join(paths.DataDir(), "secrets")
	cfg := config.Default()
	cfg.ERP = config.ERPSAP // no stored-procedure setup

	for i := 0; i < 3; i++ {
		var order []string
		_, cfgBefore := os.Stat(cfgPath)
		_, secBefore := os.Stat(secretsDir)
		oldT, oldF := guiProtectTreeFn, guiProtectFileFn
		guiProtectTreeFn = func(p string, _ dataacl.TreeOptions) (dataacl.Report, error) {
			_, cfgErr := os.Stat(cfgPath)
			_, secErr := os.Stat(secretsDir)
			order = append(order, "tree:"+p)
			if (cfgBefore != nil && cfgErr == nil) || (secBefore != nil && secErr == nil) {
				t.Errorf("save %d: ProtectTree ran after this save wrote (config.yaml: %v, secrets: %v)", i, cfgErr, secErr)
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
		if _, err := persistConfig(cfg, "a password", &recLogger{}); err != nil {
			t.Fatalf("persistConfig: %v", err)
		}
		guiProtectTreeFn, guiProtectFileFn = oldT, oldF
		if got, want := strings.Join(order, "|"), "tree:"+paths.DataDir()+"|file:"+cfgPath; got != want {
			t.Errorf("save %d: calls %s; want %s", i, got, want)
		}
	}
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
