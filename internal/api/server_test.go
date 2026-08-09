package api

import (
	"net/http"
	"net/http/httptest"
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

	cfg := authConfig(t)

	srv, err := NewServer(cfg, ServerDeps{QueryStore: store})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if srv.Addr != cfg.APIListen {
		t.Fatalf("expected addr %q, got %q", cfg.APIListen, srv.Addr)
	}
}

func TestNewServerRequiresQueryStore(t *testing.T) {
	cfg := authConfig(t)

	if _, err := NewServer(cfg, ServerDeps{}); err == nil {
		t.Fatalf("expected error without query store")
	}
}

// newServerForTest builds a server and returns the error, for cases that assert
// NewServer refuses a configuration.
func newServerForTest(t *testing.T, cfg config.Config) (*http.Server, error) {
	t.Helper()
	store, err := queries.NewStore(filepath.Join(t.TempDir(), "queries.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return NewServer(cfg, ServerDeps{QueryStore: store})
}

// mustServer builds a server that is expected to be valid.
func mustServer(t *testing.T, cfg config.Config) *http.Server {
	t.Helper()
	srv, err := newServerForTest(t, cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv
}

// Most of the legacy compatibility surface stays gone. `/auth/token` and
// `/api/ping` came back as supported features (auth_exchange_test.go); these did
// not, and `/api/query` in particular must never return — the backend team asked
// for that explicitly.
func TestLegacyRoutesAreGone(t *testing.T) {
	h := mustServer(t, authConfig(t)).Handler
	token := mustIssuedToken(t)

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/test-connection"},
		{http.MethodGet, "/api/customers"},
		{http.MethodGet, "/api/orders/1"},
		{http.MethodPost, "/api/query"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404 — the legacy surface should not exist",
				tc.method, tc.path, rec.Code)
		}
	}
}
