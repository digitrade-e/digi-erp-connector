package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/auth"
	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// These pin the requirements in docs/connector-adaptation-plan.md §2 (R1–R6).
// erp-manager is stricter than the contract it was written against, so each
// assertion here corresponds to a way a caller breaks in production rather than
// to a style preference.

const (
	testAuthUser   = "bfl-reads"
	testAuthPass   = "operator-set-password"
	testAuthSecret = "0123456789abcdef0123456789abcdef"
)

func authConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.BearerToken = testStaticToken
	cfg.Auth = config.AuthConfig{
		Enabled:  true,
		Username: testAuthUser,
		Password: testAuthPass,
		Secret:   testAuthSecret,
	}
	return cfg
}

func postToken(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/auth/token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// R1: 200 exactly, with access_token present. A caller treats 201 or 204 as a
// failure, and stores a missing access_token as null — then sends an empty
// credential and loops on 401.
func TestTokenExchangeReturns200WithAccessToken(t *testing.T) {
	h := mustServer(t, authConfig(t)).Handler

	rec := postToken(t, h, `{"username":"`+testAuthUser+`","password":"`+testAuthPass+`"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want exactly 200 (body %s)", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	raw, present := body["access_token"]
	if !present {
		t.Fatalf("access_token missing from a 200 response: %s", rec.Body.String())
	}
	issued, _ := raw.(string)
	if strings.TrimSpace(issued) == "" {
		t.Fatal("access_token present but empty")
	}

	// It must be a token this installation can verify.
	signer, err := auth.NewSigner(testAuthSecret)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	claims, err := signer.Verify(issued)
	if err != nil {
		t.Fatalf("issued token does not verify: %v", err)
	}
	if claims.Subject != testAuthUser {
		t.Errorf("sub = %q, want %q", claims.Subject, testAuthUser)
	}
}

// R2: the issued token authenticates the data routes, and so does the static
// bearer token. Dual acceptance is what lets the eventual static-token migration
// be a configuration change on the caller's side.
func TestBothCredentialsAreAccepted(t *testing.T) {
	cfg := authConfig(t)
	h := mustServer(t, cfg).Handler

	signer, _ := auth.NewSigner(testAuthSecret)
	issued, _, err := signer.Sign(testAuthUser, time.Minute)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	expired, _, _ := signer.Sign(testAuthUser, -time.Minute)
	foreign, _ := auth.NewSigner("a-different-installations-secret")
	foreignToken, _, _ := foreign.Sign(testAuthUser, time.Minute)

	for _, tc := range []struct {
		name       string
		credential string
		wantAuthed bool
	}{
		{"static bearer token", testStaticToken, true},
		{"issued token", issued, true},
		{"expired token", expired, false},
		{"token from another installation", foreignToken, false},
		{"garbage", "nonsense", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/folders/list", nil)
			req.Header.Set("Authorization", "Bearer "+tc.credential)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			authed := rec.Code != http.StatusUnauthorized
			if authed != tc.wantAuthed {
				t.Errorf("status %d: authenticated=%v, want %v", rec.Code, authed, tc.wantAuthed)
			}
		})
	}
}

// R3: every rejection is exactly 401. A 403 or a 500 skips the caller's
// re-authenticate branch and leaves a cached credential broken until a human
// clears it.
func TestRejectionsAreExactly401(t *testing.T) {
	h := mustServer(t, authConfig(t)).Handler

	signer, _ := auth.NewSigner(testAuthSecret)
	expired, _, _ := signer.Sign(testAuthUser, -time.Minute)

	for _, credential := range []string{"", "wrong", expired, "Bearer", "a.b.c"} {
		req := httptest.NewRequest(http.MethodGet, "/api/custom_sql", nil)
		if credential != "" {
			req.Header.Set("Authorization", "Bearer "+credential)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("credential %q gave %d, want exactly 401", credential, rec.Code)
		}
	}
}

// The credentials that shipped with the old Node app must not work anywhere.
func TestShippedDefaultCredentialsAreRejected(t *testing.T) {
	h := mustServer(t, authConfig(t)).Handler

	rec := postToken(t, h, `{"username":"digitrade","password":"123456"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("the old default credentials returned %d, want 401", rec.Code)
	}
}

func TestTokenExchangeRejectsBadCredentials(t *testing.T) {
	h := mustServer(t, authConfig(t)).Handler

	for _, body := range []string{
		`{"username":"` + testAuthUser + `","password":"wrong"}`,
		`{"username":"nobody","password":"` + testAuthPass + `"}`,
		`{"username":"","password":""}`,
		`{}`,
		`not json`,
	} {
		rec := postToken(t, h, body)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("body %s gave %d, want 401", body, rec.Code)
		}
	}
}

// The exchange is opt-in: without it the route does not exist and only the
// static token authenticates.
func TestExchangeAbsentWhenDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.BearerToken = testStaticToken
	h := mustServer(t, cfg).Handler

	rec := postToken(t, h, `{"username":"x","password":"y"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST /auth/token = %d, want 404 when auth.enabled is false", rec.Code)
	}
}

// A half-configured block must stop the service rather than expose an exchange
// that accepts blanks.
func TestExchangeRequiresCompleteConfig(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config.AuthConfig)
		expect string
	}{
		{"no username", func(a *config.AuthConfig) { a.Username = "" }, "username"},
		{"no password", func(a *config.AuthConfig) { a.Password = "" }, "password"},
		{"no secret", func(a *config.AuthConfig) { a.Secret = "" }, "secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := authConfig(t)
			tc.mutate(&cfg.Auth)

			_, err := newServerForTest(t, cfg)
			if err == nil {
				t.Fatal("expected NewServer to refuse an incomplete auth block")
			}
			if !strings.Contains(err.Error(), tc.expect) {
				t.Errorf("error %q should name the missing setting %q", err, tc.expect)
			}
		})
	}
}

// An installation authenticates with one credential. Which one is a deployment
// decision, so both fields exist — but a server with neither would 401 every
// request, which reads as a credential problem and sends whoever debugs it
// somewhere else entirely.
func TestAtLeastOneCredentialIsRequired(t *testing.T) {
	cfg := config.Default()
	cfg.BearerToken = ""

	if _, err := newServerForTest(t, cfg); err == nil {
		t.Fatal("expected NewServer to refuse a config with no credential at all")
	} else if !strings.Contains(err.Error(), "no credential configured") {
		t.Errorf("error %q should say no credential is configured", err)
	}
}

// Exchange-only: no static token at all. This is what a box serving erp-manager
// runs, and the empty bearerToken must not become a credential of its own — an
// empty or absent Authorization value has to fail like any other.
func TestExchangeOnlyInstallation(t *testing.T) {
	cfg := authConfig(t)
	cfg.BearerToken = ""
	h := mustServer(t, cfg).Handler

	signer, _ := auth.NewSigner(testAuthSecret)
	issued, _, err := signer.Sign(testAuthUser, time.Minute)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	for _, tc := range []struct {
		name       string
		credential string
		wantAuthed bool
	}{
		{"issued token", issued, true},
		{"the retired static token", testStaticToken, false},
		{"empty credential", "", false},
		{"a single space", " ", false},
		{"garbage", "nonsense", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/folders/list", nil)
			req.Header.Set("Authorization", "Bearer "+tc.credential)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			authed := rec.Code != http.StatusUnauthorized
			if authed != tc.wantAuthed {
				t.Errorf("status %d: authenticated=%v, want %v", rec.Code, authed, tc.wantAuthed)
			}
		})
	}

	// The exchange itself still works — it is the only way in.
	if rec := postToken(t, h, `{"username":"`+testAuthUser+`","password":"`+testAuthPass+`"}`); rec.Code != http.StatusOK {
		t.Errorf("exchange on an exchange-only install = %d, want 200", rec.Code)
	}
}

// R4: /api/ping answers without touching the database, so an operator checking a
// connection gets a clear answer even while SQL Server is down. It is
// authenticated and rate-limited like everything else.
func TestPing(t *testing.T) {
	h := mustServer(t, authConfig(t)).Handler

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
	if body.TS < 1_000_000_000_000 {
		t.Errorf("ts = %d, want epoch milliseconds", body.TS)
	}
}

func TestPingRequiresAuthentication(t *testing.T) {
	h := mustServer(t, authConfig(t)).Handler

	req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated ping = %d, want 401", rec.Code)
	}
}

// Ping exists whether or not the exchange is enabled: the static-token callers
// need it too.
func TestPingExistsWithoutTheExchange(t *testing.T) {
	cfg := config.Default()
	cfg.BearerToken = testStaticToken
	h := mustServer(t, cfg).Handler

	req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	req.Header.Set("Authorization", "Bearer "+testStaticToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("ping without the exchange = %d, want 200", rec.Code)
	}
}

// The raw-SQL route stays deleted — explicitly requested, and nothing calls it.
func TestRawSQLRouteStaysDeleted(t *testing.T) {
	h := mustServer(t, authConfig(t)).Handler

	req := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(`{"sql":"SELECT 1"}`))
	req.Header.Set("Authorization", "Bearer "+testStaticToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("POST /api/query = %d, want 404 — it must not come back", rec.Code)
	}
}
