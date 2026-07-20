package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
)

// newTestQueue returns an OrderQueue with a nil sender.
// The queue is not started, so submitted jobs are never executed.
// This is safe for handler-level tests that only exercise validation.
func newTestQueue() *hasavshevet.OrderQueue {
	return hasavshevet.NewOrderQueue(nil, &noopLogger{})
}

func newTestQueueWithNumberStore(t *testing.T) *hasavshevet.OrderQueue {
	t.Helper()
	dir := t.TempDir()
	numStore := hasavshevet.NewOrderNumberStore(filepath.Join(dir, "lastOrderNumber.json"))
	sender := hasavshevet.NewSender(nil, config.Config{}, numStore, &noopLogger{})
	return hasavshevet.NewOrderQueue(sender, &noopLogger{})
}

type noopLogger struct{}

func (l *noopLogger) Info(msg string)             {}
func (l *noopLogger) Error(msg string, err error) {}
func (l *noopLogger) Warn(msg string)             {}
func (l *noopLogger) Success(msg string)          {}
func (l *noopLogger) Close() error                { return nil }

func sendOrderRequest(t *testing.T, handler http.HandlerFunc, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/sendOrder", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler(w, req)
	return w
}

func ptr[T any](v T) *T { return &v }

// TestSendOrderHandler_EmptyBody returns 400 for an empty JSON object.
func TestSendOrderHandler_EmptyBody(t *testing.T) {
	h := NewSendOrderHandler(newTestQueue())
	w := sendOrderRequest(t, h, map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Errorf("empty body: got %d, want 400", w.Code)
	}
}

// TestSendOrderHandler_MissingDocumentType returns 400.
func TestSendOrderHandler_MissingDocumentType(t *testing.T) {
	h := NewSendOrderHandler(newTestQueue())
	body := validOrderBody()
	delete(body, "documentType")
	w := sendOrderRequest(t, h, body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing documentType: got %d, want 400", w.Code)
	}
}

// TestSendOrderHandler_InvalidDocumentType returns 400.
func TestSendOrderHandler_InvalidDocumentType(t *testing.T) {
	h := NewSendOrderHandler(newTestQueue())
	body := validOrderBody()
	body["documentType"] = "INVOICE"
	w := sendOrderRequest(t, h, body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid documentType: got %d, want 400", w.Code)
	}
}

// TestSendOrderHandler_MissingDiscount returns 400 (discount can be 0, but not absent).
func TestSendOrderHandler_MissingDiscount(t *testing.T) {
	h := NewSendOrderHandler(newTestQueue())
	body := validOrderBody()
	delete(body, "discount")
	w := sendOrderRequest(t, h, body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing discount: got %d, want 400", w.Code)
	}
}

// TestSendOrderHandler_ZeroQuantity returns 400 per Hasavshevet spec (line23 ≠ 0).
func TestSendOrderHandler_ZeroQuantity(t *testing.T) {
	h := NewSendOrderHandler(newTestQueue())
	body := validOrderBody()
	details := body["details"].([]any)
	details[0].(map[string]any)["quantity"] = 0.0
	w := sendOrderRequest(t, h, body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("zero quantity: got %d, want 400", w.Code)
	}
}

// TestSendOrderHandler_MissingSKU returns 400 per Hasavshevet spec (line22 required).
func TestSendOrderHandler_MissingSKU(t *testing.T) {
	h := NewSendOrderHandler(newTestQueue())
	body := validOrderBody()
	details := body["details"].([]any)
	delete(details[0].(map[string]any), "sku")
	w := sendOrderRequest(t, h, body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing sku: got %d, want 400", w.Code)
	}
}

// TestSendOrderHandler_ValidRequest returns 202 Accepted with a jobId.
func TestSendOrderHandler_ValidRequest(t *testing.T) {
	q := newTestQueueWithNumberStore(t)
	// Start the queue so Submit doesn't block (but with no sender, jobs are never processed)
	// We need to start the queue to prevent "queue full" on the channel send.
	// Actually, since defaultQueueSize=64 and we only submit once, Submit won't block
	// even without Start. The channel has capacity 64.
	h := NewSendOrderHandler(q)
	w := sendOrderRequest(t, h, validOrderBody())
	if w.Code != http.StatusAccepted {
		t.Errorf("valid request: got %d, want 202; body: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp["status"] != "queued" {
		t.Errorf("response status = %q, want 'queued'", resp["status"])
	}
	if resp["jobId"] != "1" {
		t.Errorf("response jobId = %v, want reserved lastOrderNumber '1'", resp["jobId"])
	}
}

// TestSendOrderHandler_AllDocumentTypes verifies ORDER, QUOATE, RETURN all return 202.
func TestSendOrderHandler_AllDocumentTypes(t *testing.T) {
	for _, dt := range []string{"ORDER", "QUOATE", "RETURN"} {
		t.Run(dt, func(t *testing.T) {
			h := NewSendOrderHandler(newTestQueue())
			body := validOrderBody()
			body["documentType"] = dt
			w := sendOrderRequest(t, h, body)
			if w.Code != http.StatusAccepted {
				t.Errorf("documentType=%s: got %d, want 202", dt, w.Code)
			}
		})
	}
}

// validOrderBody returns a minimal valid order request body.
func validOrderBody() map[string]any {
	return map[string]any{
		"documentType": "ORDER",
		"userExtId":    "CUST001",
		"dueDate":      "2026-03-01",
		"createdDate":  "2026-02-23",
		"comment":      "test order",
		"discount":     0.0,
		"historyId":    "HID-001",
		"total":        150.0,
		"currency":     `ש"ח`,
		"details": []any{
			map[string]any{
				"title":         "Test Item",
				"sku":           "SKU-001",
				"quantity":      2.0,
				"originalPrice": 75.0,
				"singlePrice":   75.0,
				"totalPrice":    150.0,
				"discount":      0.0,
			},
		},
	}
}
