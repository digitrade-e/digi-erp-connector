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
	r.Address = "‏\nרחוב הדוגמה 12\n‏" // invisible marks around the breaks
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
	for _, blank := range []string{"\n", "\r\n", " \n\t", "‏\n", "\n‏‬"} {
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
