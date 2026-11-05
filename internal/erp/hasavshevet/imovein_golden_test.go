package hasavshevet

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// The goldens in testdata/imovein were written by this test running on main
// (21e2455, before DIG-1295) with IMOVEIN_GOLDEN_UPDATE=1. They pin the bytes a
// request WITHOUT the document fields produces, so that adding the fields
// changes nothing for every tenant that does not send them. Regenerate only for
// a byte change that is meant, and say so in the commit.
var updateIMOVEINGoldens = os.Getenv("IMOVEIN_GOLDEN_UPDATE") == "1"

type imoveinGoldenCase struct {
	name   string
	number int64
	req    OrderRequest
}

func imoveinGoldenCases() []imoveinGoldenCase {
	account := &AccountDetails{
		AccountKey: "40217", FullName: "לקוח 'בדיקה' בע\"מ", Address: "הרצל 1\nקומה 2",
		City: "חיפה", Phone: "04-1234567", Agent: "7", HProtect: "514000000",
	}
	base := OrderRequest{
		DocumentType: "ORDER", UserExtID: "40217", DueDate: "2026-11-10", CreatedDate: "2026-11-05T09:30:00Z",
		Comment: "הערה ללקוח\nשורה שנייה", Discount: 5, HistoryID: "4711-3f2c9a1e-7b4d-4c8e-9a51-2d6f0b8e4c17",
		Total: 150, Currency: `ש"ח`, Account: account,
		Details: []OrderLineItem{
			{Title: "מוצר א'", SKU: "1001", Quantity: 2, Packs: 1, OriginalPrice: 75, SinglePrice: 70, TotalPrice: 140, Discount: 6.5},
			{Title: "Widget B", SKU: "SKU-2002", Quantity: 1.5, OriginalPrice: 10, SinglePrice: 10, TotalPrice: 15},
		},
	}
	usd := base
	usd.Currency = "USD"
	quote := base
	quote.DocumentType = "QUOATE"
	ret := base
	ret.DocumentType = "RETURN"
	ret.Details = []OrderLineItem{{Title: "החזרה", SKU: "1001", Quantity: -1, OriginalPrice: 75, SinglePrice: 75, TotalPrice: -75}}
	odd := base
	odd.Discount = 150                           // written as 99.99
	odd.Comment = strings.Repeat("א", 300)       // cut to 250 runes
	odd.CreatedDate = "2026-11-05"               // date-only form
	odd.Account = &AccountDetails{FullName: "x"} // account key falls back to userExtId
	return []imoveinGoldenCase{
		{"order-ils", 1000295, base},
		{"order-usd", 1000296, usd},
		{"quote", 1000297, quote},
		{"return", 1000298, ret},
		{"order-odd", 1000299, odd},
	}
}

// renderIMOVEIN runs the real send-order flow (no database: the account is in
// the request, so the rate is 1.0) and returns the IMOVEIN pair it wrote.
func renderIMOVEIN(t *testing.T, req OrderRequest, n int64) (doc, prm []byte) {
	t.Helper()
	dir := t.TempDir()
	s := NewSender(nil, config.Config{SendOrderDir: dir}, NewOrderNumberStore(filepath.Join(dir, "lastOrderNumber.json")), noopLogger{})
	if _, err := s.processOrderWithNumber(context.Background(), req, n); err != nil {
		t.Fatalf("processOrderWithNumber: %v", err)
	}
	doc = readFile(t, filepath.Join(dir, "IMOVEIN.doc"))
	prm = readFile(t, filepath.Join(dir, "IMOVEIN.prm"))
	if hist := readFile(t, filepath.Join(dir, "history", strconv.FormatInt(n, 10), "IMOVEIN_"+strconv.FormatInt(n, 10)+".doc")); !bytes.Equal(hist, doc) {
		t.Fatal("the history copy differs from IMOVEIN.doc")
	}
	return doc, prm
}

func goldenPath(name string) string { return filepath.Join("testdata", "imovein", name) }

// A request without the document fields produces exactly main's bytes.
func TestIMOVEIN_GoldenWithoutDocumentFields(t *testing.T) {
	for _, tc := range imoveinGoldenCases() {
		t.Run(tc.name, func(t *testing.T) {
			doc, prm := renderIMOVEIN(t, tc.req, tc.number)
			if updateIMOVEINGoldens {
				writeGolden(t, tc.name+".doc", doc)
				writeGolden(t, "IMOVEIN.prm", prm)
				return
			}
			if want := readFile(t, goldenPath(tc.name+".doc")); !bytes.Equal(doc, want) {
				t.Errorf("IMOVEIN.doc for %s differs from main's golden (%d vs %d bytes)%s",
					tc.name, len(doc), len(want), firstDiff(doc, want))
			}
			if want := readFile(t, goldenPath("IMOVEIN.prm")); !bytes.Equal(prm, want) {
				t.Errorf("IMOVEIN.prm differs from main's golden%s", firstDiff(prm, want))
			}
		})
	}
}

// DIG-1291 stores each order's request hash and warns when a repeat's body
// differs. A request without the document fields must hash as it did on
// main, or every replay across the upgrade would be reported as changed.
func TestOrderRequestHash_UnchangedWithoutDocumentFields(t *testing.T) {
	got := orderRequestHash(imoveinGoldenCases()[0].req)
	if updateIMOVEINGoldens {
		writeGolden(t, "request-sha256.txt", []byte(got+"\n"))
		return
	}
	if want := strings.TrimSpace(string(readFile(t, goldenPath("request-sha256.txt")))); got != want {
		t.Errorf("orderRequestHash = %s; main's was %s", got, want)
	}
}

func writeGolden(t *testing.T, name string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(goldenPath(""), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(goldenPath(name), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// firstDiff names the first differing byte and the IMOVEIN field it is in.
func firstDiff(got, want []byte) string {
	n := len(got)
	if len(want) < n {
		n = len(want)
	}
	for i := 0; i < n; i++ {
		if got[i] != want[i] {
			return "; first difference at byte " + strconv.Itoa(i) + " (" + fieldAt(i%(prmTotalRecordLength+1)) + ")"
		}
	}
	return ""
}

// fieldAt names the IMOVEIN field holding byte offset off of a record.
func fieldAt(off int) string {
	pos := 0
	for i := 2; i <= 87; i++ {
		key := "line" + strconv.Itoa(i)
		w := imoveInFieldLengths[key]
		if off < pos+w {
			return key
		}
		pos += w
	}
	return "end of record"
}
