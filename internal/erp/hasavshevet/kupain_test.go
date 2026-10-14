package hasavshevet

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
)

// The kupain_golden* files were produced by running the legacy BFL writer
// (global_api.php createReceiptBat and the functions it calls) in
// backend-php:latest. docs/hasavshevet-send-receipt.md has the exact inputs and
// the command; the requests below must stay in step with them.

func goldenReceipt() ReceiptRequest {
	return ReceiptRequest{
		IdempotencyKey:   "0b6f2c1e-6a3d-4f7e-9c2a-5d8e1f4a7b30",
		AccountKey:       "30012",
		SettledDate:      "2026-10-14",
		AmountMinor:      693200,
		Installments:     1,
		CardIssuer:       "2",
		CardBrand:        "2",
		Last4:            "4580",
		ConfirmationCode: "0505293",
		ProviderUID:      "26101412000000012345",
		Remarks:          "26004295, 19057",
		AgentKey:         "12",
		Name:             `מרכז הבנייה בע"מ`,
		Address:          "רחוב הרצל 12, ראשון לציון",
		Phone:            "03-9512345",
		Email:            "office@binyan.co.il",
	}
}

// goldenMaxReceipt fills every field to exactly its width in Windows-1255
// bytes, so pad-to-width and the one trailing space are both pinned.
func goldenMaxReceipt() ReceiptRequest {
	return ReceiptRequest{
		IdempotencyKey:   "7f9d7c02-1c4b-4d0e-8a51-3e2b9c6d4f18",
		AccountKey:       "123456789012345",
		SettledDate:      "2026-12-31",
		AmountMinor:      99999999,
		Installments:     1,
		CardIssuer:       "5",
		CardBrand:        "1",
		Last4:            "0001",
		ConfirmationCode: "987654321",
		ProviderUID:      strings.Repeat("0123456789", 25),
		Remarks:          strings.Repeat("26004295, ", 25),
		AgentKey:         "123456789",
		Name:             strings.Repeat("אבגדה", 10),
		Address:          strings.Repeat("רחוב ", 10),
		Phone:            strings.Repeat("03-9512345", 5),
		Email:            strings.Repeat("a", 37) + "@binyan.co.il",
	}
}

// goldenAmexReceipt has no issuer, so the card name comes from the brand.
func goldenAmexReceipt() ReceiptRequest {
	return ReceiptRequest{
		IdempotencyKey:   "c3a1e4f0-2b7d-4c9a-b6e8-1d0f5a2c9e47",
		AccountKey:       "7",
		SettledDate:      "2027-01-01",
		AmountMinor:      1,
		Installments:     1,
		CardIssuer:       "",
		CardBrand:        "4",
		Last4:            "1234",
		ConfirmationCode: "1",
		ProviderUID:      "U",
		Name:             "א",
	}
}

func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return b
}

func TestKUPAINPrmMatchesLegacyWriter(t *testing.T) {
	want := readGolden(t, "kupain_golden.prm")
	if got := KUPAINPrm(); !bytes.Equal(got, want) {
		t.Fatalf("KUPAIN.prm differs from the legacy writer\n got: %q\nwant: %q", got, want)
	}
}

func TestKUPAINDocMatchesLegacyWriter(t *testing.T) {
	cases := []struct {
		golden string
		number int64
		req    ReceiptRequest
	}{
		{"kupain_golden.doc", 4217, goldenReceipt()},
		{"kupain_golden_max.doc", 999999999, goldenMaxReceipt()},
		{"kupain_golden_amex.doc", 1, goldenAmexReceipt()},
	}
	for _, c := range cases {
		t.Run(c.golden, func(t *testing.T) {
			got, warnings, err := BuildKUPAIN(c.number, c.req, nil)
			if err != nil {
				t.Fatalf("BuildKUPAIN: %v", err)
			}
			if len(warnings) != 0 {
				t.Errorf("warnings = %v, want none", warnings)
			}
			want := readGolden(t, c.golden)
			if !bytes.Equal(got, want) {
				t.Fatalf("doc differs from the legacy writer (len %d vs %d)\n got: %q\nwant: %q",
					len(got), len(want), got, want)
			}
		})
	}
}

// TestKUPAINDocRecordLength pins the layout itself: one record of the PRM's
// declared length, then LF.
func TestKUPAINDocRecordLength(t *testing.T) {
	doc, _, err := BuildKUPAIN(4217, goldenReceipt(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc) != kupainRecordLength+1 || doc[len(doc)-1] != '\n' {
		t.Fatalf("doc is %d bytes ending %q, want %d bytes + LF", len(doc), doc[len(doc)-1:], kupainRecordLength)
	}
	if bytes.Count(doc, []byte{'\n'}) != 1 {
		t.Fatal("doc must hold exactly one LF-terminated record")
	}
	if !bytes.HasPrefix(KUPAINPrm(), []byte("1260 ;")) || kupainRecordLength != 1259 {
		t.Fatalf("record length %d does not match the PRM header", kupainRecordLength)
	}
}

// The expected bytes are the legacy writer's own iconv output for each title.
func TestReceiptCardName(t *testing.T) {
	const (
		isracard = "e9f9f8e0ebf8e8"
		visa     = "e5e9e6e4"
		alpha    = "e0ecf4e020f7e0f8e32fece0e5eee920f7e0f8e3"
		amex     = "e0eef8e9f7ef20e0f7f1f4f8f1"
		diners   = "e3e9e9f0f8f1"
	)
	cases := []struct {
		name          string
		issuer, brand string
		overrides     map[string]string
		wantHex       string
		wantWarning   bool
	}{
		{"issuer 1 isracard", "1", "", nil, isracard, false},
		{"issuer 2 visa", "2", "", nil, visa, false},
		{"issuer 5 alpha/leumi", "5", "", nil, alpha, false},
		{"brand 4 amex", "", "4", nil, amex, false},
		{"brand 3 diners", "", "3", nil, diners, false},
		{"issuer wins over brand", "2", "4", nil, visa, false},
		{"unknown issuer falls back to brand", "9", "3", nil, diners, false},
		{"leading zero compares numerically like PHP ==", "01", "", nil, isracard, false},
		{"unknown → blank, never the legacy JCB", "7", "9", nil, "", true},
		{"nothing at all → blank", "", "", nil, "", true},
		{"issuer override", "7", "", map[string]string{"issuer:7": "מקס"}, "eef7f1", false},
		{"override beats the built-in table", "2", "", map[string]string{"issuer:2": "ויזה כאל"}, "e5e9e6e420ebe0ec", false},
		{"brand override after issuer table", "9", "6", map[string]string{"brand:6": "מקס"}, "eef7f1", false},
		{"issuer table beats a brand override", "1", "6", map[string]string{"brand:6": "מקס"}, isracard, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := goldenReceipt()
			req.CardIssuer, req.CardBrand = c.issuer, c.brand
			doc, warnings, err := BuildKUPAIN(1, req, c.overrides)
			if err != nil {
				t.Fatalf("BuildKUPAIN: %v", err)
			}
			want, _ := hex.DecodeString(c.wantHex)
			got := bytes.TrimRight(kupainField(doc, 17), " ")
			if !bytes.Equal(got, want) {
				t.Errorf("F17 = %x, want %x", got, want)
			}
			if (len(warnings) > 0) != c.wantWarning {
				t.Errorf("warnings = %v, want warning=%v", warnings, c.wantWarning)
			}
		})
	}
}

func TestBuildKUPAINRejectsBadCardNameConfig(t *testing.T) {
	for _, name := range []string{"😀", strings.Repeat("א", 26), "a\nb", ""} {
		_, _, err := BuildKUPAIN(1, goldenReceipt(), map[string]string{"issuer:2": name})
		if !errors.Is(err, ErrReceiptConfig) {
			t.Errorf("card name %q: err = %v, want ErrReceiptConfig", name, err)
		}
	}
}

// kupainField returns field n of a built doc, without its trailing separator.
func kupainField(doc []byte, n int) []byte {
	pos := 0
	for _, f := range kupainFields {
		if f.width == 0 {
			continue
		}
		if f.num == n {
			return doc[pos : pos+f.width]
		}
		pos += f.width + 1
	}
	return nil
}

func TestKUPAINFieldValues(t *testing.T) {
	doc, _, err := BuildKUPAIN(4217, goldenReceipt(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]string{
		2: "30012", 3: "4217", 4: "33", 5: "14/10/2026", 6: "14/10/2026", 7: "26004295, 19057",
		12: "6932.00", 16: "1", 20: "12", 22: "0505293", 23: "26101412000000012345",
		26: "", 27: "03-9512345", 28: "4580", 29: "", 42: "office@binyan.co.il", 43: "0505293",
	}
	for n, v := range want {
		if got := string(bytes.TrimRight(kupainField(doc, n), " ")); got != v {
			t.Errorf("F%d = %q, want %q", n, got, v)
		}
	}
	for _, c := range []struct {
		minor int64
		want  string
	}{{1, "0.01"}, {100, "1.00"}, {693205, "6932.05"}, {99999999, "999999.99"}} {
		req := goldenReceipt()
		req.AmountMinor = c.minor
		doc, _, err := BuildKUPAIN(1, req, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(bytes.TrimRight(kupainField(doc, 12), " ")); got != c.want {
			t.Errorf("amountMinor %d → F12 %q, want %q", c.minor, got, c.want)
		}
	}
}

func TestReceiptValidate(t *testing.T) {
	type mut func(*ReceiptRequest)
	cases := []struct {
		name  string
		field string
		m     mut
	}{
		{"key not a uuid", "idempotencyKey", func(r *ReceiptRequest) { r.IdempotencyKey = "abc" }},
		{"key upper case", "idempotencyKey", func(r *ReceiptRequest) { r.IdempotencyKey = strings.ToUpper(r.IdempotencyKey) }},
		{"key path", "idempotencyKey", func(r *ReceiptRequest) { r.IdempotencyKey = "../../0b6f2c1e-6a3d-4f7e-9c2a-5d8e1f4a7b3" }},
		{"account blank", "accountKey", func(r *ReceiptRequest) { r.AccountKey = "" }},
		{"account 16 bytes", "accountKey", func(r *ReceiptRequest) { r.AccountKey = "1234567890123456" }},
		{"date format", "settledDate", func(r *ReceiptRequest) { r.SettledDate = "14/10/2026" }},
		{"date not padded", "settledDate", func(r *ReceiptRequest) { r.SettledDate = "2026-1-4" }},
		{"date impossible", "settledDate", func(r *ReceiptRequest) { r.SettledDate = "2026-02-30" }},
		{"date with time", "settledDate", func(r *ReceiptRequest) { r.SettledDate = "2026-10-14T00:00:00Z" }},
		{"amount zero", "amountMinor", func(r *ReceiptRequest) { r.AmountMinor = 0 }},
		{"amount negative", "amountMinor", func(r *ReceiptRequest) { r.AmountMinor = -5 }},
		{"amount over", "amountMinor", func(r *ReceiptRequest) { r.AmountMinor = 100000000 }},
		{"installments 0", "installments", func(r *ReceiptRequest) { r.Installments = 0 }},
		{"installments 3", "installments", func(r *ReceiptRequest) { r.Installments = 3 }},
		{"issuer 3 digits", "cardIssuer", func(r *ReceiptRequest) { r.CardIssuer = "123" }},
		{"issuer letter", "cardIssuer", func(r *ReceiptRequest) { r.CardIssuer = "a" }},
		{"brand letter", "cardBrand", func(r *ReceiptRequest) { r.CardBrand = "V" }},
		{"brand arabic-indic digit", "cardBrand", func(r *ReceiptRequest) { r.CardBrand = "٤" }},
		{"last4 short", "last4", func(r *ReceiptRequest) { r.Last4 = "123" }},
		{"last4 long", "last4", func(r *ReceiptRequest) { r.Last4 = "12345" }},
		{"last4 letters", "last4", func(r *ReceiptRequest) { r.Last4 = "12a4" }},
		{"confirmation 10", "confirmationCode", func(r *ReceiptRequest) { r.ConfirmationCode = "0123456789" }},
		{"uid 251", "providerUid", func(r *ReceiptRequest) { r.ProviderUID = strings.Repeat("u", 251) }},
		{"remarks 251", "remarks", func(r *ReceiptRequest) { r.Remarks = strings.Repeat("א", 251) }},
		{"agent 10", "agentKey", func(r *ReceiptRequest) { r.AgentKey = "1234567890" }},
		{"name 51 hebrew bytes", "name", func(r *ReceiptRequest) { r.Name = strings.Repeat("א", 51) }},
		{"name emoji", "name", func(r *ReceiptRequest) { r.Name = "חברה 😀" }},
		{"name arabic", "name", func(r *ReceiptRequest) { r.Name = "شركة" }},
		{"name RLO", "name", func(r *ReceiptRequest) { r.Name = "‮חברה" }},
		{"name newline", "name", func(r *ReceiptRequest) { r.Name = "חברה\nבע\"מ" }},
		{"address CR", "address", func(r *ReceiptRequest) { r.Address = "רחוב\r1" }},
		{"phone tab", "phone", func(r *ReceiptRequest) { r.Phone = "03\t1" }},
		{"email NUL", "email", func(r *ReceiptRequest) { r.Email = "a\x00@b" }},
		{"remarks invalid utf-8", "remarks", func(r *ReceiptRequest) { r.Remarks = "a\xffb" }},
		{"remarks replacement char", "remarks", func(r *ReceiptRequest) { r.Remarks = "a�b" }},
		{"email 51", "email", func(r *ReceiptRequest) { r.Email = strings.Repeat("a", 38) + "@binyan.co.il" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := goldenReceipt()
			c.m(&req)
			err := req.Validate()
			var fe *ReceiptFieldError
			if !errors.As(err, &fe) {
				t.Fatalf("Validate() = %v, want a *ReceiptFieldError", err)
			}
			if fe.Field != c.field {
				t.Errorf("field = %q, want %q (%v)", fe.Field, c.field, err)
			}
			// The builder refuses the same value: nothing is ever truncated.
			if _, _, err := BuildKUPAIN(1, req, nil); !errors.As(err, &fe) {
				t.Errorf("BuildKUPAIN accepted what Validate refused: %v", err)
			}
		})
	}
}

func TestReceiptValidateAcceptsLimits(t *testing.T) {
	for _, r := range []ReceiptRequest{goldenReceipt(), goldenMaxReceipt(), goldenAmexReceipt()} {
		if err := r.Validate(); err != nil {
			t.Errorf("Validate(%s) = %v", r.IdempotencyKey, err)
		}
	}
	// Limits are Windows-1255 bytes, not UTF-8 bytes: 50 Hebrew letters are 100
	// UTF-8 bytes and still fit F24. LRM/RLM are part of the code page.
	req := goldenReceipt()
	req.Name = strings.Repeat("ש", 50)
	req.Address = "‏רחוב‎ 1"
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestBuildKUPAINRejectsBadNumber(t *testing.T) {
	for _, n := range []int64{0, -1, 1000000000} {
		if _, _, err := BuildKUPAIN(n, goldenReceipt(), nil); err == nil {
			t.Errorf("BuildKUPAIN(%d) succeeded", n)
		}
	}
}
