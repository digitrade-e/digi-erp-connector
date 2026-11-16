package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/queries"
)

// Plan 087: the documentRule route and the read routes are wired, behind the
// one credential like every /api/* route; nothing writes a counter or a
// folder on the data path.
func TestDocumentsRoutes(t *testing.T) {
	store, err := queries.NewStore(filepath.Join(t.TempDir(), "queries.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := authConfig(t)
	cfg.ERP, cfg.SendOrderDir = config.ERPHasavshevet, t.TempDir()
	srv, err := NewServer(cfg, ServerDeps{QueryStore: store, Logger: testLogger{}, DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	do := func(method, path, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, req)
		return rec
	}
	tok := mustIssuedToken(t)
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/api/counters"}, {http.MethodGet, "/api/documents/capabilities"}, {http.MethodPost, "/api/v2/sendOrder"},
	} {
		if rec := do(r.method, r.path, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s unauthenticated = %d, want 401", r.method, r.path, rec.Code)
		}
	}
	if rec := do(http.MethodGet, "/api/counters", tok); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"key":"orders"`) {
		t.Errorf("GET /api/counters = %d %s", rec.Code, rec.Body)
	}
	if rec := do(http.MethodGet, "/api/documents/capabilities", tok); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"contract":1`) {
		t.Errorf("GET /api/documents/capabilities = %d %s", rec.Code, rec.Body)
	}
	if rec := do(http.MethodPost, "/api/v2/sendOrder", tok); rec.Code != http.StatusBadRequest {
		t.Errorf("POST /api/v2/sendOrder {} = %d %s, want 400 (validated like /api/sendOrder)", rec.Code, rec.Body)
	}
	// Writes are not routes: a counter changes only through the signed
	// provision command.
	for _, r := range []struct{ method, path string }{
		{http.MethodPut, "/api/counters/orders"}, {http.MethodPost, "/api/counters/orders/start"}, {http.MethodPost, "/api/counters"},
	} {
		if rec := do(r.method, r.path, tok); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 404/405", r.method, r.path, rec.Code)
		}
	}
}
