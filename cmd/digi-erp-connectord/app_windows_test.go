//go:build windows

package main

import (
	"errors"
	"os"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/logger"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/paths"
	"golang.org/x/sys/windows"
)

// Start wired end to end, with %PROGRAMDATA% in a temp dir and no config (a
// first install's service start). The bootstrap logger is replaced so no
// server.log handle outlives the test.
func startWithoutConfig(t *testing.T) error {
	t.Helper()
	t.Setenv("PROGRAMDATA", t.TempDir())
	old := newBootstrapLog
	newBootstrapLog = func() logger.LoggerService { return &quietLogger{} }
	t.Cleanup(func() { newBootstrapLog = old })
	return (&serverApp{}).Start()
}

// B2: the data directory is protected before config.Load — so also when there
// is no config yet.
func TestStart_ProtectsTheDataDirBeforeTheConfigExists(t *testing.T) {
	trees, _ := stubProtect(t, dataacl.Report{}, nil, nil)
	err := startWithoutConfig(t)
	if !errors.Is(err, config.ErrNotFound) {
		t.Fatalf("Start = %v; want ErrNotFound", err)
	}
	if len(*trees) != 1 || (*trees)[0] != paths.DataDir() {
		t.Errorf("ProtectTree called with %v; want once with %s", *trees, paths.DataDir())
	}
}

// The error is Start's: a daemon that could not protect the data directory
// does not start.
func TestStart_RefusesWhenTheDataDirCannotBeProtected(t *testing.T) {
	boom := errors.New("boom")
	stubProtect(t, dataacl.Report{}, boom, nil)
	if err := startWithoutConfig(t); !errors.Is(err, boom) {
		t.Fatalf("Start = %v; want the protect error", err)
	}
}

// The real thing: a data directory that exists with no config ends up
// protected.
func TestStart_RealDataDirIsProtected(t *testing.T) {
	t.Setenv("PROGRAMDATA", t.TempDir())
	if err := os.MkdirAll(paths.DataDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	old := newBootstrapLog
	newBootstrapLog = func() logger.LoggerService { return &quietLogger{} }
	t.Cleanup(func() { newBootstrapLog = old })
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
