package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/legacyauth"
	"github.com/digitrade-e/digi-erp-connector/internal/queries"
)

const (
	testStaticToken = "static-bearer-token"
	testJWTSecret   = "DIGITRADE_DEVEPOPMENT_MSSQL"
	testJWTUser     = "digitrade"
	testJWTPassword = "123456"
)

func legacyConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.BearerToken = testStaticToken
	cfg.LegacyCompat = config.LegacyCompatConfig{
		Enabled:          true,
		JWTSecret:        testJWTSecret,
		JWTUser:          testJWTUser,
		JWTPassword:      testJWTPassword,
		JWTExpiryMinutes: 30,
		AllowRawSQL:      true,
	}
	return cfg
}

func newTestServer(t *testing.T, cfg config.Config) http.Handler {
	t.Helper()
	store, err := queries.NewStore(filepath.Join(t.TempDir(), "queries.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	srv, err := NewServer(cfg, ServerDeps{QueryStore: store})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv.Handler
}

// The legacy routes must not exist unless compatibility is switched on.
func TestLegacyRoutesAbsentByDefault(t *testing.T) {
	cfg := config.Default()
	cfg.BearerToken = testStaticToken
	h := newTestServer(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/auth/token", strings.NewReader(`{"username":"digitrade","password":"123456"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST /auth/token without compat = %d, want 404", rec.Code)
	}

	for _, path := range []string{"/api/ping", "/api/customers", "/api/orders/1"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+testStaticToken)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s without compat = %d, want 404", path, rec.Code)
		}
	}
}

// A half-filled compat block is a configuration error, not a route with empty
// credentials.
func TestLegacyCompatRequiresCredentials(t *testing.T) {
	store, err := queries.NewStore(filepath.Join(t.TempDir(), "queries.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*config.LegacyCompatConfig)
	}{
		{"no secret", func(l *config.LegacyCompatConfig) { l.JWTSecret = "" }},
		{"no user", func(l *config.LegacyCompatConfig) { l.JWTUser = "" }},
		{"no password", func(l *config.LegacyCompatConfig) { l.JWTPassword = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := legacyConfig(t)
			tc.mutate(&cfg.LegacyCompat)
			if _, err := NewServer(cfg, ServerDeps{QueryStore: store}); err == nil {
				t.Error("expected NewServer to refuse the incomplete compat config")
			}
		})
	}
}

func TestLegacyTokenExchange(t *testing.T) {
	h := newTestServer(t, legacyConfig(t))

	req := httptest.NewRequest(http.MethodPost, "/auth/token",
		strings.NewReader(`{"username":"digitrade","password":"123456"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	var body struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.TokenType != "Bearer" {
		t.Errorf("token_type = %q, want Bearer", body.TokenType)
	}
	if body.ExpiresIn != 1800 {
		t.Errorf("expires_in = %d, want 1800", body.ExpiresIn)
	}

	// The issued token must be usable as a bearer credential.
	signer, err := legacyauth.NewSigner(testJWTSecret)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	if _, err := signer.Verify(body.AccessToken); err != nil {
		t.Fatalf("issued token does not verify: %v", err)
	}
}

func TestLegacyTokenExchangeRejectsBadCredentials(t *testing.T) {
	h := newTestServer(t, legacyConfig(t))

	for _, body := range []string{
		`{"username":"digitrade","password":"wrong"}`,
		`{"username":"nobody","password":"123456"}`,
		`{}`,
		`not json`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/auth/token", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("body %s: status = %d, want 401", body, rec.Code)
			continue
		}
		var errBody struct{ Error string }
		_ = json.Unmarshal(rec.Body.Bytes(), &errBody)
		if errBody.Error != "invalid_credentials" {
			t.Errorf("body %s: error = %q, want invalid_credentials", body, errBody.Error)
		}
	}
}

// The point of the whole compat layer: a JWT from the old exchange authenticates
// the normal API routes, and so does the static token.
func TestAuthAcceptsBothCredentials(t *testing.T) {
	cfg := legacyConfig(t)
	h := newTestServer(t, cfg)

	signer, _ := legacyauth.NewSigner(testJWTSecret)
	jwt, _, err := signer.Sign(testJWTUser, 30*time.Minute)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	expired, _, _ := signer.Sign(testJWTUser, -time.Minute)
	foreign, _ := legacyauth.NewSigner("wrong-secret")
	foreignJWT, _, _ := foreign.Sign(testJWTUser, 30*time.Minute)

	tests := []struct {
		name       string
		credential string
		wantAuthed bool
	}{
		{"static token", testStaticToken, true},
		{"legacy jwt", jwt, true},
		{"expired jwt", expired, false},
		{"jwt signed with another secret", foreignJWT, false},
		{"garbage", "nonsense", false},
		{"empty", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
			if tc.credential != "" {
				req.Header.Set("Authorization", "Bearer "+tc.credential)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			authed := rec.Code != http.StatusUnauthorized
			if authed != tc.wantAuthed {
				t.Errorf("status = %d, authenticated = %v, want authenticated = %v",
					rec.Code, authed, tc.wantAuthed)
			}
		})
	}
}

// With compat off, a legacy JWT must NOT be accepted — the switch has to
// actually close the door.
func TestLegacyJWTRejectedWhenCompatDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.BearerToken = testStaticToken
	h := newTestServer(t, cfg)

	signer, _ := legacyauth.NewSigner(testJWTSecret)
	jwt, _, _ := signer.Sign(testJWTUser, 30*time.Minute)

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for a legacy JWT with compat disabled", rec.Code)
	}
}

func TestLegacyPingShape(t *testing.T) {
	h := newTestServer(t, legacyConfig(t))

	req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	req.Header.Set("Authorization", "Bearer "+testStaticToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		OK bool  `json:"ok"`
		TS int64 `json:"ts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.OK {
		t.Error("ok = false, want true")
	}
	// Milliseconds, as Date.now() produced.
	if body.TS < 1_000_000_000_000 {
		t.Errorf("ts = %d, want epoch milliseconds", body.TS)
	}
}

// /api/query must reject non-read-only SQL before it reaches the database, and
// answer with the old app's error strings.
func TestLegacyQueryValidation(t *testing.T) {
	h := newTestServer(t, legacyConfig(t))

	tests := []struct {
		name      string
		body      string
		wantCode  int
		wantError string
	}{
		{"missing sql", `{}`, http.StatusBadRequest, "sql_required"},
		{"empty sql", `{"sql":""}`, http.StatusBadRequest, "sql_required"},
		{"insert", `{"sql":"INSERT INTO t VALUES (1)"}`, http.StatusBadRequest, "only_select_allowed"},
		{"drop", `{"sql":"DROP TABLE t"}`, http.StatusBadRequest, "only_select_allowed"},
		{"chained", `{"sql":"SELECT 1; DROP TABLE t"}`, http.StatusBadRequest, "only_select_allowed"},
		{"malformed json", `{`, http.StatusBadRequest, "sql_required"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+testStaticToken)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantCode, rec.Body.String())
			}
			var errBody struct{ Error string }
			if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if errBody.Error != tc.wantError {
				t.Errorf("error = %q, want %q", errBody.Error, tc.wantError)
			}
		})
	}
}

// A valid SELECT gets past validation and fails closed at the DB layer (no DB in
// this test), proving the route never executes without a database and that
// allowRawSQL=false removes it entirely.
func TestLegacyQueryRequiresDatabase(t *testing.T) {
	h := newTestServer(t, legacyConfig(t))

	req := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(`{"sql":"SELECT 1"}`))
	req.Header.Set("Authorization", "Bearer "+testStaticToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 without a database", rec.Code)
	}
}

func TestLegacyRawSQLCanBeDisabledIndependently(t *testing.T) {
	cfg := legacyConfig(t)
	cfg.LegacyCompat.AllowRawSQL = false
	h := newTestServer(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(`{"sql":"SELECT 1"}`))
	req.Header.Set("Authorization", "Bearer "+testStaticToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when allowRawSQL is false", rec.Code)
	}

	// The rest of the compat surface must still be there.
	ping := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	ping.Header.Set("Authorization", "Bearer "+testStaticToken)
	pingRec := httptest.NewRecorder()
	h.ServeHTTP(pingRec, ping)
	if pingRec.Code != http.StatusOK {
		t.Errorf("ping status = %d, want 200", pingRec.Code)
	}
}

func TestLegacyOrderInvalidID(t *testing.T) {
	h := newTestServer(t, legacyConfig(t))

	req := httptest.NewRequest(http.MethodGet, "/api/orders/not-a-number", nil)
	req.Header.Set("Authorization", "Bearer "+testStaticToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var errBody struct{ Error string }
	_ = json.Unmarshal(rec.Body.Bytes(), &errBody)
	if errBody.Error != "invalid_id" {
		t.Errorf("error = %q, want invalid_id", errBody.Error)
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
