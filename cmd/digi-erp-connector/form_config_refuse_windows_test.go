//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/paths"
)

// R3 reproducer: a data directory the walk REFUSES (planted junction / untrusted
// owner) must not receive the secret or config.yaml.
func TestR3_PersistConfigRefusesWhenTheDirectoryIsRefused(t *testing.T) {
	t.Setenv("PROGRAMDATA", t.TempDir())
	old := guiProtectTreeFn
	guiProtectTreeFn = func(string, dataacl.TreeOptions) (dataacl.Report, error) {
		return dataacl.Report{}, dataacl.ErrReparsePoint
	}
	t.Cleanup(func() { guiProtectTreeFn = old })
	cfg := config.Default()
	cfg.ERP = config.ERPSAP
	_, err := persistConfig(cfg, "a password", &recLogger{})
	cfgPath, _ := paths.ConfigFilePath()
	_, cfgErr := os.Stat(cfgPath)
	_, secErr := os.Stat(filepath.Join(paths.DataDir(), "secrets"))
	if err == nil || cfgErr == nil || secErr == nil {
		t.Errorf("persistConfig = %v; config.yaml stat %v, secrets stat %v — wrote into a refused data directory", err, cfgErr, secErr)
	}
}
