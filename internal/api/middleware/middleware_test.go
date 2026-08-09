package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestAuth(t *testing.T) {
	// A nil verifier is the static-token-only configuration: the exchange is off.
	h := AuthWithExchange("secret-token", nil, okHandler())

	tests := []struct {
		name   string
		header string
		want   int
	}{
		{name: "valid", header: "Bearer secret-token", want: http.StatusOK},
		{name: "case-insensitive scheme", header: "bearer secret-token", want: http.StatusOK},
		{name: "missing", header: "", want: http.StatusUnauthorized},
		{name: "wrong token", header: "Bearer wrong", want: http.StatusUnauthorized},
		{name: "no scheme", header: "secret-token", want: http.StatusUnauthorized},
		{name: "extra parts", header: "Bearer secret token", want: http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/health", nil)
			if tt.header != "" {
				r.Header.Set("Authorization", tt.header)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Fatalf("expected %d, got %d", tt.want, w.Code)
			}
		})
	}
}

func TestRateLimiterAllowsBurstThenBlocks(t *testing.T) {
	l := NewRateLimiter(1, 3)
	now := time.Now()

	for i := 0; i < 3; i++ {
		if !l.allow("1.2.3.4", now) {
			t.Fatalf("request %d within burst should be allowed", i)
		}
	}
	if l.allow("1.2.3.4", now) {
		t.Fatalf("request beyond burst should be blocked")
	}

	// A different client has its own bucket.
	if !l.allow("5.6.7.8", now) {
		t.Fatalf("independent client should be allowed")
	}

	// Refill after 2 seconds at 1 token/s.
	if !l.allow("1.2.3.4", now.Add(2*time.Second)) {
		t.Fatalf("bucket should refill over time")
	}
}

func TestRateLimiterMiddlewareResponse(t *testing.T) {
	l := NewRateLimiter(0.0001, 1)
	h := l.Middleware(okHandler())

	r := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	r.RemoteAddr = "9.9.9.9:1234"

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("first request should pass, got %d", w.Code)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second request should be 429, got %d", w.Code)
	}
}
