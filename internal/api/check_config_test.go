package api

import (
	"path/filepath"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/db"
)

// CheckConfig refuses what the daemon refuses at its start — NewServer, or
// db.OpenLazy before it — so a writer that passes it never leaves a config
// the daemon will not start on.
func TestCheckConfigRefusesWhatNewServerRefuses(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*config.Config)
	}{
		{"no listen address", func(c *config.Config) { c.APIListen = "" }},
		{"a listen address with no host", func(c *config.Config) { c.APIListen = ":8082" }},
		{"a listen port out of range", func(c *config.Config) { c.APIListen = "0.0.0.0:70000" }},
		{"a blank credential", func(c *config.Config) { c.Auth.Password = " " }},
		{"half a TLS pair", func(c *config.Config) { c.TLS.CertFile = filepath.Join(t.TempDir(), "cert.pem") }},
		{"receipts in the order folder", func(c *config.Config) {
			c.ERP = config.ERPHasavshevet
			dir := t.TempDir()
			c.SendOrderDir, c.SendReceiptDir = dir, dir
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := authConfig(t)
			tc.mutate(&cfg)
			if err := CheckConfig(cfg); err == nil {
				t.Fatal("CheckConfig accepted it")
			}
			if _, err := newServerForTest(t, cfg); err == nil {
				t.Fatal("NewServer accepted it: the two checks drifted apart")
			}
		})
	}

	// The database is refused before NewServer, by the daemon's OpenLazy.
	cfg := authConfig(t)
	cfg.DB.Host, cfg.DB.Port, cfg.DB.User = "my host", 1433, "sa"
	if err := CheckConfig(cfg); err == nil {
		t.Error("CheckConfig accepted a db.host that is not a host")
	}
	if _, err := db.OpenLazy(cfg, "pw", db.DefaultOptions()); err == nil {
		t.Error("OpenLazy accepted it: the two checks drifted apart")
	}
}

// auth.secret may be empty: the daemon generates it at its first start.
func TestCheckConfigLeavesTheSecretToTheDaemon(t *testing.T) {
	cfg := authConfig(t)
	cfg.APIListen = "0.0.0.0:8082"
	cfg.Auth.Secret = ""
	if err := CheckConfig(cfg); err != nil {
		t.Fatalf("CheckConfig: %v", err)
	}
}
