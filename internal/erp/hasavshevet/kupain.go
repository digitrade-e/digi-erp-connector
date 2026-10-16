package hasavshevet

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// KUPAIN is Hashavshevet's receipt import ("קבלות"): one fixed-width record per
// receipt in KUPAIN_{n}.doc, positions declared in KUPAIN.prm.
//
// The format is reproduced from the legacy BFL writer, which produced these
// files in production for years: bfl_app backend/app/new-api/sync/global_api.php,
// getHerpRecieptConstantLengths (widths), CreateRecieptPrmDoc (titles and
// positions) and prepareForHerpDocReciept / WriteDocSpaces (the record). Its
// rules, kept byte for byte:
//
//   - every written field is padded to its width and followed by ONE space, so
//     field n+1 starts two positions after field n ends (F2 is 1-15, F3 17-25);
//   - fields 30-41 have width 0: declared "0 0" in the .prm, absent from the
//     record;
//   - the record is 1259 bytes plus LF; .prm line 1 is "1260 ;…" — the
//     position after the last separator, which is what the legacy wrote;
//   - LF line endings everywhere, Windows-1255.
//
// What is NOT legacy, by decision (plan 056 §4.6a / §5.5.3 C1): values that do
// not fit or cannot be encoded are refused (the legacy truncated and
// transliterated), F3 is the connector's receipt number (the legacy used its
// own row id), and an unknown card code leaves F17 blank with a warning (the
// legacy wrote "JCB"; see the card-name table below for why that fallback is
// rejected). F7 is the backend's remarks as sent — the backend builds the
// invoice list and its cut, the connector only writes it. F4 "33", F26 (city,
// blank) and F29 (first payment, blank) are the connector's, as the legacy
// wrote them.

// FieldError names the wire field whose value cannot be written. The route
// answers it as 400 VALIDATION_ERROR with details.field = Field.
type FieldError struct {
	Field  string // wire path, e.g. "account/name"
	Reason string
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Reason }

// ReceiptRequest is the sendReceipt wire body (plan 056 §4.6a): flat, exactly
// the keys the backend's Receipt::jsonSerialize() emits. Amounts are integer
// agorot. The JSON tags are the wire keys, and the same shape is stored in the
// idempotency index so a reserved receipt can be finished later.
//
// IdempotencyKey is not written to the record; the writer and the index use it.
type ReceiptRequest struct {
	IdempotencyKey   string `json:"idempotencyKey"`
	AccountKey       string `json:"accountKey"`  // F2
	SettledDate      string `json:"settledDate"` // F5, F6: YYYY-MM-DD, the Asia/Jerusalem day
	AmountMinor      int64  `json:"amountMinor"` // F12
	Installments     int    `json:"installments"`
	CardIssuer       string `json:"cardIssuer"`       // F17, first: raw HYP Issuer, ^\d{0,2}$
	CardBrand        string `json:"cardBrand"`        // F17, else: raw HYP Brand, ^\d{0,2}$
	Last4            string `json:"last4"`            // F28
	ConfirmationCode string `json:"confirmationCode"` // F22 and F43: HYP ACode
	ProviderUID      string `json:"providerUid"`      // F23: HYP UID
	Remarks          string `json:"remarks"`          // F7: the invoices paid, built by the backend
	AgentKey         string `json:"agentKey"`         // F20
	Name             string `json:"name"`             // F24
	Address          string `json:"address"`          // F25
	Phone            string `json:"phone"`            // F27
	Email            string `json:"email"`            // F42
}

// kupainField is one line of KUPAIN.prm. Width 0 means declared but absent.
type kupainField struct {
	num   int
	width int
	title string // verbatim from the legacy writer, including its odd ones
}

var kupainFields = []kupainField{
	{2, 15, "מפתח לקוח - 2"},
	{3, 9, "מספר קבלה - 3"},
	{4, 3, "סוג תנועה - 4"},
	{5, 10, "תאריך הקבלה - 5"},
	{6, 10, "תאריך ערך (פרעון) - 6"},
	{7, 250, "פרטים - 7"},
	{8, 9, "מספר שק - 8"},
	{9, 3, "מספר בנק - 9"},
	{10, 5, "מספר סניף - 10"},
	{11, 9, "מספר חשבון בנק - 11"},
	{12, 9, "סכום בשקלים - 12"},
	{13, 9, "סכום מטח - 13"},
	{14, 5, "מטבע מוביל בקבלה - 14"},
	{15, 5, "מטבע שערוך בתנועה - 15"},
	{16, 2, "מספר תשלומים באשראי - 16"},
	{17, 25, "שם כרטיס האשראי - 17"},
	{18, 11, "תעודת זהות - 18"},
	{19, 250, "פרטים במסמך - 19"},
	{20, 9, "סוכן - 20"},
	{21, 9, "סניף - 21"},
	{22, 9, "אסמכתא נגדית - 22"},
	{23, 250, "הערות - 23"},
	{24, 50, "שם חשבון - 24"},
	{25, 50, "כתובת - 25"},
	{26, 50, "עיר - 26"},
	{27, 50, "טלפון - 27"},
	{28, 4, "ארבע ספרות - 28"},
	{29, 9, "תשלום ראשון - 29"},
	{30, 0, "הערה נוספת 1 - 30"},
	{31, 0, "הערה נוספת 2 - 31"},
	{32, 0, "32 - הערה נוספת 3"},
	{33, 0, "הערה נוספת 4 - 33"},
	{34, 0, "הערה נוספת 5 - 34"},
	{35, 0, "סכום נוסף 1 - 35"},
	{36, 0, "סכום נוסף 2 - 36"},
	{37, 0, "סכום נוסף 3 - 37"},
	{38, 0, "38- תאריך נוסף 1"},
	{39, 0, "תאריך נוסף 2 - 39"},
	{40, 0, "40- מספר נוסף 1"},
	{41, 0, "מספר נוסף 2 - 41"},
	{42, 50, "42 - כתובת מייל"},
	{43, 50, "43 - מספר שובר"},
}

const (
	kupainRecordLength = 1259 // sum of widths + one separator per written field
	kupainReceiptType  = "33" // F4, as the legacy wrote for card receipts
	maxAmountMinor     = 99999999
	maxReceiptNumber   = 999999999 // F3 is 9 digits
)

func kupainWidth(num int) int {
	for _, f := range kupainFields {
		if f.num == num {
			return f.width
		}
	}
	panic(fmt.Sprintf("kupain: no field %d", num))
}

// generateKUPAINPRM returns KUPAIN.prm exactly as the legacy writer produced
// it: "<record length> ;אורך רשומה - 1", then "<start> <end> ;<title>" per
// field ("0 0" for width 0), LF-terminated, Windows-1255.
func generateKUPAINPRM() []byte {
	lines := make([]string, 0, len(kupainFields)+1)
	pos := 1
	for _, f := range kupainFields {
		if f.width == 0 {
			lines = append(lines, "0 0 ;"+f.title)
			continue
		}
		end := pos + f.width - 1
		lines = append(lines, fmt.Sprintf("%d %d ;%s", pos, end, f.title))
		pos = end + 2 // one separator space
	}
	lines = append([]string{fmt.Sprintf("%d ;אורך רשומה - 1", pos)}, lines...)

	var prm []byte
	for _, line := range lines {
		b, err := encode1255Strict(line)
		if err != nil {
			panic("kupain: .prm title is not Windows-1255: " + err.Error()) // constant table; covered by tests
		}
		prm = append(append(prm, b...), '\n')
	}
	return prm
}

// generateKUPAINDOC returns the content of KUPAIN_{n}.doc: one record + LF.
func generateKUPAINDOC(req ReceiptRequest, receiptNumber int64, cardNames ReceiptCardNames) ([]byte, []string, error) {
	rec, warnings, err := buildKUPAINRecord(req, receiptNumber, cardNames)
	if err != nil {
		return nil, warnings, err
	}
	return append(rec, '\n'), warnings, nil
}

// buildKUPAINRecord builds the 1259-byte record for one receipt. It never
// truncates and never substitutes: any value that does not fit its field or
// cannot be written in Windows-1255 is a *FieldError naming the wire key.
// An unusable card-name override is a *CardNamesConfigError instead.
//
// The second result is the receipt's warnings — things written blank that a
// person may want to know about (today: F17 when no card code has a name).
// They are part of the result, not just a log line: the route returns them in
// its 200 body as "warnings".
func buildKUPAINRecord(req ReceiptRequest, receiptNumber int64, cardNames ReceiptCardNames) ([]byte, []string, error) {
	var warnings []string
	vals := make(map[int][]byte, len(kupainFields))
	// put writes one request value. What surrounds it — whitespace, line
	// breaks, invisible marks — is trimmed from the RAW value first (trimEdges),
	// then normalize1255 runs (inside encodeField): normalising first would turn
	// a trailing "\n" into a written ". ".
	put := func(num int, wire, value string) error {
		b, err := encodeField(wire, trimEdges(value), kupainWidth(num))
		if err != nil {
			return err
		}
		vals[num] = b
		return nil
	}
	required := func(wire, value string) error {
		if strings.TrimSpace(normalize1255(trimEdges(value))) == "" {
			return &FieldError{Field: wire, Reason: "is required"}
		}
		return nil
	}

	// F2 customer key
	if err := required("accountKey", req.AccountKey); err != nil {
		return nil, nil, err
	}
	if err := put(2, "accountKey", req.AccountKey); err != nil {
		return nil, nil, err
	}

	// F3 receipt number, F4 type
	if receiptNumber < 1 || receiptNumber > maxReceiptNumber {
		return nil, nil, &FieldError{Field: "receiptNumber", Reason: fmt.Sprintf("must be 1-%d, got %d", maxReceiptNumber, receiptNumber)}
	}
	if err := put(3, "receiptNumber", strconv.FormatInt(receiptNumber, 10)); err != nil {
		return nil, nil, err
	}
	if err := put(4, "receiptType", kupainReceiptType); err != nil {
		return nil, nil, err
	}

	// F5 receipt date, F6 value date: the settlement day, dd/mm/YYYY
	day, err := time.Parse("2006-01-02", req.SettledDate)
	if err != nil {
		return nil, nil, &FieldError{Field: "settledDate", Reason: fmt.Sprintf("must be a YYYY-MM-DD date, got %q", req.SettledDate)}
	}
	if err := put(5, "settledDate", day.Format("02/01/2006")); err != nil {
		return nil, nil, err
	}
	vals[6] = vals[5]

	// F7 details: the invoices paid, as the backend built them
	if err := required("remarks", req.Remarks); err != nil {
		return nil, nil, err
	}
	if err := put(7, "remarks", req.Remarks); err != nil {
		return nil, nil, err
	}

	// F12 amount, F16 installments. F29 (first payment) stays blank: one
	// installment only, so there is nothing to split.
	amount, err := formatAmount(req.AmountMinor)
	if err != nil {
		return nil, nil, err
	}
	if err := put(12, "amountMinor", amount); err != nil {
		return nil, nil, err
	}
	if req.Installments != 1 {
		return nil, nil, &FieldError{Field: "installments", Reason: fmt.Sprintf("must be 1, got %d", req.Installments)}
	}
	if err := put(16, "installments", "1"); err != nil {
		return nil, nil, err
	}

	// F17 card name, from the issuer or else the brand
	if !isCardCode(req.CardIssuer) {
		return nil, nil, &FieldError{Field: "cardIssuer", Reason: fmt.Sprintf("must be 0-2 digits, got %q", req.CardIssuer)}
	}
	if !isCardCode(req.CardBrand) {
		return nil, nil, &FieldError{Field: "cardBrand", Reason: fmt.Sprintf("must be 0-2 digits, got %q", req.CardBrand)}
	}
	name, from, warning := cardName(req.CardIssuer, req.CardBrand, cardNames)
	if warning != "" {
		warnings = append(warnings, warning)
	}
	if err := put(17, from, name); err != nil {
		// The name came from the operator's override table, not the request:
		// a bad one is a configuration fault, never a 400 blaming the caller.
		// ValidateReceiptCardNames is meant to stop it at config load.
		reason := err.Error()
		var fe *FieldError
		if errors.As(err, &fe) {
			reason = fe.Reason // never wrap the FieldError: errors.As must not find one
		}
		return nil, nil, &CardNamesConfigError{Table: from, Name: name, Reason: reason}
	}

	// F20 agent, F22 confirmation code, F23 provider UID
	if err := put(20, "agentKey", req.AgentKey); err != nil {
		return nil, nil, err
	}
	if err := required("confirmationCode", req.ConfirmationCode); err != nil {
		return nil, nil, err
	}
	if err := put(22, "confirmationCode", req.ConfirmationCode); err != nil {
		return nil, nil, err
	}
	if err := put(23, "providerUid", req.ProviderUID); err != nil {
		return nil, nil, err
	}

	// F24, F25, F27 account; F26 (city) stays blank
	if err := required("name", req.Name); err != nil {
		return nil, nil, err
	}
	for _, f := range []struct {
		num         int
		wire, value string
	}{
		{24, "name", req.Name},
		{25, "address", req.Address},
		{27, "phone", req.Phone},
	} {
		if err := put(f.num, f.wire, f.value); err != nil {
			return nil, nil, err
		}
	}

	// F28 last four digits
	if !isFourASCIIDigits(req.Last4) {
		return nil, nil, &FieldError{Field: "last4", Reason: fmt.Sprintf("must be exactly 4 digits, got %q", req.Last4)}
	}
	if err := put(28, "last4", req.Last4); err != nil {
		return nil, nil, err
	}

	// F42 email, F43 voucher number (the confirmation code again, as the legacy did)
	if err := put(42, "email", req.Email); err != nil {
		return nil, nil, err
	}
	if err := put(43, "confirmationCode", req.ConfirmationCode); err != nil {
		return nil, nil, err
	}

	rec := make([]byte, 0, kupainRecordLength)
	for _, f := range kupainFields {
		if f.width == 0 {
			continue
		}
		v := vals[f.num] // unset fields (8-11, 13-15, 18, 19, 21, 26, 29) are blank
		rec = append(rec, v...)
		rec = append(rec, bytes.Repeat([]byte{' '}, f.width-len(v)+1)...)
	}
	if len(rec) != kupainRecordLength {
		panic(fmt.Sprintf("kupain: record is %d bytes, want %d", len(rec), kupainRecordLength)) // table bug; covered by tests
	}
	return rec, warnings, nil
}

// formatAmount renders agorot as shekels with two decimals: 710700 → "7107.00",
// 5 → "0.05". The range is 1…99 999 999, which is what F12's 9 bytes hold.
func formatAmount(minor int64) (string, error) {
	if minor < 1 || minor > maxAmountMinor {
		return "", &FieldError{Field: "amountMinor", Reason: fmt.Sprintf("must be 1-%d agorot, got %d", maxAmountMinor, minor)}
	}
	return fmt.Sprintf("%d.%02d", minor/100, minor%100), nil
}

// isCardCode is the wire's ^\d{0,2}$ for cardIssuer and cardBrand: ASCII
// digits only, no spaces.
func isCardCode(s string) bool {
	if len(s) > 2 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isFourASCIIDigits(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// ReceiptCardNames overrides the built-in card-name table, keyed by the raw
// HYP code as a string. The zero value means "built-ins only". An override
// replaces the built-in for that code and leaves the others alone; an empty
// name means "no override". Check a configured table with
// ValidateReceiptCardNames when it is loaded.
type ReceiptCardNames struct {
	Issuer map[string]string
	Brand  map[string]string
}

// CardNamesConfigError reports a card-name override that cannot be written in
// F17. The operator's configuration is at fault, not the request.
type CardNamesConfigError struct {
	Table  string // "cardIssuer" or "cardBrand": which table the name came from
	Name   string
	Reason string
}

func (e *CardNamesConfigError) Error() string {
	return fmt.Sprintf("receipt card-name override %q (%s table) cannot be written in F17: %s", e.Name, e.Table, e.Reason)
}

// ValidateReceiptCardNames checks a configured override table: every code is
// non-blank, and every name can be written in Windows-1255 within F17's width.
func ValidateReceiptCardNames(names ReceiptCardNames) error {
	width := kupainWidth(17)
	for _, table := range []struct {
		label string
		m     map[string]string
	}{{"issuer", names.Issuer}, {"brand", names.Brand}} {
		for code, name := range table.m {
			if strings.TrimSpace(code) == "" {
				return fmt.Errorf("receipt card names: blank %s code", table.label)
			}
			if name == "" {
				continue
			}
			if _, err := encodeField("card name", name, width); err != nil {
				return fmt.Errorf("receipt card names: %s %q: %w", table.label, code, err)
			}
		}
	}
	return nil
}

// Built-in card names: exactly the legacy createReceiptBat table
// (global_api.php:5338-5348), which looked at the HYP Issuer first (stored as
// CredType) and only then at the Brand (stored as Tranmode). No brand code the
// legacy did not name is added here.
//
// The legacy's fallback — "JCB" for every card neither table named — is
// deliberately NOT kept: it printed a false card name on every receipt whose
// issuer it did not know (a Visa or Mastercard brand with no known issuer, for
// one), and a wrong name on a legal document is worse than a blank one plus a
// warning the caller gets back.
var (
	defaultIssuerCardNames = map[string]string{
		"1": "ישראכרט",
		"2": "ויזה",
		"5": "אלפא קארד/לאומי קארד",
	}
	defaultBrandCardNames = map[string]string{
		"4": "אמריקן אקספרס",
		"3": "דיינרס",
	}
)

// lookupCardName finds a code's name, the override first. Codes compare as
// numbers, as the legacy's PHP == did for these numeric strings: "02" is 2,
// whether it is the request's code or an override's key.
func lookupCardName(code string, override, builtin map[string]string) string {
	code = canonicalCardCode(code)
	if code == "" {
		return ""
	}
	for k, v := range override {
		if v != "" && canonicalCardCode(k) == code {
			return v
		}
	}
	return builtin[code]
}

// canonicalCardCode is a card code without surrounding space or leading
// zeros ("02" → "2", "00" → "0"); text that is not a number stays as it is.
func canonicalCardCode(code string) string {
	code = strings.TrimSpace(code)
	if n, err := strconv.Atoi(code); err == nil && n >= 0 {
		return strconv.Itoa(n)
	}
	return code
}

// cardName returns the F17 card name for an issuer/brand pair — issuer first,
// then brand, as the legacy did — with the wire field it came from and, when
// neither code has a name, a warning (F17 is then left blank).
func cardName(issuer, brand string, names ReceiptCardNames) (name, from, warning string) {
	issuer, brand = strings.TrimSpace(issuer), strings.TrimSpace(brand)
	if n := lookupCardName(issuer, names.Issuer, defaultIssuerCardNames); n != "" {
		return n, "cardIssuer", ""
	}
	if n := lookupCardName(brand, names.Brand, defaultBrandCardNames); n != "" {
		return n, "cardBrand", ""
	}
	if issuer == "" && brand == "" {
		return "", "cardBrand", "no card issuer or brand given; card name (F17) left blank"
	}
	return "", "cardBrand", fmt.Sprintf("no card name for issuer %q / brand %q; card name (F17) left blank", issuer, brand)
}
