package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
	"github.com/digitrade-e/digi-erp-connector/internal/queries"
)

// GET /api/sendOrder counts the orders a restart would lose. It sits behind
// the same credential as every /api/* route, and answers counts only.
func TestSendOrderQueueRoute(t *testing.T) {
	store, err := queries.NewStore(filepath.Join(t.TempDir(), "queries.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	queue := hasavshevet.NewOrderQueue(nil, testLogger{}) // not started: what is submitted stays queued
	srv, err := NewServer(authConfig(t), ServerDeps{QueryStore: store, Logger: testLogger{}, SendOrderQueue: queue})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	h := srv.Handler

	get := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/sendOrder", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := get(""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d, want 401", rec.Code)
	}

	if _, err := queue.Submit(hasavshevet.OrderRequest{DocumentType: "ORDER"}); err != nil {
		t.Fatal(err)
	}
	rec := get(mustIssuedToken(t))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body map[string]int
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body) != 2 || body["queued"] != 1 || body["running"] != 0 {
		t.Errorf("body = %v, want exactly {queued:1 running:0}", body)
	}
}
