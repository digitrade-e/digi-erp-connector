package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// redirectDataDir points paths.DataDir at a temp directory for the duration of
// the test, so nothing here can touch a real installation's config.yaml.
func redirectDataDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("the data directory is only relocatable via PROGRAMDATA on Windows")
	}
	dir := t.TempDir()
	t.Setenv("PROGRAMDATA", dir)
	return dir
}

func TestSaveLoadRoundTrip(t *testing.T) {
	redirectDataDir(t)

	want := Default()
	want.APIListen = "[::]:8082"
	want.BearerToken = "deadbeef"
	want.ERP = ERPHasavshevet
	want.DB.Host = "localhost"
	want.DB.Port = 1433
	want.DB.User = "sa"
	want.DB.Database = "BFL"
	want.DB.Encrypt = true
	want.DB.TrustServerCertificate = true
	want.ImageFolders = []string{`C:\images`, `D:\more images`}
	want.Queries.MaxRows = 100000

	if err := Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got.APIListen != want.APIListen {
		t.Errorf("apiListen = %q, want %q", got.APIListen, want.APIListen)
	}
	if got.BearerToken != want.BearerToken {
		t.Errorf("bearerToken = %q, want %q", got.BearerToken, want.BearerToken)
	}
	if got.DB != want.DB {
		t.Errorf("db = %+v, want %+v", got.DB, want.DB)
	}
	if got.Queries != want.Queries {
		t.Errorf("queries = %+v, want %+v", got.Queries, want.Queries)
	}
	if len(got.ImageFolders) != 2 || got.ImageFolders[0] != want.ImageFolders[0] {
		t.Errorf("imageFolders = %v, want %v", got.ImageFolders, want.ImageFolders)
	}
}

func TestLoadMissingConfig(t *testing.T) {
	redirectDataDir(t)

	_, err := Load()
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Load with no file = %v, want ErrNotFound", err)
	}
}

// LoadOrDefault is what the GUI uses on first run: a missing file must yield
// defaults, but a corrupt file must not be silently replaced by them.
func TestLoadOrDefault(t *testing.T) {
	dir := redirectDataDir(t)

	cfg, err := LoadOrDefault()
	if err != nil {
		t.Fatalf("LoadOrDefault with no file: %v", err)
	}
	if cfg.APIListen != Default().APIListen {
		t.Errorf("apiListen = %q, want the default", cfg.APIListen)
	}

	path := filepath.Join(dir, "digi-erp-connector", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("this: is: not: valid: yaml:\n\t- broken"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadOrDefault(); err == nil {
		t.Error("LoadOrDefault silently accepted a corrupt config; it must report the error")
	}
}

// Save must replace the file wholesale, not merge into leftover bytes.
func TestSaveOverwritesCompletely(t *testing.T) {
	dir := redirectDataDir(t)

	first := Default()
	first.BearerToken = "a-very-long-token-value-from-the-first-save"
	first.ImageFolders = []string{`C:\one`, `C:\two`, `C:\three`}
	if err := Save(first); err != nil {
		t.Fatalf("first Save: %v", err)
	}

	second := Default()
	second.BearerToken = "short"
	if err := Save(second); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.BearerToken != "short" {
		t.Errorf("bearerToken = %q, want %q", got.BearerToken, "short")
	}
	if len(got.ImageFolders) != 0 {
		t.Errorf("imageFolders = %v, want empty — the first save's values must not survive", got.ImageFolders)
	}

	// No temp files may be left in the data directory.
	entries, err := os.ReadDir(filepath.Join(dir, "digi-erp-connector"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("data dir holds %v, want only config.yaml", names)
	}
}

func TestDefaultIsUsable(t *testing.T) {
	cfg := Default()

	if cfg.Queries.TimeoutSeconds <= 0 || cfg.Queries.MaxRows <= 0 {
		t.Errorf("query limits must have positive defaults, got %+v", cfg.Queries)
	}
	if cfg.DB.Encrypt || cfg.DB.TrustServerCertificate {
		t.Error("TLS options must default off so existing installs are unaffected")
	}
}
