package hasavshevet

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"golang.org/x/text/encoding/charmap"
)

// fullHeader sets every header field to a value of its own, Hebrew where the
// field is a text.
func fullHeader() *OrderHeader {
	return &OrderHeader{
		AccountNameDetail: "משה כהן",
		Address:           "הרצל 12, דירה 3",
		City:              "תל אביב-יפו",
		Phone:             "050-1234567",
		ExtraText1:        "לקוח 'בדיקה'|משה כהן",
		ExtraText2:        "הרצל 12",
		ExtraText3:        "SHIP-778",
		ExtraText4:        "ממתין",
		ExtraText5:        "PO-2026/11",
		ExtraSum1:         "1250.50",
		ExtraSum2:         "99",
		ExtraSum3:         "18",
		ExtraNum1:         "1",
		ExtraNum2:         "-2",
	}
}

// wantHeaderLines is what fullHeader writes in each line (trailing padding
// removed): texts as sanitizeField leaves them, numbers as sent, the account
// name of imoveinGoldenCases ("לקוח 'בדיקה' בע"מ", its quote dropped) joined
// with the detail.
var wantHeaderLines = map[int]string{
	5:  `לקוח בדיקה בע"מ|משה כהן`,
	6:  "הרצל 12, דירה 3",
	7:  "תל אביב-יפו",
	35: "050-1234567",
	45: "לקוח בדיקה|משה כהן",
	46: "הרצל 12",
	47: "SHIP-778",
	48: "ממתין",
	49: "PO-2026/11",
	50: "1250.50",
	51: "99",
	52: "18",
	81: "1",
	82: "-2",
}

func renderWith(t *testing.T, format string, req OrderRequest, n int64) []byte {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{ERP: config.ERPHasavshevet, SendOrderDir: dir, SendOrderFormat: format}
	s := NewSender(nil, cfg, NewOrderNumberStore(filepath.Join(dir, "lastOrderNumber.json")), noopLogger{})
	if _, err := s.processOrderWithNumber(context.Background(), req, n); err != nil {
		t.Fatalf("processOrderWithNumber: %v", err)
	}
	return readFile(t, filepath.Join(dir, "IMOVEIN.doc"))
}

// legacySpan is the byte range of a written legacy field within one line.
func legacySpan(line int) (start, end int) {
	pos := 0
	for _, f := range legacyFields {
		if f.width == 0 {
			continue
		}
		if f.line == line {
			return pos, pos + f.width
		}
		pos += f.width + 1
	}
	panic("no legacy field " + strconv.Itoa(line))
}

func headerText1255(t *testing.T, b []byte) string {
	t.Helper()
	s, err := charmap.Windows1255.NewDecoder().Bytes(b)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(string(s), " ")
}

// A header with no field set — nil, or an empty object — writes exactly the
// goldens' bytes in both layouts: today's tenants see no change.
func TestHeader_EmptyIsTheGoldenBytes(t *testing.T) {
	for _, tc := range imoveinGoldenCases() {
		for _, h := range []*OrderHeader{nil, {}} {
			req := tc.req
			req.Header = h
			if got, want := renderWith(t, "", req, tc.number), readFile(t, goldenPath(tc.name+".doc")); !bytes.Equal(got, want) {
				t.Errorf("%s header=%v: bytes differ from the golden%s", tc.name, h, firstDiff(got, want))
			}
		}
	}
	for _, tc := range legacyRequestCases() {
		req := tc.req
		req.Header = &OrderHeader{}
		if got, want := renderWith(t, config.OrderFormatLegacy, req, tc.number), readFile(t, legacyGoldenPath(tc.name+".doc")); !bytes.Equal(got, want) {
			t.Errorf("legacy %s: an empty header changed the bytes", tc.name)
		}
	}
}

// Each header field lands in its own IMOVEIN field, in Windows-1255 that
// decodes back to the text sent (Hebrew included); every other byte of the
// record is the golden's.
func TestHeader_EachFieldLandsInItsLine_Official(t *testing.T) {
	tc := imoveinGoldenCases()[0]
	req := tc.req
	req.Header = fullHeader()
	doc := renderWith(t, "", req, tc.number)
	golden := readFile(t, goldenPath(tc.name+".doc"))
	if len(doc) != len(golden) {
		t.Fatalf("length %d; golden %d", len(doc), len(golden))
	}
	rec := prmTotalRecordLength + 1
	inHeader := make([]bool, rec)
	for line, want := range wantHeaderLines {
		start, end := imoveinSpan("line" + strconv.Itoa(line))
		for i := start; i < end; i++ {
			inHeader[i] = true
		}
		for r := 0; r*rec < len(doc); r++ { // the header repeats on every record
			if got := headerText1255(t, doc[r*rec+start:r*rec+end]); got != want {
				t.Errorf("record %d line%d = %q; want %q", r, line, got, want)
			}
		}
	}
	for i := range doc {
		if !inHeader[i%rec] && doc[i] != golden[i] {
			t.Fatalf("byte %d (%s) changed outside the header fields", i, fieldAt(i%rec))
		}
	}
}

func TestHeader_EachFieldLandsInItsLine_Legacy(t *testing.T) {
	tc := legacyRequestCases()[0]
	req := tc.req
	req.Header = fullHeader()
	doc := renderWith(t, config.OrderFormatLegacy, req, tc.number)
	golden := readFile(t, legacyGoldenPath(tc.name+".doc"))
	if len(doc) != len(golden) {
		t.Fatalf("length %d; golden %d", len(doc), len(golden))
	}
	rec := legacyRecordLength + 1
	inHeader := make([]bool, rec)
	for line, want := range wantHeaderLines {
		start, end := legacySpan(line)
		for i := start; i < end; i++ {
			inHeader[i] = true
		}
		for r := 0; r*rec < len(doc); r++ {
			if got := headerText1255(t, doc[r*rec+start:r*rec+end]); got != want {
				t.Errorf("record %d legacy line%d = %q; want %q", r, line, got, want)
			}
		}
	}
	for i := range doc {
		if !inHeader[i%rec] && doc[i] != golden[i] {
			t.Fatalf("byte %d changed outside the header fields", i)
		}
	}
}

// Hebrew at the full width of a field fits exactly (one byte per letter) and
// comes back unchanged; the encoding is Windows-1255, not UTF-8.
func TestHeader_HebrewRoundTripAtFullWidth(t *testing.T) {
	tc := imoveinGoldenCases()[0]
	req := tc.req
	full := strings.Repeat("ש", 50)
	req.Header = &OrderHeader{ExtraText2: full, City: "באר שבע"}
	doc := renderWith(t, "", req, tc.number)
	start, end := imoveinSpan("line46")
	raw := doc[start:end]
	if bytes.IndexByte(raw, ' ') != -1 {
		t.Fatalf("a 50-letter Hebrew text left padding in a 50-byte field: %q", raw)
	}
	if raw[0] != 0xf9 { // ש in Windows-1255
		t.Fatalf("line46 starts with byte %#x; want 0xf9 (ש in Windows-1255)", raw[0])
	}
	if got := headerText1255(t, raw); got != full {
		t.Fatalf("line46 = %q; want %q", got, full)
	}
	cs, ce := imoveinSpan("line7")
	if got := headerText1255(t, doc[cs:ce]); got != "באר שבע" {
		t.Fatalf("line7 = %q", got)
	}
}

func TestValidateHeader(t *testing.T) {
	he := func(n int) string { return strings.Repeat("א", n) }
	valid := []*OrderHeader{
		nil, {},
		{AccountNameDetail: he(49)},
		{Address: he(50)}, {City: he(50)}, {Phone: he(30)},
		{ExtraText1: he(50)}, {ExtraText5: he(50)},
		{ExtraText1: he(48) + "\n"},   // 48 + ". " = 50
		{ExtraText1: he(48) + "\r\n"}, // CR LF is one line break
		{ExtraText1: he(50) + "'"},    // the quote is dropped
		{ExtraSum1: "1234567890123"},  // 13
		{ExtraSum2: "99"}, {ExtraSum3: "-17.5"}, {ExtraNum1: "1"}, {ExtraNum2: "0012.000"},
		fullHeader(),
	}
	for _, h := range valid {
		if err := ValidateHeader(h); err != nil {
			t.Errorf("ValidateHeader(%+v) = %v; want valid", h, err)
		}
	}
	for _, tc := range []struct {
		name  string
		h     OrderHeader
		field string
	}{
		{"detail 50 + separator", OrderHeader{AccountNameDetail: he(50)}, "accountNameDetail"},
		{"address 51", OrderHeader{Address: he(51)}, "address"},
		{"city 51", OrderHeader{City: he(51)}, "city"},
		{"phone 31", OrderHeader{Phone: strings.Repeat("1", 31)}, "phone"},
		{"text 51", OrderHeader{ExtraText1: he(51)}, "extraText1"},
		{"text 49 + line break", OrderHeader{ExtraText3: he(49) + "\n"}, "extraText3"},
		{"text 51 ASCII", OrderHeader{ExtraText5: strings.Repeat("x", 51)}, "extraText5"},
		{"a tab", OrderHeader{ExtraText4: "a\tb"}, "extraText4"},
		{"a NUL", OrderHeader{ExtraText2: "a\x00b"}, "extraText2"},
		{"only spaces", OrderHeader{City: "   "}, "city"},
		{"sum 14", OrderHeader{ExtraSum1: "12345678901234"}, "extraSum1"},
		{"sum exponent", OrderHeader{ExtraSum2: "1e2"}, "extraSum2"},
		{"sum plus", OrderHeader{ExtraSum2: "+1"}, "extraSum2"},
		{"sum space", OrderHeader{ExtraSum3: " 18"}, "extraSum3"},
		{"sum trailing point", OrderHeader{ExtraSum3: "18."}, "extraSum3"},
		{"sum leading point", OrderHeader{ExtraSum3: ".5"}, "extraSum3"},
		{"sum comma", OrderHeader{ExtraSum1: "1,5"}, "extraSum1"},
		{"num words", OrderHeader{ExtraNum1: "yes"}, "extraNum1"},
		{"num minus only", OrderHeader{ExtraNum2: "-"}, "extraNum2"},
		{"num Arabic-Indic", OrderHeader{ExtraNum2: "١"}, "extraNum2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.h
			err := ValidateHeader(&h)
			var fe *FieldError
			if !errors.As(err, &fe) || fe.Field != "header/"+tc.field {
				t.Fatalf("ValidateHeader = %v; want a FieldError on header/%s", err, tc.field)
			}
		})
	}
}

// The legacy layout's header numbers are 12 characters, not 13: a 13-digit
// number that fits the official layout is refused there, never cut.
func TestValidateLegacyFit_HeaderNumbers(t *testing.T) {
	req := legacyRequestCases()[0].req
	req.Header = &OrderHeader{ExtraNum1: "1234567890123"}
	if err := ValidateHeader(req.Header); err != nil {
		t.Fatalf("13 digits fit the official layout: %v", err)
	}
	var fe *FieldError
	if err := ValidateLegacyFit(req); !errors.As(err, &fe) || fe.Field != "header/extraNum1" {
		t.Fatalf("ValidateLegacyFit = %v; want a FieldError on header/extraNum1", err)
	}
	req.Header = &OrderHeader{ExtraSum1: "123456789.000"} // 13, cut to 123456789.00: same number
	if err := ValidateLegacyFit(req); err != nil {
		t.Fatalf("a zero cut after the point keeps the number: %v", err)
	}
	req.Header = &OrderHeader{ExtraSum1: "123456789012"}
	if err := ValidateLegacyFit(req); err != nil {
		t.Fatalf("12 digits fit: %v", err)
	}
}

// The writer checks the header again for a caller that skipped the route:
// refused before anything is written, so the order stays retryable.
func TestSender_RefusesAnOverlongHeaderBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	s := NewSender(nil, config.Config{SendOrderDir: dir}, NewOrderNumberStore(filepath.Join(dir, "lastOrderNumber.json")), noopLogger{})
	req := imoveinGoldenCases()[0].req
	req.Header = &OrderHeader{ExtraText1: strings.Repeat("ת", 51)}
	_, err := s.processOrderWithNumber(context.Background(), req, 9)
	if err == nil || !strings.Contains(err.Error(), "header/extraText1") {
		t.Fatalf("processOrderWithNumber = %v; want refused on header/extraText1", err)
	}
	if !errors.Is(err, ErrOrderNotWritten) {
		t.Fatalf("not marked ErrOrderNotWritten: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("files were written: %v", entries)
	}
}

// The detail is kept whole; an account name too long for both loses its end,
// as it always was cut to the field.
func TestJoinAccountNameDetail(t *testing.T) {
	for _, tc := range []struct{ name, detail, want string }{
		{"לקוח", "משה", "לקוח|משה"},
		{"", "משה", "|משה"},
		{strings.Repeat("א", 60), "משה כהן", strings.Repeat("א", 42) + "|משה כהן"},
		{"abc", strings.Repeat("ב", 49), "|" + strings.Repeat("ב", 49)},
	} {
		got := joinAccountNameDetail(tc.name, tc.detail, 50)
		if got != tc.want {
			t.Errorf("joinAccountNameDetail(%q, %q) = %q; want %q", tc.name, tc.detail, got, tc.want)
		}
		if n := len([]rune(got)); n > 50 {
			t.Errorf("%q is %d characters", got, n)
		}
	}
}

// Applied lists what is set, in the fixed echo order, and nothing else.
func TestOrderHeader_Applied(t *testing.T) {
	if got := (*OrderHeader)(nil).Applied(); got == nil || len(got) != 0 {
		t.Fatalf("nil.Applied() = %#v; want an empty, non-nil list", got)
	}
	h := &OrderHeader{ExtraNum2: "1", City: "חיפה", ExtraText5: "PO"}
	if got := strings.Join(h.Applied(), ","); got != "city,extraText5,extraNum2" {
		t.Fatalf("Applied() = %s", got)
	}
	if got := strings.Join(fullHeader().Applied(), ","); got != strings.Join(HeaderFieldNames(), ",") {
		t.Fatalf("a full header applies %s; want every field", got)
	}
}

// A request without a header hashes as before DIG-1315 (the DIG-1291 index
// compares request hashes on a repeat).
func TestOrderRequestHash_UnchangedWithoutHeader(t *testing.T) {
	req := imoveinGoldenCases()[0].req
	req.Header = nil
	want := strings.TrimSpace(string(readFile(t, goldenPath("request-sha256.txt"))))
	if got := orderRequestHash(req); got != want {
		t.Fatalf("orderRequestHash = %s; golden %s", got, want)
	}
	req.Header = &OrderHeader{City: "חיפה"}
	if orderRequestHash(req) == want {
		t.Fatal("a header does not change the hash: a changed repeat would not be noticed")
	}
}
