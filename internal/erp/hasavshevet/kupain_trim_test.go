package hasavshevet

import (
	"bytes"
	"testing"
)

// Surrounding whitespace — line breaks included — is trimmed BEFORE
// normalize1255, which would otherwise turn a trailing "\n" into a written
// ". " (F2 "ab." for "ab\n"), and let a value that is nothing but a line
// break pass as present.
func TestKUPAINRecord_SurroundingLineBreaksAreTrimmedNotWritten(t *testing.T) {
	r := goldenReceipt()
	r.AccountKey = "100234\n"
	r.ConfirmationCode = "\r\n0123456\r\n"
	r.AgentKey = "\t17\n"
	r.Name = "\n" + `לקוח בדיקה בע"מ` + "\r\n"
	r.Remarks = "INV-1001, INV-1002 | 2824019\n"
	r.Address = "\u200F\nרחוב הדוגמה 12\n\u200F" // invisible marks around the breaks
	r.Phone = "04-0000000 \n"
	r.Email = "\ttest@example.com\n"
	r.ProviderUID = "26101112345678901234567\n"
	rec, _ := mustBuild(t, r)
	want := readGolden(t, "KUPAIN_redacted.doc")[:1259]
	if !bytes.Equal(rec, want) {
		t.Fatalf("record differs from the golden once trimmed\n got: %q\nwant: %q", decode1255(t, rec), decode1255(t, want))
	}
}

func TestKUPAINRecord_ALineBreakAloneIsNotAValue(t *testing.T) {
	for _, blank := range []string{"\n", "\r\n", " \n\t", "\u200F\n", "\n\u200F\u202C"} {
		for key, mutate := range map[string]func(*ReceiptRequest, string){
			"accountKey":       func(r *ReceiptRequest, v string) { r.AccountKey = v },
			"confirmationCode": func(r *ReceiptRequest, v string) { r.ConfirmationCode = v },
			"name":             func(r *ReceiptRequest, v string) { r.Name = v },
			"remarks":          func(r *ReceiptRequest, v string) { r.Remarks = v },
		} {
			r := goldenReceipt()
			mutate(&r, blank)
			_, _, err := buildKUPAINRecord(r, goldenReceiptNumber, ReceiptCardNames{})
			wantFieldError(t, err, key)
		}
	}
}

// Pinned byte change (round 1, trim before normalize1255): a leading
// no-break space or a leading "' " used to be written as a leading space in
// the free-text fields (normalize1255 makes U+00A0 a space and drops the
// apostrophe, and only identifiers were trimmed). Now every value's edges
// are trimmed, so the field holds the value alone.
func TestKUPAINRecord_LeadingNoBreakSpaceAndApostropheAreDropped(t *testing.T) {
	fields := []struct {
		num  int
		set  func(*ReceiptRequest, string)
		want string
	}{
		{7, func(r *ReceiptRequest, v string) { r.Remarks = v }, "INV-1001, INV-1002 | 2824019"},
		{23, func(r *ReceiptRequest, v string) { r.ProviderUID = v }, "26101112345678901234567"},
		{24, func(r *ReceiptRequest, v string) { r.Name = v }, `לקוח בדיקה בע"מ`},
		{25, func(r *ReceiptRequest, v string) { r.Address = v }, "רחוב הדוגמה 12"},
		{27, func(r *ReceiptRequest, v string) { r.Phone = v }, "04-0000000"},
		{42, func(r *ReceiptRequest, v string) { r.Email = v }, "test@example.com"},
	}
	for _, f := range fields {
		for _, lead := range []string{"\u00A0", "' "} {
			r := goldenReceipt()
			f.set(&r, lead+f.want)
			rec, _ := mustBuild(t, r)
			width := goldenSpans(t)[f.num].end - goldenSpans(t)[f.num].start + 1
			if got := field(t, rec, f.num); !bytes.Equal(got, padded(t, f.want, width)) {
				t.Errorf("F%d with leading %+q = %q; want %q", f.num, lead, decode1255(t, got), f.want)
			}
		}
	}
}
