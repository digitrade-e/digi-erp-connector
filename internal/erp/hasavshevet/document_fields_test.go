package hasavshevet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// imoveinSpan is the byte range of an IMOVEIN field within one record.
func imoveinSpan(line string) (start, end int) {
	pos := 0
	for i := 2; i <= 87; i++ {
		key := "line" + strconv.Itoa(i)
		w := imoveInFieldLengths[key]
		if key == line {
			return pos, pos + w
		}
		pos += w
	}
	panic("no field " + line)
}

func TestValidateDocumentFields(t *testing.T) {
	valid := []OrderRequest{
		{},
		{DocumentID: 1}, {DocumentID: 31}, {DocumentID: 99},
		{Warehouse: 1}, {Warehouse: 81}, {Warehouse: 999999999},
		{VatPercent: "0.00"}, {VatPercent: "17.00"}, {VatPercent: "18.50"}, {VatPercent: "99.99"},
		{Asmahta2: "0"}, {Asmahta2: "26004295"}, {Asmahta2: "123456789"}, {Asmahta2: "000000001"},
		{DocumentID: 31, Warehouse: 81, VatPercent: "17.00", Asmahta2: "26004295"},
	}
	for _, req := range valid {
		if err := ValidateDocumentFields(req); err != nil {
			t.Errorf("ValidateDocumentFields(%+v) = %v; want valid", req, err)
		}
	}
	for _, tc := range []struct {
		req   OrderRequest
		field string
	}{
		{OrderRequest{DocumentID: -1}, "documentId"},
		{OrderRequest{DocumentID: 100}, "documentId"},
		{OrderRequest{Warehouse: -1}, "warehouse"},
		{OrderRequest{Warehouse: 1000000000}, "warehouse"},
		{OrderRequest{VatPercent: "100.00"}, "vatPercent"},
		{OrderRequest{VatPercent: "18.5"}, "vatPercent"},
		{OrderRequest{VatPercent: "18"}, "vatPercent"},
		{OrderRequest{VatPercent: "1e1"}, "vatPercent"},
		{OrderRequest{VatPercent: "-1.00"}, "vatPercent"},
		{OrderRequest{VatPercent: " 7.00"}, "vatPercent"},
		{OrderRequest{Asmahta2: "1234567890"}, "asmahta2"},
		{OrderRequest{Asmahta2: "12a"}, "asmahta2"},
		{OrderRequest{Asmahta2: "-1"}, "asmahta2"},
		{OrderRequest{Asmahta2: " 12"}, "asmahta2"},
		{OrderRequest{Asmahta2: "١٢"}, "asmahta2"}, // Arabic-Indic digits are not ASCII digits
	} {
		err := ValidateDocumentFields(tc.req)
		var fe *FieldError
		if !errors.As(err, &fe) || fe.Field != tc.field {
			t.Errorf("ValidateDocumentFields(%+v) = %v; want a FieldError on %s", tc.req, err, tc.field)
		}
	}
}

// vatPercent arrives as the caller's number text; it is written as IMOVEIN's
// "18.00" form, and anything that would need rounding or more than the field's
// five characters is refused, never cut.
func TestFormatVatPercent(t *testing.T) {
	for in, want := range map[string]string{
		"17": "17.00", "18.5": "18.50", "18.50": "18.50", "0": "0.00", "0.5": "0.50",
		"7": "7.00", "99.99": "99.99", "017": "17.00", "18.0": "18.00",
	} {
		got, err := FormatVatPercent(in)
		if err != nil || got != want {
			t.Errorf("FormatVatPercent(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"18.555", "18.501", "9.125", "0.505", "100", "100.0", "abc", "", "1e1", "-5", "18.", ".5", "18,5", " 18", "18 ", "+18", "١٨"} {
		if got, err := FormatVatPercent(in); err == nil {
			t.Errorf("FormatVatPercent(%q) = %q; want a refusal", in, got)
		} else {
			var fe *FieldError
			if !errors.As(err, &fe) || fe.Field != "vatPercent" {
				t.Errorf("FormatVatPercent(%q) error = %v; want a FieldError on vatPercent", in, err)
			}
		}
	}
}

// Each document field, when given, changes exactly its own IMOVEIN field on
// every row and nothing else; the rest of the record stays main's bytes.
func TestIMOVEIN_DocumentFieldsLandInTheirFields(t *testing.T) {
	golden := imoveinGoldenCases()[0] // order-ils: DocumentID 30, warehouse 1, VAT 18.00, Asmahta2 = number
	want := readFile(t, goldenPath(golden.name+".doc"))
	for _, tc := range []struct {
		name   string
		set    func(*OrderRequest)
		fields map[string]string // field → value it must hold
	}{
		{"documentId", func(r *OrderRequest) { r.DocumentID = 31 }, map[string]string{"line4": "31"}},
		{"warehouse", func(r *OrderRequest) { r.Warehouse = 81 }, map[string]string{"line12": "81"}},
		{"vatPercent", func(r *OrderRequest) { r.VatPercent = "17.00" }, map[string]string{"line18": "17.00"}},
		{"asmahta2", func(r *OrderRequest) { r.Asmahta2 = "26004295" }, map[string]string{"line8": "26004295"}},
		{"all four", func(r *OrderRequest) {
			r.DocumentID, r.Warehouse, r.VatPercent, r.Asmahta2 = 31, 81, "17.00", "26004295"
		}, map[string]string{"line4": "31", "line12": "81", "line18": "17.00", "line8": "26004295"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := golden.req
			tc.set(&req)
			got, _ := renderIMOVEIN(t, req, golden.number)
			if len(got) != len(want) {
				t.Fatalf("%d bytes; main's golden has %d", len(got), len(want))
			}
			rowSize := prmTotalRecordLength + 1
			inChanged := func(off int) bool {
				for line := range tc.fields {
					s, e := imoveinSpan(line)
					if off >= s && off < e {
						return true
					}
				}
				return false
			}
			for i := range got {
				if got[i] != want[i] && !inChanged(i%rowSize) {
					t.Fatalf("byte %d (%s) differs from main's golden; only %v may", i, fieldAt(i%rowSize), tc.fields)
				}
			}
			for row := 0; row < len(got)/rowSize; row++ {
				rec := got[row*rowSize : (row+1)*rowSize]
				for line, value := range tc.fields {
					s, e := imoveinSpan(line)
					if field := string(rec[s:e]); field != string(padW1255(value, e-s)) {
						t.Errorf("row %d %s = %q; want %q", row, line, field, value)
					}
				}
				// MyID stays the connector's number whatever asmahta2 says.
				s, e := imoveinSpan("line3")
				if field := string(rec[s:e]); field != string(padW1255(strconv.FormatInt(golden.number, 10), e-s)) {
					t.Errorf("row %d line3 (MyID) = %q; want the order number", row, field)
				}
			}
		})
	}
}

// documentId replaces the built-in type rule for every document type,
// including ORDER's currency switch (30 / 32).
func TestIMOVEIN_DocumentIDOverridesTheTypeRule(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   int
	}{{"order-usd", 31}, {"quote", 41}, {"return", 75}} {
		var c imoveinGoldenCase
		for _, g := range imoveinGoldenCases() {
			if g.name == tc.name {
				c = g
			}
		}
		c.req.DocumentID = tc.id
		doc, _ := renderIMOVEIN(t, c.req, c.number)
		s, e := imoveinSpan("line4")
		if got := string(doc[s:e]); got != strconv.Itoa(tc.id) {
			t.Errorf("%s line4 = %q; want %d", tc.name, got, tc.id)
		}
	}
}

// The Sender checks the fields itself too (a caller other than the route): a
// bad value fails before anything is written, as a FieldError.
func TestSender_RefusesABadDocumentField(t *testing.T) {
	for _, set := range []func(*OrderRequest){
		func(r *OrderRequest) { r.DocumentID = 100 },
		func(r *OrderRequest) { r.Warehouse = -2 },
		func(r *OrderRequest) { r.VatPercent = "18.5" },
		func(r *OrderRequest) { r.Asmahta2 = "1234567890" },
	} {
		req := imoveinGoldenCases()[0].req
		set(&req)
		dir := t.TempDir()
		s := NewSender(nil, config.Config{SendOrderDir: dir}, NewOrderNumberStore(filepath.Join(dir, "lastOrderNumber.json")), noopLogger{})
		_, err := s.processOrderWithNumber(context.Background(), req, 7)
		var fe *FieldError
		if !errors.As(err, &fe) || !errors.Is(err, ErrOrderNotWritten) {
			t.Errorf("processOrderWithNumber(%+v) = %v; want a FieldError, not written", req, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "IMOVEIN.doc")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("IMOVEIN.doc written for a refused request: %v", err)
		}
	}
}

// A repeat of a historyId answers the first order and writes nothing, even if
// it now carries different document fields: the first request's document is
// the one written.
func TestOrderQueue_ReplayKeepsTheFirstDocumentFields(t *testing.T) {
	h := newOrderHarness(t)
	q := h.queue(true)
	first := keyedOrder("rel-a")
	first.DocumentID, first.Warehouse = 31, 81
	waitForStatus(t, q, mustAccept(t, q, first).JobID, JobStatusDone)
	doc := readFile(t, h.historyDoc(1))

	again := keyedOrder("rel-a")
	again.DocumentID, again.Warehouse, again.VatPercent = 40, 8, "0.00"
	acc := mustAccept(t, q, again)
	if acc.JobID != "1" || acc.Status != JobStatusDone || !acc.Replay {
		t.Fatalf("repeat = %+v; want job 1 done, replay", acc)
	}
	time.Sleep(50 * time.Millisecond)
	if got := readFile(t, h.historyDoc(1)); string(got) != string(doc) {
		t.Error("a repeat rewrote order 1")
	}
	s, e := imoveinSpan("line4")
	if got := string(doc[s:e]); got != "31" {
		t.Errorf("order 1 line4 = %q; want the first request's 31", got)
	}
	if !h.log.warned("different body") {
		t.Error("a repeat with different document fields was not logged")
	}
	if n := h.counter(); n != 1 {
		t.Errorf("counter = %d; want 1", n)
	}
}

// Setting a document field changes the request's fingerprint (so a repeat
// that changes it is reported), while leaving them all out keeps main's
// (TestOrderRequestHash_UnchangedWithoutDocumentFields).
func TestOrderRequestHash_SeesTheDocumentFields(t *testing.T) {
	base := imoveinGoldenCases()[0].req
	for _, set := range []func(*OrderRequest){
		func(r *OrderRequest) { r.DocumentID = 31 },
		func(r *OrderRequest) { r.Warehouse = 81 },
		func(r *OrderRequest) { r.VatPercent = "17.00" },
		func(r *OrderRequest) { r.Asmahta2 = "26004295" },
	} {
		req := base
		set(&req)
		if orderRequestHash(req) == orderRequestHash(base) {
			t.Errorf("hash of %+v equals the request without it", req)
		}
	}
}
