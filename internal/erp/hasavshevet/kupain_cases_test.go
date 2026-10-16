package hasavshevet

import (
	"bytes"
	"strings"
	"testing"
)

// Edge cases around the legacy goldens (plan 056 §5.5.3 C1, §7.5): the record
// keeps the golden layout for text the backend lets through, amounts the
// legacy would have split, and F7 at its exact limit.

// F7 is the backend's remarks verbatim — the connector neither builds nor
// cuts it — and it holds exactly 250 Windows-1255 bytes: 250 Hebrew letters
// are 500 bytes of UTF-8 and still fit.
func TestKUPAINRecord_RemarksAtExactly250Bytes(t *testing.T) {
	for _, letter := range []string{"R", "ר"} {
		r := goldenReceipt()
		r.Remarks = strings.Repeat(letter, 250)
		rec, _ := mustBuild(t, r)
		if got := field(t, rec, 7); !bytes.Equal(got, padded(t, r.Remarks, 250)) {
			t.Errorf("F7 = %q, want 250 × %q", decode1255(t, got), letter)
		}
		r.Remarks += letter
		_, _, err := buildKUPAINRecord(r, goldenReceiptNumber, ReceiptCardNames{})
		wantFieldError(t, err, "remarks")
	}
	// The backend's own cut form ("a, b +3") is written as given.
	r := goldenReceipt()
	r.Remarks = strings.Repeat("9", 120) + ", " + strings.Repeat("8", 122) + " +17"
	if len(r.Remarks) != 248 {
		t.Fatalf("fixture is %d bytes", len(r.Remarks))
	}
	rec, _ := mustBuild(t, r)
	if got := field(t, rec, 7); !bytes.Equal(got, padded(t, r.Remarks, 250)) {
		t.Errorf("F7 = %q", got)
	}
}

// Amounts the installments would not divide are never split: §4.6a admits one
// installment only, so the amount is written whole in F12, F16 is 1 and F29 is
// blank — and any other count is refused before a number is used.
func TestKUPAINRecord_OddAmountsAreWrittenWholeInOneInstallment(t *testing.T) {
	for minor, want := range map[int64]string{1: "0.01", 1000: "10.00", 710701: "7107.01", 99999999: "999999.99", 333334: "3333.34"} {
		r := goldenReceipt()
		r.AmountMinor = minor
		rec, _ := mustBuild(t, r)
		if got := field(t, rec, 12); !bytes.Equal(got, padded(t, want, 9)) {
			t.Errorf("amount %d: F12 = %q, want %q", minor, got, want)
		}
		if got := field(t, rec, 16); string(got) != "1 " {
			t.Errorf("amount %d: F16 = %q, want %q", minor, got, "1 ")
		}
		if got := field(t, rec, 29); !bytes.Equal(got, blank(9)) {
			t.Errorf("amount %d: F29 = %q, want blank", minor, got)
		}
	}
	for _, n := range []int{0, 2, 3, 12, 99, -1} {
		r := goldenReceipt()
		r.AmountMinor = 1000 // 1000 in 3 would be 334 + 333 + 333
		r.Installments = n
		_, _, err := buildKUPAINRecord(r, goldenReceiptNumber, ReceiptCardNames{})
		wantFieldError(t, err, "installments")
	}
}

// TestKUPAINRecord_HebrewBidiThroughNormalize1255 is the whole path for the
// text the backend lets through: Hebrew with niqqud, a presentation form, bidi
// marks and isolates around a number, an apostrophe — each written as
// normalize1255 decides, in Windows-1255, and never cut. Arabic is refused
// (the code page has no Arabic letters), naming the field.
func TestKUPAINRecord_HebrewBidiThroughNormalize1255(t *testing.T) {
	r := goldenReceipt()
	r.Name = "\u202Bש\u05C1\u05B8לום\u202C בע\"מ"     // RLE … PDF around pointed Hebrew, marks out of NFC order
	r.Address = "\uFB2Aלום \u2067רחוב 12\u2069"       // presentation form שׁ + RLI … PDI
	r.Remarks = "\u200Fחשבונית 26004295\u200E, 19057" // RLM, LRM
	r.Phone = "04\u00A0812 3456"                      // a no-break space
	rec, _ := mustBuild(t, r)
	for _, c := range []struct {
		n    int
		want string
	}{
		{24, "ש\u05B8\u05C1לום בע\"מ"}, // NFC: qamats before shin dot
		{25, "ש\u05C1לום רחוב 12"},     // the presentation form decomposed
		{7, "חשבונית 26004295, 19057"},
		{27, "04 812 3456"},
	} {
		width := goldenSpans(t)[c.n].end - goldenSpans(t)[c.n].start + 1
		if got := field(t, rec, c.n); !bytes.Equal(got, padded(t, c.want, width)) {
			t.Errorf("F%d = %q, want %q", c.n, decode1255(t, got), c.want)
		}
	}
	for key, mutate := range map[string]func(*ReceiptRequest){
		"name":    func(r *ReceiptRequest) { r.Name = "مرحبا" },
		"address": func(r *ReceiptRequest) { r.Address = "حيفا" },
		"remarks": func(r *ReceiptRequest) { r.Remarks = "فاتورة 1" },
		"phone":   func(r *ReceiptRequest) { r.Phone = "٠٤" },
	} {
		r := goldenReceipt()
		mutate(&r)
		_, _, err := buildKUPAINRecord(r, goldenReceiptNumber, ReceiptCardNames{})
		wantFieldError(t, err, key)
	}
}

// What normalize1255 removes and what it refuses, as the doc says: ZWNJ, ZWJ,
// the BOM and the soft hyphen are removed; the zero-width SPACE (U+200B) and
// the Unicode line/paragraph separators (U+2028/U+2029) are not in its list and
// not in Windows-1255, so a value carrying them is a 400.
func TestKUPAINRecord_InvisibleCharactersStrippedOrRefused(t *testing.T) {
	for _, c := range []string{"\u200C", "\u200D", "\uFEFF", "\u00AD"} {
		r := goldenReceipt()
		r.Name = "לקוח" + c + " בדיקה בע\"מ"
		rec, _ := mustBuild(t, r)
		if got := field(t, rec, 24); !bytes.Equal(got, padded(t, `לקוח בדיקה בע"מ`, 50)) {
			t.Errorf("%+q: F24 = %q; want it removed", c, decode1255(t, got))
		}
	}
	for _, c := range []string{"\u200B", "\u2028", "\u2029"} {
		r := goldenReceipt()
		r.Name = "לקוח" + c + "בדיקה"
		_, _, err := buildKUPAINRecord(r, goldenReceiptNumber, ReceiptCardNames{})
		wantFieldError(t, err, "name")
	}
}
