package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/buildinfo"
	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
	"github.com/digitrade-e/digi-erp-connector/internal/queries"
)

func receiptServer(t *testing.T, cfg config.Config, writer *slowReceiptWriter) (*http.Server, error) {
	t.Helper()
	store, err := queries.NewStore(filepath.Join(t.TempDir(), "queries.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	deps := ServerDeps{QueryStore: store, Logger: testLogger{}}
	if writer != nil {
		deps.ReceiptWriter = writer
	}
	return NewServer(cfg, deps)
}

type testLogger struct{}

func (testLogger) Info(string)         {}
func (testLogger) Error(string, error) {}
func (testLogger) Warn(string)         {}
func (testLogger) Success(string)      {}
func (testLogger) Close() error        { return nil }

// slowReceiptWriter takes delay to answer, as a model (b) import does.
type slowReceiptWriter struct{ delay time.Duration }

func (s *slowReceiptWriter) Write(_ context.Context, req hasavshevet.ReceiptRequest) (*hasavshevet.ReceiptResult, error) {
	time.Sleep(s.delay)
	return &hasavshevet.ReceiptResult{ReceiptNumber: 7, IdempotencyKey: req.IdempotencyKey, Written: true, Import: hasavshevet.ImportOK}, nil
}

func receiptConfig(t *testing.T) config.Config {
	cfg := authConfig(t)
	cfg.ERP = config.ERPHasavshevet
	dir := t.TempDir()
	cfg.SendOrderDir = filepath.Join(dir, "orders")
	cfg.SendReceiptDir = filepath.Join(dir, "herp")
	return cfg
}

const validReceiptBody = `{"idempotencyKey":"3f2c9a1e-7b4d-4c8e-9a51-2d6f0b8e4c17","accountKey":"40217","settledDate":"2026-10-08",
"amountMinor":694700,"installments":1,"cardIssuer":"2","cardBrand":"2","last4":"4580","confirmationCode":"0123456",
"providerUid":"","remarks":"26004295","agentKey":"","name":"Levi","address":"","phone":"","email":""}`

// Without the receipt keys the route exists, needs a credential like every
// other, and answers 503 — the v1.0.17 surface plus this route.
func TestSendReceiptRoute_UnconfiguredIs503BehindAuth(t *testing.T) {
	srv, err := receiptServer(t, authConfig(t), nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/sendReceipt", strings.NewReader(validReceiptBody))
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d; want 401", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/sendReceipt", strings.NewReader(validReceiptBody))
	req.Header.Set("Authorization", "Bearer "+mustIssuedToken(t))
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "RECEIPTS_NOT_CONFIGURED") {
		t.Fatalf("unconfigured = %d %s; want 503 RECEIPTS_NOT_CONFIGURED", rec.Code, rec.Body.String())
	}

	// Only POST.
	req = httptest.NewRequest(http.MethodGet, "/api/sendReceipt", nil)
	req.Header.Set("Authorization", "Bearer "+mustIssuedToken(t))
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK || rec.Code == http.StatusServiceUnavailable {
		t.Errorf("GET /api/sendReceipt = %d; want it refused", rec.Code)
	}
}

// The daemon refuses to start on a receipt config the GUI also refuses.
func TestNewServer_RefusesBadReceiptConfig(t *testing.T) {
	sameDir := receiptConfig(t)
	sameDir.SendReceiptDir = sameDir.SendOrderDir + string(filepath.Separator)

	badNames := receiptConfig(t)
	badNames.ReceiptCardNames = config.ReceiptCardNamesConfig{Issuer: map[string]string{"6": strings.Repeat("א", 26)}}

	relative := receiptConfig(t)
	relative.SendReceiptDir = "herp"

	for name, cfg := range map[string]config.Config{"sendReceiptDir == sendOrderDir": sameDir, "card name too wide": badNames, "relative sendReceiptDir": relative} {
		t.Run(name, func(t *testing.T) {
			if _, err := receiptServer(t, cfg, &slowReceiptWriter{}); err == nil {
				t.Fatal("NewServer accepted it")
			}
			if err := hasavshevet.ValidateReceiptConfig(cfg); err == nil {
				t.Fatal("the GUI's validator accepted what the daemon refuses")
			}
		})
	}
}

// Configured receipts with no writer is a wiring bug, not a 503.
func TestNewServer_ConfiguredReceiptsNeedAWriter(t *testing.T) {
	if _, err := receiptServer(t, receiptConfig(t), nil); err == nil {
		t.Fatal("NewServer accepted sendReceiptDir without a receipt writer")
	}
}

// The route outlives the server's WriteTimeout: a model (b) import can take
// longer than any other route, and its answer must still arrive. Debug logging
// on, so the logging middleware's wrapper is in the path too.
func TestSendReceiptRoute_ExtendsItsWriteDeadline(t *testing.T) {
	cfg := receiptConfig(t)
	cfg.Debug = true
	srv, err := receiptServer(t, cfg, &slowReceiptWriter{delay: 600 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewUnstartedServer(srv.Handler)
	ts.Config.WriteTimeout = 150 * time.Millisecond
	ts.Start()
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/sendReceipt", bytes.NewReader([]byte(validReceiptBody)))
	req.Header.Set("Authorization", "Bearer "+mustIssuedToken(t))
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("the answer was lost past the server's WriteTimeout: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"receiptNumber":"7"`) {
		t.Fatalf("status = %d body = %s; want 200 with the receipt", resp.StatusCode, body)
	}
}

// /api/ping says which build is answering.
func TestPingCarriesVersion(t *testing.T) {
	h := mustServer(t, authConfig(t)).Handler
	req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	req.Header.Set("Authorization", "Bearer "+mustIssuedToken(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["version"] != buildinfo.Version || buildinfo.Version == "" {
		t.Errorf("ping version = %v; want %q", body["version"], buildinfo.Version)
	}
	if body["ok"] != true || body["ts"] == nil {
		t.Errorf("ping lost its v1.0.17 fields: %v", body)
	}
}
