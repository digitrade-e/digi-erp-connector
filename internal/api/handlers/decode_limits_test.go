package handlers

import (
	"bytes"
	"net/http"
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
