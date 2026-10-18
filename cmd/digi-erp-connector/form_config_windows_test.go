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
// save walks the data directory first — before the secret or config.yaml is
// written (R2-2, R3-2) — and again after, for what it wrote (R3-5).
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
		var calls []string
		_, cfgBefore := os.Stat(cfgPath)
		_, secBefore := os.Stat(secretsDir)
		old := guiProtectTreeFn
		guiProtectTreeFn = func(p string, opt dataacl.TreeOptions) (dataacl.Report, error) {
			_, cfgErr := os.Stat(cfgPath)
			_, secErr := os.Stat(secretsDir)
			if len(calls) == 0 && ((cfgBefore != nil && cfgErr == nil) || (secBefore != nil && secErr == nil)) {
				t.Errorf("save %d: the first walk ran after this save wrote (config.yaml: %v, secrets: %v)", i, cfgErr, secErr)
			}
			if len(calls) == 1 && (cfgErr != nil || secErr != nil) {
				t.Errorf("save %d: the second walk ran before the save wrote (config.yaml: %v, secrets: %v)", i, cfgErr, secErr)
			}
			calls = append(calls, p)
			return dataacl.Report{}, nil
		}
		if _, err := persistConfig(cfg, "a password", &recLogger{}); err != nil {
			t.Fatalf("persistConfig: %v", err)
		}
		guiProtectTreeFn = old
		if len(calls) != 2 || calls[0] != paths.DataDir() || calls[1] != paths.DataDir() {
			t.Errorf("save %d: walks %v; want the data directory before and after", i, calls)
		}
	}
}

// R3-5: what a real save writes ends owned by Administrators, an owner the
// daemon always trusts.
func TestPersistConfig_HandsWhatItWritesToAdministrators(t *testing.T) {
	t.Setenv("PROGRAMDATA", t.TempDir())
	cfg := config.Default()
	cfg.ERP = config.ERPSAP
	if _, err := persistConfig(cfg, "a password", &recLogger{}); err != nil {
		t.Fatalf("persistConfig: %v", err)
	}
	cfgPath, _ := paths.ConfigFilePath()
	for _, p := range []string{paths.DataDir(), cfgPath, filepath.Join(paths.DataDir(), "secrets", "db_password_sap.bin")} {
		sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if o, _, _ := sd.Owner(); o.String() != "S-1-5-32-544" {
			t.Errorf("%s is owned by %s; want BUILTIN\\Administrators", p, o.String())
		}
	}
}

// R3-3: at launch, a ui.log left with the default ACL (here an explicit
// Everyone ACE) is moved aside before the GUI opens it.
func TestPrepareGUIDataDir_MovesAForeignLogAside(t *testing.T) {
	t.Setenv("PROGRAMDATA", t.TempDir())
	uiPath, _ := paths.UILogFilePath()
	if err := os.MkdirAll(filepath.Dir(uiPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(uiPath, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sd, _ := windows.SecurityDescriptorFromString("D:(A;;FR;;;WD)")
	dacl, _, _ := sd.DACL()
	if err := windows.SetNamedSecurityInfo(uiPath, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	ok, err := prepareGUIDataDir(paths.DataDir(), []string{uiPath}, &recLogger{})
	if err != nil || !ok[uiPath] {
		t.Fatalf("prepareGUIDataDir = %v, %v", ok, err)
	}
	if _, err := os.Stat(uiPath); !os.IsNotExist(err) {
		t.Errorf("ui.log is still in place (%v); want it moved aside", err)
	}
	if aside, _ := filepath.Glob(uiPath + ".*.old"); len(aside) != 1 {
		t.Errorf("moved-aside logs %v; want one", aside)
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
