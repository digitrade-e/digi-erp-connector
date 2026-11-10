package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Data routes carry no rate limit (Ilya, 2026-11-10): a shop's buyers asking
// for price and stock all arrive from the shop's one address.
func TestDataRoutesAreNotRateLimited(t *testing.T) {
	h := mustServer(t, authConfig(t)).Handler
	token := mustIssuedToken(t)
	for i := 0; i < 300; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.RemoteAddr = "198.51.100.7:40000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d from one address: %d — a data route is limited", i, rec.Code)
		}
	}
}

// The credential exchange stays throttled: password guessing is shed cheaply.
func TestTokenExchangeIsStillRateLimited(t *testing.T) {
	h := mustServer(t, authConfig(t)).Handler
	for i := 0; i < 200; i++ {
		req := httptest.NewRequest(http.MethodPost, "/auth/token", strings.NewReader(`{"username":"guess","password":"guess-number-0000"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "198.51.100.8:40000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			return
		}
	}
	t.Fatal("200 password guesses from one address in a burst were never throttled")
}

// A slow saved query must be able to answer: the write deadline follows the
// query timeout (it was a fixed 60 s, under the 600 s a config may set).
func TestWriteTimeoutOutlastsTheQueryTimeout(t *testing.T) {
	for _, c := range []struct {
		seconds int
		want    time.Duration
	}{{0, 60 * time.Second}, {30, 60 * time.Second}, {120, 150 * time.Second}, {600, 630 * time.Second}} {
		cfg := authConfig(t)
		cfg.Queries.TimeoutSeconds = c.seconds
		if got := mustServer(t, cfg).WriteTimeout; got != c.want {
			t.Errorf("queries.timeoutSeconds %d: WriteTimeout %s, want %s", c.seconds, got, c.want)
		}
	}
}
