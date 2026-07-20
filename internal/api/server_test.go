package api

import (
	"path/filepath"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/queries"
)

// TestNewServerRegistersRoutes constructs the full server; http.ServeMux
// panics at registration on conflicting patterns, so this locks the route
// table against accidental conflicts.
func TestNewServerRegistersRoutes(t *testing.T) {
	store, err := queries.NewStore(filepath.Join(t.TempDir(), "queries.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	cfg := config.Default()
	cfg.BearerToken = "test-token"

	srv, err := NewServer(cfg, ServerDeps{QueryStore: store})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if srv.Addr != cfg.APIListen {
		t.Fatalf("expected addr %q, got %q", cfg.APIListen, srv.Addr)
	}
}

func TestNewServerRequiresQueryStore(t *testing.T) {
	cfg := config.Default()
	cfg.BearerToken = "test-token"

	if _, err := NewServer(cfg, ServerDeps{}); err == nil {
		t.Fatalf("expected error without query store")
	}
}
