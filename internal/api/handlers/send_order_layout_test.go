package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
)

// configuredQueue is newIndexedTestQueue with the order keys of cfg (its
// SendOrderDir is set to root/orders).
func configuredQueue(t *testing.T, root string, cfg config.Config) *hasavshevet.OrderQueue {
	t.Helper()
	cfg.ERP = config.ERPHasavshevet
	cfg.SendOrderDir = filepath.Join(root, "orders")
	if err := os.MkdirAll(cfg.SendOrderDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateOrders(); err != nil {
		t.Fatalf("test config refused: %v", err)
	}
	numStore := hasavshevet.NewOrderNumberStore(filepath.Join(cfg.SendOrderDir, "lastOrderNumber.json"))
	q := hasavshevet.NewOrderQueue(hasavshevet.NewSender(nil, cfg, numStore, &noopLogger{}), &noopLogger{})
	q.UseKeyIndex(filepath.Join(root, "data"))
	if err := q.Recover(); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	return q
}

// With sendOrderFormat legacy, a number its narrower fields would change is a
// 400 VALIDATION_ERROR naming the field, before anything is reserved; the
// official layout takes the same order.
func TestSendOrderHandler_LegacyLayoutRefusesANumberItWouldCut(t *testing.T) {
	for _, tc := range []struct {
		format string
		want   int
	}{
		{config.OrderFormatLegacy, http.StatusBadRequest},
		{"", http.StatusAccepted},
	} {
		t.Run("format="+tc.format, func(t *testing.T) {
			root := t.TempDir()
			q := configuredQueue(t, root, config.Config{SendOrderFormat: tc.format})
			body := validOrderBody()
			body["vatPercent"] = "10.25" // the legacy VAT field holds 4 characters
			w := sendOrderRequest(t, NewSendOrderHandler(q, true), body)
			if w.Code != tc.want {
				t.Fatalf("POST = %d %s; want %d", w.Code, w.Body.String(), tc.want)
			}
			if tc.want != http.StatusBadRequest {
				return
			}
			var resp struct {
				Code    string         `json:"code"`
				Details map[string]any `json:"details"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Code != "VALIDATION_ERROR" || resp.Details["field"] != "vatPercent" {
				t.Errorf("answer = %s; want VALIDATION_ERROR on vatPercent", w.Body.String())
			}
			if q.Pending() != 0 {
				t.Error("a refused request was queued")
			}
			if _, err := os.Stat(filepath.Join(root, "orders", "lastOrderNumber.json")); !os.IsNotExist(err) {
				t.Errorf("a refused request reserved a number: %v", err)
			}
		})
	}
}

// Model (a) end to end through the route: 202, then the order is the one file
// IMOVEIN_<n>.doc in the pickup folder.
func TestSendOrderHandler_PickupWritesTheOrderForTheScheduler(t *testing.T) {
	root := t.TempDir()
	pickup := filepath.Join(root, "herp")
	if err := os.MkdirAll(pickup, 0o755); err != nil {
		t.Fatal(err)
	}
	q := configuredQueue(t, root, config.Config{SendOrderPickupDir: pickup, SendOrderFormat: config.OrderFormatLegacy})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		select {
		case <-q.Done():
		case <-time.After(10 * time.Second):
			t.Error("the order worker did not exit")
		}
	})
	q.Start(ctx)

	body := validOrderBody()
	body["account"] = map[string]any{"accountKey": "CUST001", "fullName": "Test Customer"}
	w := sendOrderRequest(t, NewSendOrderHandler(q, true), body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("POST = %d %s; want 202", w.Code, w.Body.String())
	}
	waitDone(t, q, "1")
	entries, err := os.ReadDir(pickup)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "IMOVEIN_1.doc" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("pickup folder = %v; want [IMOVEIN_1.doc]", names)
	}
}
