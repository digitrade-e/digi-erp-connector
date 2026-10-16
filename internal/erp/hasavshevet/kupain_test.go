package hasavshevet

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/text/encoding/charmap"
)

// The two goldens under testdata/kupain were produced by the LEGACY PHP writer
// itself, not by this package: getHerpRecieptConstantLengths,
// CreateRecieptPrmDoc, prepareForHerpDocReciept, WriteDocSpaces and
// CreateRecieptMainDoc copied verbatim out of bfl_app
// backend/app/new-api/sync/global_api.php (lines 5282-5925) into a stub class
// and run under php:8.3-cli (glibc iconv), fed the synthetic values of
// goldenReceipt below. KUPAIN.prm is therefore byte-for-byte what the legacy
// system writes; KUPAIN_redacted.doc is a legacy record of the same length with
// made-up values. Do not regenerate them from this package's output.

func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "kupain", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return b
}

func decode1255(t *testing.T, b []byte) string {
	t.Helper()
	s, err := charmap.Windows1255.NewDecoder().Bytes(b)
	if err != nil {
		t.Fatalf("decode Windows-1255: %v", err)
	}
	return string(s)
}

// fieldSpan is a 1-based inclusive byte range, as KUPAIN.prm states it.
type fieldSpan struct{ start, end int }

// goldenSpans parses the legacy KUPAIN.prm so the record tests locate fields
// by the legacy file's own positions, not by this package's table.
func goldenSpans(t *testing.T) map[int]fieldSpan {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(decode1255(t, readGolden(t, "KUPAIN.prm")), "\n"), "\n")
	spans := map[int]fieldSpan{}
	for i, line := range lines[1:] { // line 1 is the record length
		pos := strings.Fields(strings.SplitN(line, ";", 2)[0])
		start, _ := strconv.Atoi(pos[0])
		end, _ := strconv.Atoi(pos[1])
		spans[i+2] = fieldSpan{start, end}
	}
	return spans
}

// field returns field n's bytes from a record, excluding its trailing space.
func field(t *testing.T, record []byte, n int) []byte {
	t.Helper()
	s, ok := goldenSpans(t)[n]
	if !ok || s.start == 0 {
		t.Fatalf("field %d has no position in KUPAIN.prm", n)
	}
	return record[s.start-1 : s.end]
}

func blank(width int) []byte { return bytes.Repeat([]byte{' '}, width) }

func padded(t *testing.T, s string, width int) []byte {
	t.Helper()
	b, err := charmap.Windows1255.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatalf("test fixture %q is not Windows-1255: %v", s, err)
	}
	return append(b, blank(width-len(b))...)
}

// goldenReceipt is the Go form of the values the legacy writer was fed to
// produce KUPAIN_redacted.doc, on the flat §4.6a wire. remarks is what the
// legacy wrote into F7 ("{refs} | 2824019"): the backend now sends F7 whole.
func goldenReceipt() ReceiptRequest {
	return ReceiptRequest{
		IdempotencyKey:   "0b8f6a4e-3c1d-4e2f-9a7b-5c6d7e8f9a0b",
		AccountKey:       "100234",
		SettledDate:      "2026-10-11",
		AmountMinor:      710700,
		Installments:     1,
		CardIssuer:       "2", // HYP Issuer 2 → ויזה …
		CardBrand:        "2", // … and Brand 2 (Visa), which the legacy table has no name for; the issuer wins
		Last4:            "1234",
		ConfirmationCode: "0123456",
		ProviderUID:      "26101112345678901234567",
		Remarks:          "INV-1001, INV-1002 | 2824019",
		AgentKey:         "17",
		Name:             `לקוח בדיקה בע"מ`,
		Address:          "רחוב הדוגמה 12",
		Phone:            "04-0000000",
		Email:            "test@example.com",
	}
}

const goldenReceiptNumber = 4567

func mustBuild(t *testing.T, req ReceiptRequest) ([]byte, []string) {
	t.Helper()
	rec, warns, err := buildKUPAINRecord(req, goldenReceiptNumber, ReceiptCardNames{})
	if err != nil {
		t.Fatalf("buildKUPAINRecord: %v", err)
	}
	return rec, warns
}

func wantFieldError(t *testing.T, err error, wantField string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want an error naming %q, got nil", wantField)
	}
	var fe *FieldError
	if !errors.As(err, &fe) {
		t.Fatalf("want *FieldError naming %q, got %T: %v", wantField, err, err)
	}
	if fe.Field != wantField {
		t.Fatalf("error names field %q, want %q (%v)", fe.Field, wantField, err)
	}
	if !strings.Contains(err.Error(), wantField) {
		t.Fatalf("error text %q does not name %q", err.Error(), wantField)
	}
}

// ---------------------------------------------------------------- .prm

func TestGenerateKUPAINPRM_Golden(t *testing.T) {
	got := generateKUPAINPRM()
	want := readGolden(t, "KUPAIN.prm")
	if !bytes.Equal(got, want) {
		t.Fatalf("KUPAIN.prm differs from the legacy golden\n got: %q\nwant: %q",
			decode1255(t, got), decode1255(t, want))
	}
}

func TestGenerateKUPAINPRM_Shape(t *testing.T) {
	prm := generateKUPAINPRM()
	if bytes.ContainsRune(prm, '\r') {
		t.Fatal("KUPAIN.prm contains CR; the legacy writer uses LF only")
	}
	if !bytes.HasSuffix(prm, []byte("\n")) {
		t.Fatal("KUPAIN.prm does not end with LF")
	}
	lines := strings.Split(strings.TrimSuffix(decode1255(t, prm), "\n"), "\n")
	if len(lines) != 43 { // line 1 + fields 2..43
		t.Fatalf("KUPAIN.prm has %d lines, want 43", len(lines))
	}
	exact := map[int]string{
		1:  "1260 ;אורך רשומה - 1",
		2:  "1 15 ;מפתח לקוח - 2",
		3:  "17 25 ;מספר קבלה - 3",
		32: "0 0 ;32 - הערה נוספת 3",
		38: "0 0 ;38- תאריך נוסף 1",
		40: "0 0 ;40- מספר נוסף 1",
		42: "1158 1207 ;42 - כתובת מייל",
		43: "1209 1258 ;43 - מספר שובר",
	}
	for n, want := range exact {
		if lines[n-1] != want {
			t.Errorf("line %d = %q, want %q", n, lines[n-1], want)
		}
	}
	for n := 30; n <= 41; n++ {
		if !strings.HasPrefix(lines[n-1], "0 0 ;") {
			t.Errorf("line %d = %q, want a zero-length field", n, lines[n-1])
		}
	}
}

// ---------------------------------------------------------------- .doc

func TestGenerateKUPAINDOC_Golden(t *testing.T) {
	got, _, err := generateKUPAINDOC(goldenReceipt(), goldenReceiptNumber, ReceiptCardNames{})
	if err != nil {
		t.Fatalf("generateKUPAINDOC: %v", err)
	}
	want := readGolden(t, "KUPAIN_redacted.doc")
	if !bytes.Equal(got, want) {
		for i := range min(len(got), len(want)) {
			if got[i] != want[i] {
				t.Fatalf("KUPAIN.doc differs from the legacy golden at byte %d (got %d bytes, want %d)\n got: %q\nwant: %q",
					i+1, len(got), len(want), decode1255(t, got), decode1255(t, want))
			}
		}
		t.Fatalf("KUPAIN.doc length %d, want %d", len(got), len(want))
	}
}

func TestGenerateKUPAINDOC_Shape(t *testing.T) {
	doc, _, err := generateKUPAINDOC(goldenReceipt(), goldenReceiptNumber, ReceiptCardNames{})
	if err != nil {
		t.Fatalf("generateKUPAINDOC: %v", err)
	}
	if len(doc) != 1260 || doc[1259] != '\n' {
		t.Fatalf("doc is %d bytes, want 1259 + LF", len(doc))
	}
	if bytes.ContainsRune(doc, '\r') {
		t.Fatal("doc contains CR")
	}
}

// fullWidthReceipt fills every written field to its full width, so a missing
// separator cannot hide behind padding.
func fullWidthReceipt() ReceiptRequest {
	r := goldenReceipt()
	r.AccountKey = strings.Repeat("K", 15)
	r.Name = strings.Repeat("ש", 50)
	r.Address = strings.Repeat("A", 50)
	r.Phone = strings.Repeat("9", 50)
	r.AgentKey = strings.Repeat("7", 9)
	r.Email = strings.Repeat("e", 50)
	r.AmountMinor = 99999999
	r.CardIssuer = "5" // אלפא קארד/לאומי קארד, 20 bytes
	r.ConfirmationCode = strings.Repeat("8", 9)
	r.ProviderUID = strings.Repeat("U", 250)
	r.Remarks = strings.Repeat("R", 250)
	return r
}

func TestKUPAINRecord_EveryFieldEndsWithOneSpace(t *testing.T) {
	rec, _, err := buildKUPAINRecord(fullWidthReceipt(), 999999999, ReceiptCardNames{})
	if err != nil {
		t.Fatalf("buildKUPAINRecord: %v", err)
	}
	if len(rec) != 1259 {
		t.Fatalf("record is %d bytes, want 1259", len(rec))
	}
	for n, s := range goldenSpans(t) {
		if s.start == 0 {
			continue
		}
		if rec[s.end] != ' ' { // the byte right after the field (1-based end == 0-based next)
			t.Errorf("field %d is not followed by a space (0x%02x)", n, rec[s.end])
		}
	}
	if got := field(t, rec, 2); !bytes.Equal(got, []byte(strings.Repeat("K", 15))) {
		t.Errorf("F2 = %q", got)
	}
	if got := field(t, rec, 3); string(got) != "999999999" {
		t.Errorf("F3 = %q", got)
	}
}

func TestKUPAINRecord_FieldPlacement(t *testing.T) {
	rec, _ := mustBuild(t, goldenReceipt())
	cases := []struct {
		n    int
		want string
	}{
		{2, "100234"},
		{3, "4567"},
		{4, "33"},
		{5, "11/10/2026"},
		{6, "11/10/2026"},
		{7, "INV-1001, INV-1002 | 2824019"},
		{12, "7107.00"},
		{16, "1"},
		{17, "ויזה"},
		{20, "17"},
		{22, "0123456"},
		{23, "26101112345678901234567"},
		{24, `לקוח בדיקה בע"מ`},
		{25, "רחוב הדוגמה 12"},
		{26, ""},
		{27, "04-0000000"},
		{28, "1234"},
		{29, ""},
		{42, "test@example.com"},
		{43, "0123456"},
	}
	spans := goldenSpans(t)
	for _, c := range cases {
		width := spans[c.n].end - spans[c.n].start + 1
		if got, want := field(t, rec, c.n), padded(t, c.want, width); !bytes.Equal(got, want) {
			t.Errorf("F%d = %q, want %q", c.n, decode1255(t, got), c.want)
		}
	}
	for _, n := range []int{8, 9, 10, 11, 13, 14, 15, 18, 19, 21, 26, 29} {
		got := field(t, rec, n)
		if !bytes.Equal(got, blank(len(got))) {
			t.Errorf("F%d = %q, want blank", n, got)
		}
	}
}

// F26 (city) and F29 (first payment) are the connector's, and blank, as the
// legacy wrote them: the wire carries neither.
func TestKUPAINRecord_CityAndFirstPaymentAreAlwaysBlank(t *testing.T) {
	rec, _ := mustBuild(t, fullWidthReceipt())
	for _, n := range []int{26, 29} {
		if got := field(t, rec, n); !bytes.Equal(got, blank(len(got))) {
			t.Errorf("F%d = %q, want blank", n, got)
		}
	}
}

// ---------------------------------------------------------------- amounts

func TestFormatAmount(t *testing.T) {
	ok := map[int64]string{
		710700:   "7107.00",
		5:        "0.05",
		100:      "1.00",
		1:        "0.01",
		123456:   "1234.56",
		99999999: "999999.99",
	}
	for minor, want := range ok {
		got, err := formatAmount(minor)
		if err != nil || got != want {
			t.Errorf("formatAmount(%d) = %q, %v; want %q", minor, got, err, want)
		}
	}
	for _, bad := range []int64{0, -1, -710700, 100000000} {
		if got, err := formatAmount(bad); err == nil {
			t.Errorf("formatAmount(%d) = %q, want an error", bad, got)
		}
	}
}

func TestKUPAINRecord_AmountOutOfRange(t *testing.T) {
	r := goldenReceipt()
	r.AmountMinor = 100000000
	_, _, err := buildKUPAINRecord(r, goldenReceiptNumber, ReceiptCardNames{})
	wantFieldError(t, err, "amountMinor")
}

// ---------------------------------------------------------------- encoding

func TestEncode1255Strict_HebrewRoundTrips(t *testing.T) {
	in := "שלום עולם, אבגדהוזחטיכלמנסעפצקרשת ךםןףץ 0123 ₪"
	b, err := encode1255Strict(in)
	if err != nil {
		t.Fatalf("encode1255Strict: %v", err)
	}
	if len(b) != len([]rune(in)) {
		t.Fatalf("%d bytes for %d runes; Windows-1255 is one byte per character", len(b), len([]rune(in)))
	}
	if got := decode1255(t, b); got != in {
		t.Fatalf("round trip = %q, want %q", got, in)
	}
	if b[0] != 0xF9 { // ש
		t.Fatalf("ש encoded to 0x%02x, want 0xF9", b[0])
	}
}

func TestEncode1255Strict_NiqqudEncodes(t *testing.T) {
	// shin, qamats, shin dot — the canonical (NFC) order.
	b, err := encode1255Strict("ש\u05B8\u05C1")
	if err != nil {
		t.Fatalf("encode1255Strict: %v", err)
	}
	if !bytes.Equal(b, []byte{0xF9, 0xC8, 0xD1}) {
		t.Fatalf("niqqud encoded to % x, want f9 c8 d1", b)
	}
}

func TestNormalize1255_NFDAndNFCEncodeTheSame(t *testing.T) {
	forms := []string{
		"ש\u05B8\u05C1", // NFC: shin, qamats, shin dot
		"ש\u05C1\u05B8", // marks in the other order
		"\uFB2A\u05B8",  // presentation form U+FB2A shin-with-shin-dot, then qamats
	}
	var want []byte
	for i, f := range forms {
		got, err := encode1255Strict(normalize1255(f))
		if err != nil {
			t.Fatalf("form %d %+q: %v", i, f, err)
		}
		if i == 0 {
			want = got
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("form %d %+q encoded to % x, want % x", i, f, got, want)
		}
	}
}

func TestNormalize1255_StripsBidiControls(t *testing.T) {
	controls := []rune{
		0x200E, 0x200F, // LRM, RLM — both ARE representable in Windows-1255 (0xFD/0xFE)
		0x202A, 0x202B, 0x202C, 0x202D, 0x202E,
		0x2066, 0x2067, 0x2068, 0x2069,
		0x061C, // Arabic letter mark
	}
	for _, c := range controls {
		in := "א" + string(c) + "ב"
		if got := normalize1255(in); got != "אב" {
			t.Errorf("normalize1255(%+q) = %+q, want %+q", in, got, "אב")
		}
	}
}

func TestNormalize1255_LineBreaksApostrophesTabs(t *testing.T) {
	cases := map[string]string{
		"a\r\nb":   "a. b",
		"a\rb":     "a. b",
		"a\nb":     "a. b",
		"a\r\n\nb": "a. . b",
		"צ'ק":      "צק",
		"it's":     "its",
		"a\tb":     "a b",
		"a\u00A0b": "a b",  // NBSP is a space
		"a\u00ADb": "ab",   // soft hyphen
		"a\u200Cb": "ab",   // ZWNJ
		"a\u200Db": "ab",   // ZWJ
		"\uFEFFab": "ab",   // BOM
		`בע"מ`:     `בע"מ`, // gershayim stay
	}
	for in, want := range cases {
		if got := normalize1255(in); got != want {
			t.Errorf("normalize1255(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEncode1255Strict_RejectsWhatItCannotEncode(t *testing.T) {
	for _, in := range []string{"مرحبا", "café", "日本", "a\x00b", "a\x1bb", "\xff"} {
		b, err := encode1255Strict(in)
		if err == nil {
			t.Errorf("encode1255Strict(%q) = % x, want an error", in, b)
			continue
		}
		if bytes.ContainsRune(b, '?') {
			t.Errorf("encode1255Strict(%q) substituted '?'", in)
		}
	}
}

func TestKUPAINRecord_ArabicIsAFieldError(t *testing.T) {
	r := goldenReceipt()
	r.Name = "شركة الاختبار"
	rec, _, err := buildKUPAINRecord(r, goldenReceiptNumber, ReceiptCardNames{})
	wantFieldError(t, err, "name")
	if rec != nil {
		t.Fatal("a record was returned alongside the error")
	}
}

func TestKUPAINRecord_NormalisesBeforeWriting(t *testing.T) {
	r := goldenReceipt()
	r.Address = "\u200Fרחוב\r\nהדוגמה 12\u202C"
	r.Name = "צ'יפס בע\"מ"
	rec, _ := mustBuild(t, r)
	if got := field(t, rec, 25); !bytes.Equal(got, padded(t, "רחוב. הדוגמה 12", 50)) {
		t.Errorf("F25 = %q", decode1255(t, got))
	}
	if got := field(t, rec, 24); !bytes.Equal(got, padded(t, `ציפס בע"מ`, 50)) {
		t.Errorf("F24 = %q", decode1255(t, got))
	}
}

// ---------------------------------------------------------------- widths

func TestKUPAINRecord_OneByteOverWidthNamesTheField(t *testing.T) {
	cases := []struct {
		field  string
		mutate func(*ReceiptRequest, string)
		width  int
	}{
		{"accountKey", func(r *ReceiptRequest, v string) { r.AccountKey = v }, 15},
		{"name", func(r *ReceiptRequest, v string) { r.Name = v }, 50},
		{"address", func(r *ReceiptRequest, v string) { r.Address = v }, 50},
		{"phone", func(r *ReceiptRequest, v string) { r.Phone = v }, 50},
		{"agentKey", func(r *ReceiptRequest, v string) { r.AgentKey = v }, 9},
		{"email", func(r *ReceiptRequest, v string) { r.Email = v }, 50},
		{"confirmationCode", func(r *ReceiptRequest, v string) { r.ConfirmationCode = v }, 9},
		{"providerUid", func(r *ReceiptRequest, v string) { r.ProviderUID = v }, 250},
		{"remarks", func(r *ReceiptRequest, v string) { r.Remarks = v }, 250},
	}
	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			for _, letter := range []string{"x", "ש"} { // ASCII and Hebrew are both one byte
				r := goldenReceipt()
				c.mutate(&r, strings.Repeat(letter, c.width))
				if _, _, err := buildKUPAINRecord(r, goldenReceiptNumber, ReceiptCardNames{}); err != nil {
					t.Fatalf("exactly %d bytes of %q rejected: %v", c.width, letter, err)
				}
				r = goldenReceipt()
				c.mutate(&r, strings.Repeat(letter, c.width+1))
				rec, _, err := buildKUPAINRecord(r, goldenReceiptNumber, ReceiptCardNames{})
				wantFieldError(t, err, c.field)
				if rec != nil {
					t.Fatal("over-width value was truncated into a record")
				}
			}
		})
	}
}

func TestKUPAINRecord_RequiredAndShape(t *testing.T) {
	cases := []struct {
		field  string
		mutate func(*ReceiptRequest)
	}{
		{"accountKey", func(r *ReceiptRequest) { r.AccountKey = "" }},
		{"accountKey", func(r *ReceiptRequest) { r.AccountKey = " \u00A0" }},
		{"name", func(r *ReceiptRequest) { r.Name = "" }},
		{"name", func(r *ReceiptRequest) { r.Name = "\u200F" }}, // nothing left once normalised
		{"confirmationCode", func(r *ReceiptRequest) { r.ConfirmationCode = "" }},
		{"remarks", func(r *ReceiptRequest) { r.Remarks = "" }},
		{"last4", func(r *ReceiptRequest) { r.Last4 = "123" }},
		{"last4", func(r *ReceiptRequest) { r.Last4 = "12a4" }},
		{"last4", func(r *ReceiptRequest) { r.Last4 = "١٢٣٤" }}, // Arabic-Indic digits
		{"settledDate", func(r *ReceiptRequest) { r.SettledDate = "11/10/2026" }},
		{"settledDate", func(r *ReceiptRequest) { r.SettledDate = "2026-02-30" }},
		{"settledDate", func(r *ReceiptRequest) { r.SettledDate = "2026-1-5" }},
		{"settledDate", func(r *ReceiptRequest) { r.SettledDate = "" }},
		{"amountMinor", func(r *ReceiptRequest) { r.AmountMinor = 0 }},
		{"amountMinor", func(r *ReceiptRequest) { r.AmountMinor = 100000000 }},
		{"installments", func(r *ReceiptRequest) { r.Installments = 0 }},
		{"cardIssuer", func(r *ReceiptRequest) { r.CardIssuer = "123" }},
		{"cardIssuer", func(r *ReceiptRequest) { r.CardIssuer = " 1" }},
		{"cardIssuer", func(r *ReceiptRequest) { r.CardIssuer = "٢" }},
		{"cardBrand", func(r *ReceiptRequest) { r.CardBrand = "x" }},
	}
	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			r := goldenReceipt()
			c.mutate(&r)
			_, _, err := buildKUPAINRecord(r, goldenReceiptNumber, ReceiptCardNames{})
			wantFieldError(t, err, c.field)
		})
	}
}

func TestKUPAINRecord_ReceiptNumberRange(t *testing.T) {
	for _, n := range []int64{0, -1, 1000000000} {
		_, _, err := buildKUPAINRecord(goldenReceipt(), n, ReceiptCardNames{})
		wantFieldError(t, err, "receiptNumber")
	}
}

// ---------------------------------------------------------------- card name

func TestCardName_EveryLegacyCode(t *testing.T) {
	cases := []struct {
		issuer, brand, want, from string
	}{
		// every issuer code the legacy names (global_api.php:5339-5344)
		{"1", "", "ישראכרט", "cardIssuer"},
		{"2", "", "ויזה", "cardIssuer"},
		{"5", "", "אלפא קארד/לאומי קארד", "cardIssuer"},
		// the only brand codes the legacy names (:5345-5348)
		{"", "4", "אמריקן אקספרס", "cardBrand"},
		{"", "3", "דיינרס", "cardBrand"},
		// issuer first: a known issuer wins over a known brand
		{"2", "4", "ויזה", "cardIssuer"},
		{"1", "3", "ישראכרט", "cardIssuer"},
		{"5", "4", "אלפא קארד/לאומי קארד", "cardIssuer"},
		// an issuer the table does not know falls through to the brand
		{"9", "3", "דיינרס", "cardBrand"},
		// codes compare as numbers, as the legacy PHP == did ("01" == "1")
		{"01", "", "ישראכרט", "cardIssuer"},
		{"", "04", "אמריקן אקספרס", "cardBrand"},
		{"00", "03", "דיינרס", "cardBrand"},
	}
	for _, c := range cases {
		got, from, warn := cardName(c.issuer, c.brand, ReceiptCardNames{})
		if got != c.want || from != c.from || warn != "" {
			t.Errorf("cardName(%q, %q) = %q from %q, warning %q; want %q from %q, no warning",
				c.issuer, c.brand, got, from, warn, c.want, c.from)
		}
	}
}

func TestCardName_UnknownIsBlankWithWarning(t *testing.T) {
	// {"", "2"} is the brand-only path: issuer null, HYP Brand 2 (Visa). The
	// legacy names no brand but 3 and 4, and wrote "JCB" for the rest.
	for _, c := range [][2]string{{"", "2"}, {"", "1"}, {"7", "6"}, {"", ""}} {
		got, _, warn := cardName(c[0], c[1], ReceiptCardNames{})
		if got != "" || warn == "" {
			t.Errorf("cardName(%q, %q) = %q, %q; want blank and a warning", c[0], c[1], got, warn)
		}
		if got == "JCB" {
			t.Errorf("cardName(%q, %q) fell back to the legacy JCB", c[0], c[1])
		}
	}
}

func TestCardName_Overrides(t *testing.T) {
	names := ReceiptCardNames{
		Issuer: map[string]string{"2": "ויזה כאל"},
		Brand:  map[string]string{"1": "מאסטרקארד"},
	}
	if got, _, _ := cardName("2", "", names); got != "ויזה כאל" {
		t.Errorf("issuer override = %q", got)
	}
	if got, _, _ := cardName("", "1", names); got != "מאסטרקארד" {
		t.Errorf("brand override = %q", got)
	}
	if got, _, _ := cardName("1", "", names); got != "ישראכרט" {
		t.Errorf("built-in lost when another code is overridden: %q", got)
	}
	if got, _, _ := cardName("02", "", names); got != "ויזה כאל" {
		t.Errorf("issuer override for \"02\" = %q; codes compare as numbers", got)
	}
	if got, _, _ := cardName("", "01", ReceiptCardNames{Brand: map[string]string{"01": "מאסטרקארד"}}); got != "מאסטרקארד" {
		t.Errorf("override keyed \"01\" for brand \"01\" = %q", got)
	}
	if got, _, _ := cardName("", "1", ReceiptCardNames{Brand: map[string]string{"01": "מאסטרקארד"}}); got != "מאסטרקארד" {
		t.Errorf("override keyed \"01\" for brand \"1\" = %q", got)
	}
}

// TestKUPAINRecord_BrandOnlyPath is the golden record with the issuer missing
// and HYP Brand 2: everything is byte-identical to the legacy golden except
// F17, which is blank, and the builder reports why.
func TestKUPAINRecord_BrandOnlyPath(t *testing.T) {
	r := goldenReceipt()
	r.CardIssuer = ""
	r.CardBrand = "2"
	rec, warns := mustBuild(t, r)

	want := bytes.Clone(readGolden(t, "KUPAIN_redacted.doc")[:1259])
	s := goldenSpans(t)[17]
	copy(want[s.start-1:s.end], blank(s.end-s.start+1))
	if !bytes.Equal(rec, want) {
		t.Fatalf("brand-only record differs from the golden with F17 blanked\n got: %q\nwant: %q",
			decode1255(t, rec), decode1255(t, want))
	}
	if len(warns) != 1 || !strings.Contains(warns[0], `"2"`) {
		t.Fatalf("warnings = %q, want one naming brand \"2\"", warns)
	}
}

func TestKUPAINRecord_IssuerWinsOverBrand(t *testing.T) {
	r := goldenReceipt()
	r.CardIssuer, r.CardBrand = "1", "4" // Isracard issuer, Amex brand
	rec, warns := mustBuild(t, r)
	if got := field(t, rec, 17); !bytes.Equal(got, padded(t, "ישראכרט", 25)) {
		t.Errorf("F17 = %q, want the issuer's name", decode1255(t, got))
	}
	if len(warns) != 0 {
		t.Errorf("unexpected warnings %q", warns)
	}
}

func TestKUPAINRecord_UnknownCardBlankF17AndWarns(t *testing.T) {
	r := goldenReceipt()
	r.CardIssuer = "7"
	r.CardBrand = "6"
	rec, warns := mustBuild(t, r)
	if got := field(t, rec, 17); !bytes.Equal(got, blank(25)) {
		t.Errorf("F17 = %q, want blank", decode1255(t, got))
	}
	if len(warns) != 1 || !strings.Contains(warns[0], `"7"`) || !strings.Contains(warns[0], `"6"`) {
		t.Fatalf("warnings = %q, want one naming both codes", warns)
	}
}

func TestValidateReceiptCardNames(t *testing.T) {
	ok := ReceiptCardNames{
		Issuer: map[string]string{"2": "ויזה כאל", "6": strings.Repeat("ו", 25)},
		Brand:  map[string]string{"1": "מאסטרקארד", "9": ""}, // "" = no override
	}
	if err := ValidateReceiptCardNames(ok); err != nil {
		t.Fatalf("valid names rejected: %v", err)
	}
	if err := ValidateReceiptCardNames(ReceiptCardNames{}); err != nil {
		t.Fatalf("zero value rejected: %v", err)
	}
	bad := []ReceiptCardNames{
		{Issuer: map[string]string{"2": strings.Repeat("ו", 26)}},
		{Brand: map[string]string{"1": "ماستركارد"}},
		{Brand: map[string]string{" ": "x"}},
	}
	for i, n := range bad {
		if err := ValidateReceiptCardNames(n); err == nil {
			t.Errorf("bad[%d] accepted", i)
		}
	}
}

// An over-wide override is the operator's configuration, not the caller's
// request: it must not come back as a FieldError (which the route turns into
// a 400 blaming the request).
func TestKUPAINRecord_BadCardNameOverrideIsNotAFieldError(t *testing.T) {
	names := ReceiptCardNames{Issuer: map[string]string{"2": strings.Repeat("ו", 26)}}
	rec, _, err := buildKUPAINRecord(goldenReceipt(), goldenReceiptNumber, names)
	if err == nil || rec != nil {
		t.Fatalf("over-wide override accepted: %v", err)
	}
	var fe *FieldError
	if errors.As(err, &fe) {
		t.Fatalf("over-wide override reported as a request FieldError: %v", err)
	}
	var ce *CardNamesConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("want *CardNamesConfigError, got %T: %v", err, err)
	}
}

// ---------------------------------------------------------------- trimming

func TestKUPAINRecord_IdentifiersAreTrimmed(t *testing.T) {
	r := goldenReceipt()
	r.AccountKey = "  " + strings.Repeat("K", 15) + " \t"
	r.AgentKey = "\u00A017 "
	r.ConfirmationCode = " 012345678 "
	rec, _ := mustBuild(t, r) // 15 and 9 bytes once trimmed: not over width
	cases := []struct {
		n    int
		want string
	}{
		{2, strings.Repeat("K", 15)},
		{20, "17"},
		{22, "012345678"},
		{43, "012345678"},
	}
	spans := goldenSpans(t)
	for _, c := range cases {
		width := spans[c.n].end - spans[c.n].start + 1
		if got := field(t, rec, c.n); !bytes.Equal(got, padded(t, c.want, width)) {
			t.Errorf("F%d = %q, want %q", c.n, got, c.want)
		}
	}
}

// Two keys for one code ("6" and "06") would make F17 depend on map order —
// a different card name from run to run. The table is refused instead.
func TestValidateReceiptCardNames_RefusesTwoKeysForOneCode(t *testing.T) {
	for _, names := range []ReceiptCardNames{
		{Issuer: map[string]string{"6": "מקס", "06": "לאומי קארד"}},
		{Brand: map[string]string{"1": "מאסטרקארד", " 01 ": "מאסטרקארד"}},
		{Issuer: map[string]string{"6": "", "006": "מקס"}},
	} {
		err := ValidateReceiptCardNames(names)
		if err == nil {
			t.Errorf("%v accepted; want two keys for one code refused", names)
			continue
		}
		if !strings.Contains(err.Error(), "same code") {
			t.Errorf("error %q does not say the keys name the same code", err)
		}
	}
	// One code in each table is not a clash: issuer 6 and brand 6 are different cards.
	if err := ValidateReceiptCardNames(ReceiptCardNames{
		Issuer: map[string]string{"6": "מקס"}, Brand: map[string]string{"06": "דיינרס"},
	}); err != nil {
		t.Errorf("issuer and brand sharing a code refused: %v", err)
	}
}
