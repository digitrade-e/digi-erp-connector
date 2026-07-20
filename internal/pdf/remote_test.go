package pdf

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRemoteFetcher_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/pdf-template/connector/render/") {
			http.Error(w, "wrong path: "+r.URL.Path, http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("documentNumber") != "ORD-1" {
			http.Error(w, "missing documentNumber", http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("userExtId") != "USR-9" {
			http.Error(w, "missing userExtId", http.StatusBadRequest)
			return
		}
		if r.Header.Get("User-Agent") == "" {
			http.Error(w, "missing user-agent", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><body>ok</body>"))
	}))
	defer srv.Close()

	rf := NewRemoteFetcher(srv.URL, 5*time.Second, "digi-erp-connector/test")
	body, err := rf.Fetch(context.Background(), "abcdef", "order", "ORD-1", "USR-9")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(string(body), "ok") {
		t.Fatalf("unexpected body: %s", string(body))
	}
}

func TestRemoteFetcher_4xxIncludesPreview(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"token not found"}`))
	}))
	defer srv.Close()

	rf := NewRemoteFetcher(srv.URL, 2*time.Second, "digi-erp-connector/test")
	_, err := rf.Fetch(context.Background(), "x", "order", "1", "1")
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected 404 in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "token not found") {
		t.Fatalf("expected body preview in error, got: %v", err)
	}
}

func TestRemoteFetcher_5xxFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("oops"))
	}))
	defer srv.Close()

	rf := NewRemoteFetcher(srv.URL, 2*time.Second, "digi-erp-connector/test")
	_, err := rf.Fetch(context.Background(), "x", "order", "1", "1")
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected 500 in error, got: %v", err)
	}
}

func TestRemoteFetcher_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte("late"))
	}))
	defer srv.Close()

	rf := NewRemoteFetcher(srv.URL, 50*time.Millisecond, "digi-erp-connector/test")
	_, err := rf.Fetch(context.Background(), "x", "order", "1", "1")
	if err == nil {
		t.Fatalf("expected timeout error")
	}
}

func TestRemoteFetcher_EmptyBaseURL(t *testing.T) {
	rf := NewRemoteFetcher("", 1*time.Second, "digi-erp-connector/test")
	_, err := rf.Fetch(context.Background(), "x", "order", "1", "1")
	if err == nil {
		t.Fatalf("expected error for empty baseURL")
	}
}

func TestRemoteFetcher_EmptyToken(t *testing.T) {
	rf := NewRemoteFetcher("http://example.com", 1*time.Second, "digi-erp-connector/test")
	_, err := rf.Fetch(context.Background(), "", "order", "1", "1")
	if err == nil {
		t.Fatalf("expected error for empty token")
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	cases := map[string]string{
		"":                                            "",
		"   ":                                         "",
		"https://api.example.com":                     "https://api.example.com",
		"https://api.example.com/":                    "https://api.example.com",
		"https://api.example.com/api":                 "https://api.example.com",
		"https://api.example.com/api/pdf-template/connector/render/abc123?documentNumber={documentNumber}&userExtId={userExtId}": "https://api.example.com",
		"http://localhost:4000":                       "http://localhost:4000",
		"localhost:4000":                              "https://localhost:4000",
		"api.example.com":                             "https://api.example.com",
		"https://cpadmin.maorders.co.il/api/pdf-template/connector/render/61c0...?documentNumber={documentNumber}&userExtId={userExtId}": "https://cpadmin.maorders.co.il",
	}
	for in, want := range cases {
		got := normalizeBaseURL(in)
		if got != want {
			t.Errorf("normalizeBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRemoteFetcher_NormalizesBaseURLOnConstruct(t *testing.T) {
	// Smoke-test: construction does not panic when the operator pastes a URL
	// containing path + literal placeholders. Real network call is covered by
	// TestRemoteFetcher_HappyPath which uses an httptest server.
	rf := NewRemoteFetcher(
		"https://example.com/api/pdf-template/connector/render/abc?documentNumber={documentNumber}&userExtId={userExtId}",
		1*time.Second,
		"digi-erp-connector/test",
	)
	if rf == nil {
		t.Fatalf("expected non-nil fetcher")
	}
	if rf.baseURL != "https://example.com" {
		t.Fatalf("baseURL not normalized: got %q", rf.baseURL)
	}
}

func TestMaskToken(t *testing.T) {
	cases := map[string]string{
		"":                   "",
		"abc":                "***",
		"abcd":               "****",
		"abcdef":             "**cdef",
		"0123456789abcdef01": "**************ef01",
	}
	for in, want := range cases {
		got := MaskToken(in)
		if got != want {
			t.Errorf("MaskToken(%q) = %q, want %q", in, got, want)
		}
	}
}
