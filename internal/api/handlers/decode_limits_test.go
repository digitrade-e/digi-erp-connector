package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// deepBody nests n levels of open+close around a scalar, wrapped as prefix …
// suffix — about len(open)+len(close) bytes per level.
func deepBody(prefix, open, leaf, close, suffix string, n int) []byte {
	var b bytes.Buffer
	b.WriteString(prefix)
	b.WriteString(strings.Repeat(open, n))
	b.WriteString(leaf)
	b.WriteString(strings.Repeat(close, n))
	b.WriteString(suffix)
	return b.Bytes()
}

// allocatedBy reports the bytes allocated while f runs (TotalAlloc delta). A
// strict decode of a ~100 KB body has no business allocating more than a few
// MB; the key walk once allocated O(depth²) — 227 MB for 70 KB.
func allocatedBy(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// A deeply nested body — under a scalar key, or as the whole document — is a
// 400 at a bounded cost: one authenticated request must not be able to take
// the write node (and the order queue with it) down.
func TestSendReceipt_DeepNestingIsA400AtBoundedCost(t *testing.T) {
	const ceiling = 32 << 20 // 32 MiB
	// Past 10 000 levels encoding/json itself refuses the document ("body");
	// below that the value is simply the wrong type for its key.
	const deep, deeper = 9000, 15000 // ≈ 55 KB and 90 KB
	for _, tc := range []struct {
		name  string
		body  []byte
		field string
	}{
		{"objects under remarks", deepBody(`{"remarks":`, `{"a":`, `1`, `}`, `}`, deep), "remarks"},
		{"objects under remarks, past the decoder's cap", deepBody(`{"remarks":`, `{"a":`, `1`, `}`, `}`, deeper), "body"},
		{"arrays under remarks", deepBody(`{"remarks":`, `[`, `1`, `]`, `}`, deep), "remarks"},
		{"arrays as the document", deepBody(``, `[`, `1`, `]`, ``, deeper), "body"},
		{"objects under an unknown key", deepBody(`{"remarks":"x","zz":`, `{"a":`, `1`, `}`, `}`, deeper), "zz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := newReceiptInstall(t, 1000)
			h := NewSendReceiptHandler(in.writer, true, &noopLogger{})
			var code int
			var e errorBody
			used := allocatedBy(func() {
				rec := postReceipt(t, h, tc.body)
				code = rec.Code
				e = decodeError(t, rec)
			})
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400", code)
			}
			if got, _ := e.Details["field"].(string); got != tc.field {
				t.Errorf("details.field = %q; want %q (%s)", got, tc.field, e.Error)
			}
			if used > ceiling {
				t.Errorf("a %d-byte body allocated %d MB; want under %d MB", len(tc.body), used>>20, ceiling>>20)
			}
		})
	}
}

// The walk never descends past a small depth even for types that nest: a
// receipt is flat, so anything deeper than walkMaxDepth is refused outright.
func TestCheckJSONKeys_RefusesNestingPastTheBound(t *testing.T) {
	type node struct {
		Next *node `json:"next"`
	}
	ok := deepBody(``, `{"next":`, `null`, `}`, ``, walkMaxDepth)
	if err := checkJSONKeys(ok, reflect.TypeFor[*node]()); err != nil {
		t.Fatalf("%d levels refused: %v", walkMaxDepth, err)
	}
	deep := deepBody(``, `{"next":`, `null`, `}`, ``, walkMaxDepth+1)
	if err := checkJSONKeys(deep, reflect.TypeFor[*node]()); err == nil {
		t.Fatalf("%d levels accepted", walkMaxDepth+1)
	}
}

// failingBody yields half a document, then fails as a dropped connection does.
type failingBody struct {
	data []byte
	err  error
}

func (f *failingBody) Read(p []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, f.err
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}

// A read that fails for transport reasons is a 503 (the backend retries), never
// a 400 (the backend stops the receipt). Only the size cap stays a 400.
func TestSendReceipt_BodyReadFailureIs503(t *testing.T) {
	for name, readErr := range map[string]error{
		"unexpected EOF": io.ErrUnexpectedEOF,
		"timeout":        os.ErrDeadlineExceeded,
		"reset":          errors.New("connection reset by peer"),
	} {
		t.Run(name, func(t *testing.T) {
			in := newReceiptInstall(t, 1000)
			b := wireBytes(t)
			req := httptest.NewRequest(http.MethodPost, "/api/sendReceipt", &failingBody{data: b[:len(b)/2], err: readErr})
			rec := httptest.NewRecorder()
			NewSendReceiptHandler(in.writer, true, &noopLogger{}).ServeHTTP(rec, req)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d %s; want 503", rec.Code, rec.Body.String())
			}
			if e := decodeError(t, rec); e.Code != "RECEIPT_REQUEST_READ_FAILED" {
				t.Errorf("code = %q; want RECEIPT_REQUEST_READ_FAILED", e.Code)
			}
			if n := in.nextNumber(t); n != "1000" {
				t.Errorf("store moved to %s", n)
			}
		})
	}
}

// A 400 names the field and why — never the value. Card digits, dates, keys
// and names go to logs and to the caller's error records; the field is enough.
func TestSendReceipt_400MessagesNeverEchoTheValue(t *testing.T) {
	for _, tc := range []struct {
		key    string
		value  any
		secret string
	}{
		{"last4", "4580123412341234", "4580123412341234"},
		{"last4", "45x0", "45x0"},
		{"settledDate", "14/10/2026", "14/10/2026"},
		{"settledDate", "2026-02-30", "2026-02-30"},
		{"cardIssuer", "987", "987"},
		{"cardBrand", "Vi", "Vi"},
		{"idempotencyKey", "secret-not-a-uuid", "secret-not-a-uuid"},
		{"amountMinor", json.Number("123456789"), "123456789"},
		{"amountMinor", json.Number("6932.5"), "6932.5"},
		{"amountMinor", "693200", "693200"},
		{"installments", json.Number("77"), "77"},
		{"name", "شركة", "شركة"},
		{"name", "בל\"פ 🙂", "🙂"},
		{"last4", json.Number("4242"), "4242"},
	} {
		t.Run(tc.key+"="+tc.secret, func(t *testing.T) {
			in := newReceiptInstall(t, 1000)
			m := wireMap(t)
			m[tc.key] = tc.value
			rec := postReceipt(t, NewSendReceiptHandler(in.writer, true, &noopLogger{}), mustJSON(t, m))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d %s; want 400", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), tc.secret) || strings.Contains(decodeError(t, rec).Error, tc.secret) {
				t.Errorf("the 400 echoes the value %q: %s", tc.secret, rec.Body.String())
			}
		})
	}
}

// An unknown or duplicate key is named in the 400, but never more than 64
// bytes of it, cut on a rune boundary and marked "…": a 1 MiB key name once
// made a 2 MiB error body (message + details.field).
func TestSendReceipt_AnEchoedKeyIsCappedAt64Bytes(t *testing.T) {
	huge := strings.Repeat("k", (1<<20)-64)
	hebrew := strings.Repeat("ש", 40) // 80 bytes of UTF-8
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"1 MiB unknown key", `{"` + huge + `":1}`, strings.Repeat("k", 64) + "…"},
		{"duplicate long key", `{"` + strings.Repeat("d", 100) + `":1,"` + strings.Repeat("d", 100) + `":2}`, strings.Repeat("d", 64) + "…"},
		{"Hebrew key cut between letters", `{"` + hebrew + `":1}`, strings.Repeat("ש", 32) + "…"},
		{"byte 64 inside a letter", `{"a` + hebrew + `":1}`, "a" + strings.Repeat("ש", 31) + "…"},
		{"short key kept whole", `{"surprise":1}`, "surprise"},
		{"exactly 64 bytes kept whole", `{"` + strings.Repeat("x", 64) + `":1}`, strings.Repeat("x", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := newReceiptInstall(t, 1000)
			rec := postReceipt(t, NewSendReceiptHandler(in.writer, true, &noopLogger{}), []byte(tc.body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400", rec.Code)
			}
			if rec.Body.Len() > 1024 {
				t.Errorf("the 400 body is %d bytes", rec.Body.Len())
			}
			e := decodeError(t, rec)
			if got, _ := e.Details["field"].(string); got != tc.want {
				t.Errorf("details.field = %q; want %q", got, tc.want)
			}
			if !strings.HasPrefix(e.Error, tc.want) {
				t.Errorf("message %q does not start with the capped key", e.Error)
			}
		})
	}
}
