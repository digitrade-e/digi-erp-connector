package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/queries"
)

func newQueryMux(t *testing.T) (*http.ServeMux, *queries.Store) {
	t.Helper()
	store, err := queries.NewStore(filepath.Join(t.TempDir(), "queries.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("POST /api/custom_sql", NewCreateCustomSQLHandler(store))
	mux.Handle("POST /api/create_custom_sql", NewCreateCustomSQLHandler(store))
	mux.Handle("GET /api/custom_sql", NewListCustomSQLHandler(store))
	mux.Handle("GET /api/custom_sql/{name}", NewGetCustomSQLHandler(store))
	mux.Handle("PATCH /api/custom_sql/{name}", NewUpdateCustomSQLHandler(store))
	mux.Handle("DELETE /api/custom_sql/{name}", NewDeleteCustomSQLHandler(store))
	mux.Handle("GET /api/sqlqueries/{name}", NewRunSavedQueryHandler(store, nil))
	return mux, store
}

func do(t *testing.T, mux *http.ServeMux, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestCustomSQLCRUDFlow(t *testing.T) {
	mux, _ := newQueryMux(t)

	// Create (legacy alias route, electron-style body)
	w := do(t, mux, http.MethodPost, "/api/create_custom_sql",
		`{"name":"q1","description":"d","sql":"SELECT 1 AS x","params":{"a":1}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Duplicate name → 409
	w = do(t, mux, http.MethodPost, "/api/custom_sql",
		`{"name":"q1","sql":"SELECT 2"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate: expected 409, got %d", w.Code)
	}

	// Missing sql → 400
	w = do(t, mux, http.MethodPost, "/api/custom_sql", `{"name":"q2"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing sql: expected 400, got %d", w.Code)
	}

	// List
	w = do(t, mux, http.MethodGet, "/api/custom_sql", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", w.Code)
	}
	var list []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("list decode: %v", err)
	}
	if len(list) != 1 || list[0]["name"] != "q1" {
		t.Fatalf("list mismatch: %v", list)
	}

	// Get
	w = do(t, mux, http.MethodGet, "/api/custom_sql/q1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", w.Code)
	}

	// Patch description only
	w = do(t, mux, http.MethodPatch, "/api/custom_sql/q1", `{"description":"new"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("patch: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var patched struct {
		Updated struct {
			Description string `json:"description"`
			SQL         string `json:"sql"`
		} `json:"updated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &patched); err != nil {
		t.Fatalf("patch decode: %v", err)
	}
	if patched.Updated.Description != "new" || patched.Updated.SQL != "SELECT 1 AS x" {
		t.Fatalf("patch result mismatch: %+v", patched.Updated)
	}

	// Delete, then 404 on repeat
	w = do(t, mux, http.MethodDelete, "/api/custom_sql/q1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d", w.Code)
	}
	w = do(t, mux, http.MethodDelete, "/api/custom_sql/q1", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("delete missing: expected 404, got %d", w.Code)
	}
}

func TestRunSavedQueryWithoutDB(t *testing.T) {
	mux, _ := newQueryMux(t)
	// Runner is nil (no DB in unit tests) → the handler must fail closed.
	w := do(t, mux, http.MethodGet, "/api/sqlqueries/anything", "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 without DB, got %d", w.Code)
	}
}
