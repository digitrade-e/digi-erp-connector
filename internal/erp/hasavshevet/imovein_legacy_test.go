package hasavshevet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// The goldens in testdata/imovein-legacy were written by the old app's own
// writer (generate.php beside them runs its functions unchanged on cases.json;
// source.txt pins the file it ran). Each case below is the request that must
// produce the same order: the test first checks that the request builds the
// header and lines the fixture fed the legacy writer, then that the file
// bytes are the legacy's.

func legacyGoldenPath(name string) string { return filepath.Join("testdata", "imovein-legacy", name) }

type legacyCase struct {
	Name  string           `json:"name"`
	Stock map[string]any   `json:"stock"`
	Moves []map[string]any `json:"moves"`
}

func readLegacyCases(t *testing.T) map[string]legacyCase {
	t.Helper()
	var cases []legacyCase
	if err := json.Unmarshal(readFile(t, legacyGoldenPath("cases.json")), &cases); err != nil {
		t.Fatalf("cases.json: %v", err)
	}
	out := make(map[string]legacyCase, len(cases))
	for _, c := range cases {
		out[c.Name] = c
	}
	return out
}

// legacyNames is names.txt: the file name the legacy gave each case.
func legacyNames(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(readFile(t, legacyGoldenPath("names.txt")))), "\n") {
		name, file, ok := strings.Cut(line, " ")
		if !ok {
			t.Fatalf("names.txt: bad line %q", line)
		}
		out[name] = file
	}
	return out
}

type legacyRequestCase struct {
	name   string
	number int64
	req    OrderRequest
}

func legacyRequestCases() []legacyRequestCase {
	account := &AccountDetails{
		AccountKey: "40217", FullName: "לקוח 'בדיקה' בע\"מ", Address: "הרצל 1\nקומה 2",
		City: "חיפה", Phone: "04-1234567", Agent: "7", HProtect: "514000000",
	}
	order := OrderRequest{
		DocumentType: "ORDER", UserExtID: "40217", DueDate: "2026-11-10", CreatedDate: "2026-11-05T09:30:00Z",
		Comment: "הערה ללקוח\nשורה שנייה|הערת מנהל", Discount: 5, HistoryID: "4711-0b6c", Total: 165,
		Currency: `ש"ח`, Account: account, DocumentID: 31, Warehouse: 81, VatPercent: "18.00", Asmahta2: "4711",
		Details: []OrderLineItem{
			{Title: "מוצר א'", SKU: "1001", Quantity: 2, Packs: 1, OriginalPrice: 75, SinglePrice: 70.13, TotalPrice: 140.25, Discount: 6.5},
			{Title: "Widget B – 5₪", SKU: "SKU-2002", Quantity: 1.5, OriginalPrice: 10, SinglePrice: 10, TotalPrice: 15},
		},
	}
	quote := order
	quote.DocumentType, quote.DocumentID, quote.Warehouse, quote.Asmahta2 = "QUOATE", 40, 8, "4712"
	quote.Discount, quote.Comment = 0, ""
	quote.Details = []OrderLineItem{{Title: "מוצר א'", SKU: "1001", Quantity: 10, OriginalPrice: 70, SinglePrice: 70, TotalPrice: 700}}
	ret := order
	ret.DocumentType, ret.DocumentID, ret.Warehouse, ret.Asmahta2 = "RETURN", 74, 8, "4713"
	ret.Discount, ret.Comment = 0, "החזרה"
	ret.Details = []OrderLineItem{{Title: "החזרה", SKU: "1001", Quantity: -1, OriginalPrice: 75, SinglePrice: 75, TotalPrice: -75}}
	overflow := order
	overflow.Warehouse, overflow.Asmahta2, overflow.Discount = 999999999, "123456789", 150
	overflow.Comment = strings.Repeat("ב", 300)
	overflow.Account = &AccountDetails{
		AccountKey: "40217", FullName: strings.Repeat("א", 60), Address: strings.Repeat("רחוב ארוך מאוד ", 4),
		City: "קרית ביאליק", Phone: "+972-4-1234567 / 050-1234567 שלוחה 12", Agent: "123456789",
	}
	overflow.Details = []OrderLineItem{{
		Title: strings.Repeat("שם פריט ארוך ", 10), SKU: "ABCDEFGHIJ0123456789", Quantity: 123456,
		OriginalPrice: 99999.99, SinglePrice: 99999.99, TotalPrice: 1, Discount: 100,
	}}
	return []legacyRequestCase{
		{"order", 100001, order},
		{"quote", 100002, quote},
		{"return", 100003, ret},
		{"overflow", 999999999, overflow},
	}
}

// legacyText renders a fixture value the way PHP's string conversion does.
func legacyText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return fmt.Sprint(x)
	}
}

// TestLegacyRequestsBuildTheFixtures pins the mapping: each request builds,
// field for field, the header and lines its fixture handed the legacy writer.
// A failure here names the field; the byte test below would only say "differs".
func TestLegacyRequestsBuildTheFixtures(t *testing.T) {
	fixtures := readLegacyCases(t)
	for _, tc := range legacyRequestCases() {
		t.Run(tc.name, func(t *testing.T) {
			fx, ok := fixtures[tc.name]
			if !ok {
				t.Fatalf("no fixture %q in cases.json", tc.name)
			}
			s := NewSender(nil, config.Config{}, nil, noopLogger{})
			account, err := s.resolveAccount(context.Background(), tc.req)
			if err != nil {
				t.Fatal(err)
			}
			h, moves := buildIMOVEIN(tc.number, account, tc.req, 1)
			for _, f := range legacyFields {
				if f.width == 0 {
					continue
				}
				for i, m := range moves {
					got := legacyFieldValue(f.line, h, m)
					want := legacyFixtureValue(f.line, fx, i)
					if got != want {
						t.Errorf("line%d (row %d) = %q; the legacy writer was given %q", f.line, i, got, want)
					}
				}
			}
			if len(moves) != len(fx.Moves) {
				t.Errorf("%d lines; the fixture has %d", len(moves), len(fx.Moves))
			}
		})
	}
}

// legacyFixtureValue is what prepareForHerpDoc reads for a written line.
func legacyFixtureValue(line int, fx legacyCase, row int) string {
	s, m := fx.Stock, fx.Moves[row]
	switch line {
	case 2:
		return legacyText(s["AccountKey"])
	case 3:
		return legacyText(s["MyId"])
	case 4:
		return legacyText(s["DocumentID"])
	case 5:
		return legacyText(s["AccountName"])
	case 6:
		return legacyText(s["Address"])
	case 7:
		return legacyText(s["City"])
	case 8:
		return legacyText(s["Asmahta2"])
	case 9, 10, 36:
		return legacyText(s["ShortDate"])
	case 11:
		return legacyText(s["Agent"])
	case 12:
		return legacyText(s["WareHouse"])
	case 13, 58, 59:
		return ""
	case 17:
		return legacyText(s["DiscountPrcR"])
	case 18:
		return legacyText(s["VatPrc"])
	case 19:
		return legacyText(s["Copies"])
	case 22:
		return legacyText(m["ItemKey"])
	case 23:
		return legacyText(m["Quantity"])
	case 24:
		return legacyText(m["Price"])
	case 26:
		return legacyText(m["DiscountPrc"])
	case 28:
		return legacyText(m["ItemName"])
	case 29:
		return legacyText(m["Unit"])
	case 34:
		return "0"
	case 35:
		return legacyText(s["Phone"])
	case 37:
		return legacyText(s["Remarks"])
	case 45, 46, 47, 48, 49:
		return legacyText(s[fmt.Sprintf("ExtraText%d", line-44)])
	case 50, 51, 52:
		return legacyText(s[fmt.Sprintf("ExtraSum%d", line-49)])
	case 81, 82:
		return legacyText(s[fmt.Sprintf("ExtraNum%d", line-80)])
	}
	return "unexpected line " + strconv.Itoa(line)
}

// TestLegacyIMOVEIN_IsTheLegacyWritersBytes: with sendOrderFormat legacy, the
// importer model writes the old app's layout, byte for byte, and the PRM that
// describes it.
func TestLegacyIMOVEIN_IsTheLegacyWritersBytes(t *testing.T) {
	prmGolden := readFile(t, legacyGoldenPath("IMOVEIN.prm"))
	for _, tc := range legacyRequestCases() {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := config.Config{ERP: config.ERPHasavshevet, SendOrderDir: dir, SendOrderFormat: config.OrderFormatLegacy}
			s := NewSender(nil, cfg, NewOrderNumberStore(filepath.Join(dir, "lastOrderNumber.json")), noopLogger{})
			if _, err := s.processOrderWithNumber(context.Background(), tc.req, tc.number); err != nil {
				t.Fatalf("processOrderWithNumber: %v", err)
			}
			want := readFile(t, legacyGoldenPath(tc.name+".doc"))
			if got := readFile(t, filepath.Join(dir, "IMOVEIN.doc")); !bytes.Equal(got, want) {
				t.Errorf("IMOVEIN.doc differs from the legacy writer's %s.doc\n got: %q\nwant: %q", tc.name, got, want)
			}
			if got := readFile(t, filepath.Join(dir, "IMOVEIN.prm")); !bytes.Equal(got, prmGolden) {
				t.Errorf("IMOVEIN.prm differs from the legacy writer's\n got: %q\nwant: %q", got, prmGolden)
			}
		})
	}
}

// Every legacy line is the written fields, each cut and padded to its width
// plus one separator space, then LF: 1296 bytes + LF, which is what the PRM's
// first line says (1297, the next start).
func TestLegacyIMOVEIN_RecordLength(t *testing.T) {
	total := 0
	for _, f := range legacyFields {
		if f.width > 0 {
			total += f.width + 1
		}
	}
	if total != legacyRecordLength {
		t.Fatalf("written fields add up to %d bytes; legacyRecordLength = %d", total, legacyRecordLength)
	}
	if first, _, _ := strings.Cut(string(readFile(t, legacyGoldenPath("IMOVEIN.prm"))), " "); first != strconv.Itoa(legacyRecordLength+1) {
		t.Fatalf("the legacy PRM's first line starts %q; want %d", first, legacyRecordLength+1)
	}
}

// The official layout is untouched by the legacy one: no format set and
// "official" both write the goldens of testdata/imovein.
func TestOfficialFormat_ExplicitIsTheDefault(t *testing.T) {
	for _, tc := range imoveinGoldenCases() {
		dir := t.TempDir()
		cfg := config.Config{ERP: config.ERPHasavshevet, SendOrderDir: dir, SendOrderFormat: config.OrderFormatOfficial}
		s := NewSender(nil, cfg, NewOrderNumberStore(filepath.Join(dir, "lastOrderNumber.json")), noopLogger{})
		if _, err := s.processOrderWithNumber(context.Background(), tc.req, tc.number); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got, want := readFile(t, filepath.Join(dir, "IMOVEIN.doc")), readFile(t, goldenPath(tc.name+".doc")); !bytes.Equal(got, want) {
			t.Errorf("%s: sendOrderFormat official changed the bytes", tc.name)
		}
		if got, want := readFile(t, filepath.Join(dir, "IMOVEIN.prm")), readFile(t, goldenPath("IMOVEIN.prm")); !bytes.Equal(got, want) {
			t.Errorf("%s: sendOrderFormat official changed the PRM", tc.name)
		}
	}
}

func TestValidateLegacyFit(t *testing.T) {
	base := legacyRequestCases()[0].req
	for _, tc := range []struct {
		name      string
		edit      func(r *OrderRequest)
		wantField string // "" = fits
	}{
		{"the cases fit", func(r *OrderRequest) {}, ""},
		{"VAT 18.00 is cut to 18.0, same rate", func(r *OrderRequest) { r.VatPercent = "18.00" }, ""},
		{"VAT 17.50 is cut to 17.5, same rate", func(r *OrderRequest) { r.VatPercent = "17.50" }, ""},
		{"VAT 10.25 would be cut to 10.2", func(r *OrderRequest) { r.VatPercent = "10.25" }, "vatPercent"},
		{"the default VAT fits", func(r *OrderRequest) { r.VatPercent = "" }, ""},
		{"quantity 1234567 → 1234567.0", func(r *OrderRequest) { r.Details[0].Quantity = 1234567 }, ""},
		{"quantity 1234567.5 → 1234567.5 (a zero cut)", func(r *OrderRequest) { r.Details[0].Quantity = 1234567.5 }, ""},
		{"quantity 1234567.55 would lose a digit", func(r *OrderRequest) { r.Details[0].Quantity = 1234567.55 }, "details[0].quantity"},
		{"quantity 123456789 has no room", func(r *OrderRequest) { r.Details[0].Quantity = 123456789 }, "details[0].quantity"},
		{"price 999999.99 fits nine", func(r *OrderRequest) { r.Details[1].OriginalPrice = 999999.99 }, ""},
		{"price 9999999.99 would lose a cent", func(r *OrderRequest) { r.Details[1].OriginalPrice = 9999999.99 }, "details[1].originalPrice"},
		{"price 9999999.90 → 9999999.9", func(r *OrderRequest) { r.Details[1].OriginalPrice = 9999999.9 }, ""},
		{"a negative quantity fits", func(r *OrderRequest) { r.Details[0].Quantity = -12345.5 }, ""},
		{"a negative quantity too long", func(r *OrderRequest) { r.Details[0].Quantity = -1234567.25 }, "details[0].quantity"},
		{"line discount 100 fits six", func(r *OrderRequest) { r.Details[0].Discount = 100 }, ""},
		{"line discount 1000.55 does not", func(r *OrderRequest) { r.Details[0].Discount = 1000.55 }, "details[0].discount"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			r.Details = append([]OrderLineItem(nil), base.Details...)
			tc.edit(&r)
			err := ValidateLegacyFit(r)
			if tc.wantField == "" {
				if err != nil {
					t.Fatalf("ValidateLegacyFit = %v; want nil", err)
				}
				return
			}
			fe, ok := err.(*FieldError)
			if !ok || fe.Field != tc.wantField {
				t.Fatalf("ValidateLegacyFit = %v; want a FieldError on %s", err, tc.wantField)
			}
		})
	}
}

// A value the legacy widths would change is refused by the writer too (a
// caller that skipped the route), before anything is written: retryable.
func TestLegacyFormat_RefusesAValueItWouldCut(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{ERP: config.ERPHasavshevet, SendOrderDir: dir, SendOrderFormat: config.OrderFormatLegacy}
	s := NewSender(nil, cfg, NewOrderNumberStore(filepath.Join(dir, "lastOrderNumber.json")), noopLogger{})
	req := legacyRequestCases()[0].req
	req.VatPercent = "10.25"
	_, err := s.processOrderWithNumber(context.Background(), req, 7)
	if err == nil || !strings.Contains(err.Error(), "vatPercent") {
		t.Fatalf("processOrderWithNumber = %v; want refused on vatPercent", err)
	}
	if !errors.Is(err, ErrOrderNotWritten) {
		t.Fatalf("the refusal is not marked ErrOrderNotWritten: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("files were written: %v", entries)
	}
}
