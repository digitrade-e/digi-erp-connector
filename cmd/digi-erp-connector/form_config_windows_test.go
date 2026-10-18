//go:build windows

package main

import (
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/paths"
)

// persistConfig wired end to end with %PROGRAMDATA% in a temp dir: every save
// protects config.yaml at ConfigFilePath(); the data directory only on the
// save that creates config.yaml, or the first save after a launch that found
// no data directory.
func TestPersistConfig_ProtectsWhatItSaves(t *testing.T) {
	t.Setenv("PROGRAMDATA", t.TempDir())
	dataDirMissingAtLaunch.Store(false)
	t.Cleanup(func() { dataDirMissingAtLaunch.Store(false) })
	cfgPath, err := paths.ConfigFilePath()
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.ERP = config.ERPSAP // no stored-procedure setup

	save := func(wantDir bool) {
		t.Helper()
		dirs, files := stubGUIProtect(t, nil, nil)
		if _, err := persistConfig(cfg, "", &recLogger{}); err != nil {
			t.Fatalf("persistConfig: %v", err)
		}
		if len(*files) != 1 || (*files)[0] != cfgPath {
			t.Errorf("ProtectFile called with %v; want %s", *files, cfgPath)
		}
		if wantDir && (len(*dirs) != 1 || (*dirs)[0] != paths.DataDir()) {
			t.Errorf("ProtectTree called with %v; want %s", *dirs, paths.DataDir())
		}
		if !wantDir && len(*dirs) != 0 {
			t.Errorf("ProtectTree called with %v; want not at all", *dirs)
		}
	}
	save(true)  // first save: config.yaml did not exist
	save(false) // a later save in the session
	dataDirMissingAtLaunch.Store(true)
	save(true)  // the first save after a launch that found no data directory
	save(false) // …and only that one
}
