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
	cfg.Auth = config.AuthConfig{
		Username: testAuthUser,
		Password: testAuthPass,
		Secret:   testAuthSecret,
	}
	return cfg
}

// mustIssuedToken returns a valid credential for the test installation — the
// only kind there is.
func mustIssuedToken(t *testing.T) string {
	t.Helper()
	signer, err := auth.NewSigner(testAuthSecret)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	token, _, err := signer.Sign(testAuthUser, time.Minute)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return token
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

// There is exactly one credential: a token this installation issued and has not
// yet expired. Nothing else opens a route — no static token, no token signed by
// another installation, nothing that merely looks like a JWT.
func TestOnlyAnIssuedTokenAuthenticates(t *testing.T) {
	h := mustServer(t, authConfig(t)).Handler

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
		{"issued token", issued, true},
		{"expired token", expired, false},
		{"token from another installation", foreignToken, false},
		{"the password itself", testAuthPass, false},
		{"the signing secret", testAuthSecret, false},
		{"jwt-shaped garbage", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJkaWdpdHJhZGUifQ.sig", false},
		{"garbage", "nonsense", false},
		{"empty", "", false},
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

// A blank credential must stop the service rather than produce an exchange that
// accepts empty strings — which is an open door, not an inconvenience. The
// connector has no other way in, so there is nothing to fall back to.
func TestServerRefusesBlankCredentials(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config.AuthConfig)
		expect string
	}{
		{"no username", func(a *config.AuthConfig) { a.Username = "" }, "username"},
		{"no password", func(a *config.AuthConfig) { a.Password = "" }, "password"},
		{"no secret", func(a *config.AuthConfig) { a.Secret = "" }, "secret"},
		{"nothing at all", func(a *config.AuthConfig) { *a = config.AuthConfig{} }, "username"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := authConfig(t)
			tc.mutate(&cfg.Auth)

			_, err := newServerForTest(t, cfg)
			if err == nil {
				t.Fatal("expected NewServer to refuse a config with no usable credential")
			}
			if !strings.Contains(err.Error(), tc.expect) {
				t.Errorf("error %q should name the missing setting %q", err, tc.expect)
			}
		})
	}
}

// R4: /api/ping answers without touching the database, so an operator checking a
// connection gets a clear answer even while SQL Server is down. It is
// authenticated and rate-limited like everything else.
func TestPing(t *testing.T) {
	h := mustServer(t, authConfig(t)).Handler

	req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	req.Header.Set("Authorization", "Bearer "+mustIssuedToken(t))
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

// The raw-SQL route stays deleted — explicitly requested, and nothing calls it.
func TestRawSQLRouteStaysDeleted(t *testing.T) {
	h := mustServer(t, authConfig(t)).Handler

	req := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(`{"sql":"SELECT 1"}`))
	req.Header.Set("Authorization", "Bearer "+mustIssuedToken(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("POST /api/query = %d, want 404 — it must not come back", rec.Code)
	}
}
