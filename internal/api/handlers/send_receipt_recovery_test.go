package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
)

// Plan 056 §5.5.7 / §7.5, at the route: what the backend sees, through the real
// writer, for the backend's own bytes. The backend stops a receipt only on a
// 400 and retries everything else with the same idempotencyKey, so each of
// these must be a retryable answer that, once retried, gives ONE receipt.

func (in *receiptInstall) docs(t *testing.T) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(in.sendDir, "KUPAIN_*.doc"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func wireRequest(t *testing.T) hasavshevet.ReceiptRequest {
	t.Helper()
	var r hasavshevet.ReceiptRequest
	if err := json.Unmarshal(wireBytes(t), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func receiptNumberOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var b struct {
		ReceiptNumber string `json:"receiptNumber"`
		Written       bool   `json:"written"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil || !b.Written {
		t.Fatalf("not a written receipt: %s", rec.Body.String())
	}
	return b.ReceiptNumber
}

// A repeated key is the same receipt: the same number, and no second file.
func TestSendReceipt_RepeatedKeyIsTheSameNumberAndNoSecondFile(t *testing.T) {
	in := newReceiptInstall(t, 500)
	h := NewSendReceiptHandler(in.writer, true, &noopLogger{})

	first := postReceipt(t, h, wireBytes(t))
	if first.Code != http.StatusOK {
		t.Fatalf("first = %d %s", first.Code, first.Body.String())
	}
	doc := filepath.Join(in.sendDir, "KUPAIN_500.doc")
	before, err := os.ReadFile(doc)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		again := postReceipt(t, h, wireBytes(t))
		if again.Code != http.StatusOK || receiptNumberOf(t, again) != "500" {
			t.Fatalf("repeat = %d %s; want 200 with 500", again.Code, again.Body.String())
		}
	}
	if docs := in.docs(t); len(docs) != 1 || docs[0] != doc {
		t.Errorf("herp holds %v; want only KUPAIN_500.doc", docs)
	}
	if after, _ := os.ReadFile(doc); !bytes.Equal(after, before) {
		t.Error("a repeat rewrote the receipt")
	}
	if n := in.nextNumber(t); n != "501" {
		t.Errorf("next number = %s; want 501", n)
	}
}

// Killed between reserving n and renaming its file into place: the index owns
// n, no file is in herp. After the restart the backend's retry is answered
// written:true with n, and herp holds exactly one KUPAIN_n.
func TestSendReceipt_RetryAfterAKillBetweenReserveAndRenameIsOneReceipt(t *testing.T) {
	for _, storeMoved := range []bool{false, true} {
		name := "store not yet moved"
		if storeMoved {
			name = "store moved on"
		}
		t.Run(name, func(t *testing.T) {
			in := newReceiptInstall(t, 900)
			entry, err := json.Marshal(map[string]any{
				"key": wireKey, "number": 900, "state": "reserved", "request": wireRequest(t),
				"files": nil, "import": "not_run", "reservedAt": "2026-10-16T09:00:00Z",
			})
			if err != nil {
				t.Fatal(err)
			}
			idx := filepath.Join(in.dataDir, "receipts", "index", wireKey+".json")
			if err := os.MkdirAll(filepath.Dir(idx), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(idx, entry, 0o600); err != nil {
				t.Fatal(err)
			}
			if storeMoved {
				store := hasavshevet.ReceiptNumberStorePath(in.dataDir)
				if err := os.WriteFile(store, []byte(`{"nextReceiptNumber": 901}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if docs := in.docs(t); len(docs) != 0 {
				t.Fatalf("herp holds %v before the restart", docs)
			}

			// The restart: a fresh writer. No explicit Recover — the first Write
			// must finish the interrupted receipt before anything else.
			_ = in.writer.Close() // the old process is gone, and its lock with it
			w, err := hasavshevet.NewReceiptWriter(hasavshevet.ReceiptWriterConfig{SendReceiptDir: in.sendDir, DataDir: in.dataDir}, &noopLogger{})
			if err != nil {
				t.Fatal(err)
			}
			h := NewSendReceiptHandler(w, true, &noopLogger{})
			for range 2 {
				rec := postReceipt(t, h, wireBytes(t))
				if rec.Code != http.StatusOK || receiptNumberOf(t, rec) != "900" {
					t.Fatalf("retry = %d %s; want 200 written 900", rec.Code, rec.Body.String())
				}
			}
			if docs := in.docs(t); len(docs) != 1 || filepath.Base(docs[0]) != "KUPAIN_900.doc" {
				t.Errorf("herp holds %v; want exactly KUPAIN_900.doc", docs)
			}
			if n := in.nextNumber(t); n != "901" {
				t.Errorf("next number = %s; want 901", n)
			}
		})
	}
}

// A number store that cannot be trusted is a 503 — retryable, never a 400 and
// never a restart at 1 — and the request is answered once it is repaired.
func TestSendReceipt_CorruptNumberStoreIs503(t *testing.T) {
	for name, content := range map[string]string{
		"truncated": `{"nextReceipt`,
		"not JSON":  "\x00\x00\x00",
		"zero":      `{"nextReceiptNumber": 0}`,
		"empty":     ``,
	} {
		t.Run(name, func(t *testing.T) {
			in := newReceiptInstall(t, 1000)
			store := hasavshevet.ReceiptNumberStorePath(in.dataDir)
			if err := os.WriteFile(store, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			rec := postReceipt(t, NewSendReceiptHandler(in.writer, true, &noopLogger{}), wireBytes(t))
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d %s; want 503", rec.Code, rec.Body.String())
			}
			if e := decodeError(t, rec); e.Code != "RECEIPT_NUMBER_STORE_UNAVAILABLE" {
				t.Errorf("code = %q; want RECEIPT_NUMBER_STORE_UNAVAILABLE", e.Code)
			}
			if docs := in.docs(t); len(docs) != 0 {
				t.Errorf("herp holds %v; want nothing", docs)
			}
			if got, _ := os.ReadFile(store); string(got) != content {
				t.Errorf("the store was rewritten to %q", got)
			}
		})
	}
}

// The import lock held (an order import running) past the wait: 503 busy,
// nothing reserved, and the retry gets the number the first try would have.
func TestSendReceipt_BusyImportLockIs503AndReservesNothing(t *testing.T) {
	in := newReceiptInstall(t, 77)
	h := NewSendReceiptHandler(in.writer, true, &noopLogger{})

	release, err := hasavshevet.HoldImports(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// receiptLockWait is 20 s; the caller's own deadline ends the wait sooner.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/sendReceipt", bytes.NewReader(wireBytes(t))).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	busy := httptest.NewRecorder()
	h.ServeHTTP(busy, req)
	release()

	if busy.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d %s; want 503", busy.Code, busy.Body.String())
	}
	if e := decodeError(t, busy); e.Code != "RECEIPT_WRITER_BUSY" {
		t.Errorf("code = %q; want RECEIPT_WRITER_BUSY", e.Code)
	}
	if n := in.nextNumber(t); n != "77" {
		t.Errorf("a busy answer moved the store to %s", n)
	}
	if idx, _ := filepath.Glob(filepath.Join(in.dataDir, "receipts", "index", "*.json")); len(idx) != 0 {
		t.Errorf("a busy answer reserved %v", idx)
	}

	retry := postReceipt(t, h, wireBytes(t))
	if retry.Code != http.StatusOK || receiptNumberOf(t, retry) != "77" {
		t.Fatalf("retry = %d %s; want 200 written 77", retry.Code, retry.Body.String())
	}
}
