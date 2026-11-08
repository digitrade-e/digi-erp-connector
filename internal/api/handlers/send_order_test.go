package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	h := NewSendOrderHandler(newTestQueue(), true)
	w := sendOrderRequest(t, h, map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Errorf("empty body: got %d, want 400", w.Code)
	}
}

// TestSendOrderHandler_MissingDocumentType returns 400.
func TestSendOrderHandler_MissingDocumentType(t *testing.T) {
	h := NewSendOrderHandler(newTestQueue(), true)
	body := validOrderBody()
	delete(body, "documentType")
	w := sendOrderRequest(t, h, body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing documentType: got %d, want 400", w.Code)
	}
}

// TestSendOrderHandler_InvalidDocumentType returns 400.
func TestSendOrderHandler_InvalidDocumentType(t *testing.T) {
	h := NewSendOrderHandler(newTestQueue(), true)
	body := validOrderBody()
	body["documentType"] = "INVOICE"
	w := sendOrderRequest(t, h, body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid documentType: got %d, want 400", w.Code)
	}
}

// TestSendOrderHandler_MissingDiscount returns 400 (discount can be 0, but not absent).
func TestSendOrderHandler_MissingDiscount(t *testing.T) {
	h := NewSendOrderHandler(newTestQueue(), true)
	body := validOrderBody()
	delete(body, "discount")
	w := sendOrderRequest(t, h, body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing discount: got %d, want 400", w.Code)
	}
}

// TestSendOrderHandler_ZeroQuantity returns 400 per Hasavshevet spec (line23 ≠ 0).
func TestSendOrderHandler_ZeroQuantity(t *testing.T) {
	h := NewSendOrderHandler(newTestQueue(), true)
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
	h := NewSendOrderHandler(newTestQueue(), true)
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
	h := NewSendOrderHandler(q, true)
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
			h := NewSendOrderHandler(newTestQueue(), true)
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

// A connector deployed only to serve queries has no sendOrderDir and cannot
// build IMOVEIN files. It must say so immediately: returning 202 and failing
// inside the worker reads as success to the caller.
func TestSendOrderHandler_NotConfigured(t *testing.T) {
	h := NewSendOrderHandler(newTestQueue(), false)
	w := sendOrderRequest(t, h, validOrderBody())

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("got %d, want 501 when orders are not configured", w.Code)
	}

	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Code != "ORDERS_NOT_CONFIGURED" {
		t.Errorf("code = %q, want ORDERS_NOT_CONFIGURED", body.Code)
	}
}

// The refusal must come before any work: an unconfigured node rejects even a
// body that would otherwise be invalid, rather than reporting a validation error
// that hides the real problem.
func TestSendOrderHandler_NotConfiguredBeatsValidation(t *testing.T) {
	h := NewSendOrderHandler(newTestQueue(), false)
	w := sendOrderRequest(t, h, map[string]any{})

	if w.Code != http.StatusNotImplemented {
		t.Errorf("got %d, want 501 — configuration is checked before validation", w.Code)
	}
}

// newIndexedTestQueue is a queue with the historyId index, as the daemon
// builds it. Not started: accepted orders stay queued.
func newIndexedTestQueue(t *testing.T, root string) *hasavshevet.OrderQueue {
	t.Helper()
	sendDir := filepath.Join(root, "orders")
	if err := os.MkdirAll(sendDir, 0o755); err != nil {
		t.Fatal(err)
	}
	numStore := hasavshevet.NewOrderNumberStore(filepath.Join(sendDir, "lastOrderNumber.json"))
	sender := hasavshevet.NewSender(nil, config.Config{SendOrderDir: sendDir}, numStore, &noopLogger{})
	q := hasavshevet.NewOrderQueue(sender, &noopLogger{})
	q.UseKeyIndex(filepath.Join(root, "data"))
	if err := q.Recover(); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	return q
}

func orderIndexFile(root, historyID string) string {
	sum := sha256.Sum256([]byte(historyID))
	return filepath.Join(hasavshevet.OrderIndexDir(filepath.Join(root, "data")), hex.EncodeToString(sum[:])+".json")
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	return m
}

// A retry, redelivery or resend of one release (same historyId) answers the
// first jobId — 202, the same body shape, plus replay: true — and creates no
// second order. A new historyId is a new order.
func TestSendOrderHandler_ReplayAnswersTheFirstJobID(t *testing.T) {
	root := t.TempDir()
	h := NewSendOrderHandler(newIndexedTestQueue(t, root), true)
	body := validOrderBody()
	body["historyId"] = "4711-3f2c9a1e-7b4d-4c8e-9a51-2d6f0b8e4c17"

	first := sendOrderRequest(t, h, body)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first POST = %d %s; want 202", first.Code, first.Body.String())
	}
	m := decodeBody(t, first)
	if m["jobId"] != "1" || m["status"] != "queued" {
		t.Fatalf("first answer = %v; want job 1 queued", m)
	}
	// The first answer keeps exactly the shape callers already parse.
	if _, has := m["replay"]; has || len(m) != 3 || m["meta"] == nil {
		t.Errorf("first answer = %v; want only status, jobId, meta", m)
	}

	for i := 0; i < 2; i++ {
		again := sendOrderRequest(t, h, body)
		if again.Code != http.StatusAccepted {
			t.Fatalf("repeat = %d %s; want 202", again.Code, again.Body.String())
		}
		m := decodeBody(t, again)
		if m["jobId"] != "1" || m["status"] != "queued" || m["replay"] != true || m["meta"] == nil {
			t.Fatalf("repeat answer = %v; want job 1, queued, replay true", m)
		}
	}

	body["historyId"] = "4711-0b8e4c17-2d6f-4c8e-9a51-3f2c9a1e7b4d"
	if m := decodeBody(t, sendOrderRequest(t, h, body)); m["jobId"] != "2" || m["replay"] != nil {
		t.Errorf("a new historyId = %v; want job 2, no replay", m)
	}
}

// An index entry that cannot be trusted is never answered 202 and never gets
// a second number: 500 ORDER_INDEX_UNREADABLE, with no path in the message.
func TestSendOrderHandler_UnreadableIndexEntryIsNot202(t *testing.T) {
	root := t.TempDir()
	h := NewSendOrderHandler(newIndexedTestQueue(t, root), true)
	body := validOrderBody()
	if w := sendOrderRequest(t, h, body); w.Code != http.StatusAccepted {
		t.Fatalf("first POST = %d", w.Code)
	}
	if err := os.WriteFile(orderIndexFile(root, "HID-001"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	w := sendOrderRequest(t, h, body)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("POST over an unreadable entry = %d %s; want 500", w.Code, w.Body.String())
	}
	m := decodeBody(t, w)
	if m["code"] != "ORDER_INDEX_UNREADABLE" {
		t.Errorf("code = %v; want ORDER_INDEX_UNREADABLE", m["code"])
	}
	if strings.Contains(w.Body.String(), root) || strings.Contains(w.Body.String(), ".json") {
		t.Errorf("the error names a file: %s", w.Body.String())
	}
	b, _ := os.ReadFile(filepath.Join(root, "orders", "lastOrderNumber.json"))
	if !strings.Contains(string(b), `"lastOrderNumber": 1`) {
		t.Errorf("counter = %s; the refusal reserved a number", b)
	}
}

// After a restart the status endpoint answers a jobId from the index; a
// reserved entry with no job in memory is "unknown".
func TestSendOrderStatusHandler_AnswersFromTheIndexAfterRestart(t *testing.T) {
	root := t.TempDir()
	if w := sendOrderRequest(t, NewSendOrderHandler(newIndexedTestQueue(t, root), true), validOrderBody()); w.Code != http.StatusAccepted {
		t.Fatalf("POST = %d", w.Code)
	}
	restarted := newIndexedTestQueue(t, root)

	req := httptest.NewRequest(http.MethodGet, "/api/sendOrder/1", nil)
	req.SetPathValue("jobId", "1")
	w := httptest.NewRecorder()
	NewSendOrderStatusHandler(restarted)(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status of job 1 after restart = %d %s; want 200", w.Code, w.Body.String())
	}
	if m := decodeBody(t, w); m["status"] != "unknown" || m["jobId"] != "1" || m["orderNumber"] != float64(1) {
		t.Errorf("status = %v; want job 1, unknown, order 1", m)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/sendOrder/2", nil)
	req.SetPathValue("jobId", "2")
	w = httptest.NewRecorder()
	NewSendOrderStatusHandler(restarted)(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status of an unknown job = %d; want 404", w.Code)
	}
}

// A key whose order failed before anything was written is retryable: the
// resend is queued again under the SAME number — 202, the first jobId,
// replay: true, status queued — and no new number is taken. (Planted by hand
// here, which is also the documented way back for an "unknown" order.)
func TestSendOrderHandler_RetryableKeyIsQueuedAgainUnderItsNumber(t *testing.T) {
	root := t.TempDir()
	q := newIndexedTestQueue(t, root)
	h := NewSendOrderHandler(q, true)
	if w := sendOrderRequest(t, h, validOrderBody()); w.Code != http.StatusAccepted {
		t.Fatalf("first POST = %d", w.Code)
	}
	restarted := newIndexedTestQueue(t, root)
	entry := `{"key":"HID-001","number":1,"state":"retryable","error":"open IMOVEIN.doc: sharing violation","reservedAt":"2026-11-05T10:00:00Z"}`
	if err := os.WriteFile(orderIndexFile(root, "HID-001"), []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}

	w := sendOrderRequest(t, NewSendOrderHandler(restarted, true), validOrderBody())
	if w.Code != http.StatusAccepted {
		t.Fatalf("resend = %d %s; want 202", w.Code, w.Body.String())
	}
	if m := decodeBody(t, w); m["jobId"] != "1" || m["status"] != "queued" || m["replay"] != true {
		t.Errorf("resend answer = %v; want job 1, queued, replay true", m)
	}
	if restarted.Pending() != 1 {
		t.Errorf("queued %d; want the order queued again once", restarted.Pending())
	}
	b, _ := os.ReadFile(filepath.Join(root, "orders", "lastOrderNumber.json"))
	if !strings.Contains(string(b), `"lastOrderNumber": 1`) {
		t.Errorf("counter = %s; a retry must not take a new number", b)
	}
}

// Each refusal says what it is, and only a transient one tells the caller to
// retry with the same historyId.
func TestWriteSendOrderError(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		status    int
		code      string
		saysRetry bool
	}{
		{"this key's entry unreadable", fmt.Errorf("%w: /data/orders/index/ab.json: bad", hasavshevet.ErrOrderIndexUnreadable), 500, "ORDER_INDEX_UNREADABLE", false},
		{"queue full", fmt.Errorf("%w (capacity 64)", hasavshevet.ErrQueueFull), 503, "QUEUE_FULL", false},
		{"queue closed", hasavshevet.ErrQueueClosed, 503, "QUEUE_FULL", false},
		{"counter unreadable", fmt.Errorf("recover the order index: %w: /x/lastOrderNumber.json", hasavshevet.ErrOrderCounterUnreadable), 503, "ORDER_COUNTER_UNREADABLE", false},
		{"index not writable", errors.New("persist order index entry for order 7: disk full"), 503, "ORDER_NOT_QUEUED", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeSendOrderError(w, tc.err)
			if w.Code != tc.status {
				t.Errorf("status = %d; want %d", w.Code, tc.status)
			}
			var body struct {
				Code    string `json:"code"`
				Message string `json:"message"`
				Error   string `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Code != tc.code {
				t.Errorf("code = %q; want %q", body.Code, tc.code)
			}
			text := w.Body.String()
			if got := strings.Contains(text, "retry with the same historyId"); got != tc.saysRetry {
				t.Errorf("says retry = %v; want %v (%s)", got, tc.saysRetry, text)
			}
			if strings.Contains(text, "/data/") || strings.Contains(text, "lastOrderNumber.json:") {
				t.Errorf("the answer echoes a path: %s", text)
			}
		})
	}
}

// IMOVEIN byte spans the document fields land in (0-based, end exclusive),
// from imovein.go's field widths: line3 MyID, line4 DocumentID, line8
// Asmahta2, line12 warehouse, line18 VAT.
var imoveinSpans = map[string][2]int{
	"line3": {15, 24}, "line4": {24, 26}, "line8": {176, 185}, "line12": {214, 223}, "line18": {497, 502},
}

func imoveinField(doc []byte, line string) string {
	s := imoveinSpans[line]
	return strings.TrimRight(string(doc[s[0]:s[1]]), " ")
}

// startedIndexedQueue is newIndexedTestQueue with its worker running.
func startedIndexedQueue(t *testing.T, root string) *hasavshevet.OrderQueue {
	t.Helper()
	q := newIndexedTestQueue(t, root)
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
	return q
}

func waitDone(t *testing.T, q *hasavshevet.OrderQueue, jobID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if r, ok := q.Status(jobID); ok && (r.Status == hasavshevet.JobStatusDone || r.Status == hasavshevet.JobStatusFailed) {
			if r.Status != hasavshevet.JobStatusDone {
				t.Fatalf("job %s failed: %v", jobID, r.Err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s never finished", jobID)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The document fields (DIG-1295) reach IMOVEIN: as JSON numbers or strings,
// and null is the same as absent (today's values).
func TestSendOrderHandler_DocumentFieldsReachTheFile(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields map[string]any
		want   map[string]string
	}{
		{"numbers", map[string]any{"documentId": 31, "warehouse": 81, "vatPercent": 17, "asmahta2": 26004295},
			map[string]string{"line4": "31", "line12": "81", "line18": "17.00", "line8": "26004295", "line3": "1"}},
		{"strings", map[string]any{"documentId": "31", "warehouse": "8", "vatPercent": "18.5", "asmahta2": "000123"},
			map[string]string{"line4": "31", "line12": "8", "line18": "18.50", "line8": "000123", "line3": "1"}},
		{"null is absent", map[string]any{"documentId": nil, "warehouse": nil, "vatPercent": nil, "asmahta2": nil},
			map[string]string{"line4": "30", "line12": "1", "line18": "18.00", "line8": "1", "line3": "1"}},
		{"absent", map[string]any{},
			map[string]string{"line4": "30", "line12": "1", "line18": "18.00", "line8": "1", "line3": "1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			q := startedIndexedQueue(t, root)
			body := validOrderBody()
			body["account"] = map[string]any{"accountKey": "CUST001", "fullName": "Test Customer"}
			for k, v := range tc.fields {
				body[k] = v
			}
			w := sendOrderRequest(t, NewSendOrderHandler(q, true), body)
			if w.Code != http.StatusAccepted {
				t.Fatalf("POST = %d %s; want 202", w.Code, w.Body.String())
			}
			waitDone(t, q, "1")
			doc, err := os.ReadFile(filepath.Join(root, "orders", "history", "1", "IMOVEIN_1.doc"))
			if err != nil {
				t.Fatal(err)
			}
			for line, want := range tc.want {
				if got := imoveinField(doc, line); got != want {
					t.Errorf("%s = %q; want %q", line, got, want)
				}
			}
		})
	}
}

// A bad document field is a 400 VALIDATION_ERROR naming the field, before
// anything is reserved — never cut to fit or rounded.
func TestSendOrderHandler_RefusesABadDocumentField(t *testing.T) {
	for _, tc := range []struct {
		field string
		value any
	}{
		{"documentId", 0}, {"documentId", 100}, {"documentId", 31.5}, {"documentId", "3a"},
		{"documentId", true}, {"documentId", -1}, {"documentId", ""}, {"documentId", map[string]any{}},
		{"warehouse", 0}, {"warehouse", 1000000000}, {"warehouse", 1.5}, {"warehouse", "81 "},
		{"vatPercent", 100}, {"vatPercent", 18.555}, {"vatPercent", "x"}, {"vatPercent", -1}, {"vatPercent", "1e1"}, {"vatPercent", []any{17}},
		{"asmahta2", 1234567890}, {"asmahta2", "12a"}, {"asmahta2", ""}, {"asmahta2", 12.5}, {"asmahta2", -3}, {"asmahta2", "1234567890"},
	} {
		t.Run(fmt.Sprintf("%s=%v", tc.field, tc.value), func(t *testing.T) {
			root := t.TempDir()
			q := newIndexedTestQueue(t, root)
			body := validOrderBody()
			body[tc.field] = tc.value
			w := sendOrderRequest(t, NewSendOrderHandler(q, true), body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("POST = %d %s; want 400", w.Code, w.Body.String())
			}
			var resp struct {
				Code    string         `json:"code"`
				Details map[string]any `json:"details"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Code != "VALIDATION_ERROR" || resp.Details["field"] != tc.field {
				t.Errorf("answer = %s; want VALIDATION_ERROR on %s", w.Body.String(), tc.field)
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
