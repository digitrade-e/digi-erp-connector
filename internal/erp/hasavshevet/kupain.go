package hasavshevet

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
)

// KUPAIN is Hashavshevet's receipt ("קבלה") import: KUPAIN_{n}.doc holds one
// fixed-width record per receipt and KUPAIN.prm describes its layout.
//
// Everything here reproduces the legacy BFL writer byte for byte
// (bfl_backend2 app/new-api/sync/global_api.php: getHerpRecieptConstantLengths
// :5207-5255, CreateRecieptPrmDoc :5257-5633, prepareForHerpDocReciept
// :5635-5685, WriteDocSpaces :5043-5071, createReceiptBat :5088-5152) with one
// deliberate difference: the legacy writer truncated and transliterated
// silently, this one refuses. A value that does not fit its field or cannot be
// written in Windows-1255 is an error, never a cut or a '?' — on a receipt for
// real money a quietly changed name or code is worse than no receipt.
// The golden files kupain_golden* are the legacy writer's own output.

// kupainFieldSpec is one KUPAIN field: the legacy field number, its width in
// Windows-1255 bytes and its PRM title. Width 0 fields take no room in the
// record and are described as "0 0" in the PRM.
type kupainFieldSpec struct {
	num   int
	width int
	title string
}

// kupainFields is the legacy layout in record order. The titles are copied
// from CreateRecieptPrmDoc, including their inconsistent number placement.
var kupainFields = []kupainFieldSpec{
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

// kupainRecordLength is the record length without its LF: every non-zero field
// plus the one space the legacy writer appends after each.
var kupainRecordLength = func() int {
	n := 0
	for _, f := range kupainFields {
		if f.width > 0 {
			n += f.width + 1
		}
	}
	return n
}()

const (
	// kupainMovementType is F4, the receipt's movement type (legacy constant).
	kupainMovementType = "33"
	// maxReceiptNumber is the largest number F3 (9 bytes) can hold.
	maxReceiptNumber = 999999999
	// maxAmountMinor keeps F12 within 9 bytes: 999999.99.
	maxAmountMinor = 99999999
	// receiptCardNameWidth is F17's width; receiptCardNames values must fit it.
	receiptCardNameWidth = 25
)

// ReceiptRequest is one receipt as POST /api/sendReceipt carries it (plan 005
// §4.6a). The JSON tags are the wire keys; the same shape is stored in the
// idempotency index so a reserved receipt can be finished later.
type ReceiptRequest struct {
	IdempotencyKey   string `json:"idempotencyKey"`
	AccountKey       string `json:"accountKey"`
	SettledDate      string `json:"settledDate"`
	AmountMinor      int64  `json:"amountMinor"`
	Installments     int    `json:"installments"`
	CardIssuer       string `json:"cardIssuer"`
	CardBrand        string `json:"cardBrand"`
	Last4            string `json:"last4"`
	ConfirmationCode string `json:"confirmationCode"`
	ProviderUID      string `json:"providerUid"`
	Remarks          string `json:"remarks"`
	AgentKey         string `json:"agentKey"`
	Name             string `json:"name"`
	Address          string `json:"address"`
	Phone            string `json:"phone"`
	Email            string `json:"email"`
}

// ReceiptFieldError names the request field that cannot become a receipt. The
// handler turns it into 400 VALIDATION_ERROR; Field is the wire key.
type ReceiptFieldError struct {
	Field  string
	Reason string
}

func (e *ReceiptFieldError) Error() string {
	return e.Field + ": " + e.Reason
}

// ErrReceiptConfig marks a receipt that cannot be built because of this
// installation's configuration (a receiptCardNames value), not the request.
var ErrReceiptConfig = errors.New("receipt configuration invalid")

var (
	// idempotencyKeyPattern is a canonical lower-case uuid. Upper case is refused
	// rather than folded: the key names an index file, and on Windows two keys
	// differing only in case would share it.
	idempotencyKeyPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	settledDatePattern    = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	cardCodePattern       = regexp.MustCompile(`^[0-9]{0,2}$`)
	last4Pattern          = regexp.MustCompile(`^[0-9]{4}$`)
)

// Validate reports the first field, in wire order, that cannot be written.
func (r ReceiptRequest) Validate() error {
	_, err := r.encodeFields()
	return err
}

// receiptText is a request value already encoded for its KUPAIN field.
type receiptText struct {
	account, date, remarks, amount, issuer, brand, last4 []byte
	confirmation, uid, agent, name, address, phone       []byte
	email                                                []byte
}

func (r ReceiptRequest) encodeFields() (receiptText, error) {
	var t receiptText
	bad := func(field, reason string) (receiptText, error) {
		return receiptText{}, &ReceiptFieldError{Field: field, Reason: reason}
	}

	if !idempotencyKeyPattern.MatchString(r.IdempotencyKey) {
		return bad("idempotencyKey", "must be a lower-case uuid")
	}

	var err error
	if r.AccountKey == "" {
		return bad("accountKey", "is required")
	}
	if t.account, err = encodeReceiptText("accountKey", r.AccountKey, 15); err != nil {
		return receiptText{}, err
	}

	if !settledDatePattern.MatchString(r.SettledDate) {
		return bad("settledDate", "must be YYYY-MM-DD")
	}
	settled, perr := time.Parse("2006-01-02", r.SettledDate)
	if perr != nil {
		return bad("settledDate", "is not a calendar date")
	}
	t.date = []byte(settled.Format("02/01/2006"))

	if r.AmountMinor < 1 || r.AmountMinor > maxAmountMinor {
		return bad("amountMinor", fmt.Sprintf("must be between 1 and %d", maxAmountMinor))
	}
	t.amount = []byte(fmt.Sprintf("%d.%02d", r.AmountMinor/100, r.AmountMinor%100))

	if r.Installments != 1 {
		return bad("installments", "must be 1")
	}

	if !cardCodePattern.MatchString(r.CardIssuer) {
		return bad("cardIssuer", "must be up to 2 digits")
	}
	if !cardCodePattern.MatchString(r.CardBrand) {
		return bad("cardBrand", "must be up to 2 digits")
	}
	if !last4Pattern.MatchString(r.Last4) {
		return bad("last4", "must be exactly 4 digits")
	}
	t.last4 = []byte(r.Last4)

	texts := []struct {
		field string
		value string
		width int
		dst   *[]byte
	}{
		{"confirmationCode", r.ConfirmationCode, 9, &t.confirmation},
		{"providerUid", r.ProviderUID, 250, &t.uid},
		{"remarks", r.Remarks, 250, &t.remarks},
		{"agentKey", r.AgentKey, 9, &t.agent},
		{"name", r.Name, 50, &t.name},
		{"address", r.Address, 50, &t.address},
		{"phone", r.Phone, 50, &t.phone},
		{"email", r.Email, 50, &t.email},
	}
	for _, x := range texts {
		if *x.dst, err = encodeReceiptText(x.field, x.value, x.width); err != nil {
			return receiptText{}, err
		}
	}
	return t, nil
}

// encodeReceiptText encodes one value for a field of the given width.
func encodeReceiptText(field, value string, width int) ([]byte, error) {
	b, err := encodeFieldValue(value)
	if err != nil {
		return nil, &ReceiptFieldError{Field: field, Reason: err.Error()}
	}
	if len(b) > width {
		return nil, &ReceiptFieldError{
			Field:  field,
			Reason: fmt.Sprintf("is %d bytes in Windows-1255; the limit is %d", len(b), width),
		}
	}
	return b, nil
}

// encodeFieldValue encodes a value bound for one fixed-width field. On top of
// encodeW1255Strict it refuses control characters: an LF or CR would end the
// one-line record early, and a tab or NUL has no business in a receipt.
func encodeFieldValue(s string) ([]byte, error) {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return nil, fmt.Errorf("contains control character U+%04X", r)
		}
	}
	return encodeW1255Strict(s)
}

// encodeW1255Strict encodes s to Windows-1255 and fails instead of
// substituting: on invalid UTF-8 and on a character the code page does not
// have. Unlike padW1255 (IMOVEIN), it never writes '?' and never truncates.
func encodeW1255Strict(s string) ([]byte, error) {
	out := make([]byte, 0, len(s))
	for i, r := range s {
		if r == utf8.RuneError {
			if _, size := utf8.DecodeRuneInString(s[i:]); size <= 1 {
				return nil, errors.New("is not valid UTF-8")
			}
		}
		b, ok := charmap.Windows1255.EncodeRune(r)
		if !ok {
			return nil, fmt.Errorf("character U+%04X is not in Windows-1255", r)
		}
		out = append(out, b)
	}
	return out, nil
}

// builtinIssuerCardNames and builtinBrandCardNames are the legacy F17 table
// (createReceiptBat :5097-5109), issuer first. The legacy fallback "JCB" for
// anything else is deliberately not kept: an unknown card leaves F17 blank and
// the response carries a warning, so a wrong name never reaches the books.
var (
	builtinIssuerCardNames = map[int]string{
		1: "ישראכרט",
		2: "ויזה",
		5: "אלפא קארד/לאומי קארד",
	}
	builtinBrandCardNames = map[int]string{
		4: "אמריקן אקספרס",
		3: "דיינרס",
	}
)

// receiptCardName resolves F17: the issuer, then the brand, each looked up in
// receiptCardNames ("issuer:N" / "brand:N") before the built-in table. Codes
// compare as numbers, as PHP's == did for the legacy strings ("01" is 1).
func receiptCardName(issuer, brand string, overrides map[string]string) (string, bool) {
	steps := []struct {
		kind    string
		code    string
		builtin map[int]string
	}{
		{"issuer", issuer, builtinIssuerCardNames},
		{"brand", brand, builtinBrandCardNames},
	}
	for _, s := range steps {
		n, err := strconv.Atoi(s.code)
		if err != nil {
			continue
		}
		if name, ok := overrides[s.kind+":"+strconv.Itoa(n)]; ok {
			return name, true
		}
		if name, ok := s.builtin[n]; ok {
			return name, true
		}
	}
	return "", false
}

// ValidateReceiptCardName checks one receiptCardNames value: a name Hashavshevet
// can store in F17.
func ValidateReceiptCardName(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("is blank")
	}
	b, err := encodeFieldValue(name)
	if err != nil {
		return err
	}
	if len(b) > receiptCardNameWidth {
		return fmt.Errorf("is %d bytes in Windows-1255; F17 holds %d", len(b), receiptCardNameWidth)
	}
	return nil
}

// BuildKUPAIN lays out receipt number n as one KUPAIN record (the .doc
// contents). cardNames is the receiptCardNames config. Warnings are
// operator-facing notes that do not stop the receipt, such as a blank F17.
//
// A *ReceiptFieldError means the request cannot be written; an error wrapping
// ErrReceiptConfig means this installation's card names cannot.
func BuildKUPAIN(n int64, req ReceiptRequest, cardNames map[string]string) ([]byte, []string, error) {
	if n < 1 || n > maxReceiptNumber {
		return nil, nil, fmt.Errorf("receipt number %d is outside 1..%d", n, maxReceiptNumber)
	}
	t, err := req.encodeFields()
	if err != nil {
		return nil, nil, err
	}

	var warnings []string
	var cardName []byte
	if name, ok := receiptCardName(req.CardIssuer, req.CardBrand, cardNames); ok {
		if verr := ValidateReceiptCardName(name); verr != nil {
			return nil, nil, fmt.Errorf("%w: receiptCardNames value for issuer %q / brand %q %v",
				ErrReceiptConfig, req.CardIssuer, req.CardBrand, verr)
		}
		cardName, _ = encodeFieldValue(name)
	} else {
		warnings = append(warnings, fmt.Sprintf(
			"no card name for cardIssuer %q / cardBrand %q: F17 left blank (add one under receiptCardNames)",
			req.CardIssuer, req.CardBrand))
	}

	values := map[int][]byte{
		2:  t.account,
		3:  []byte(strconv.FormatInt(n, 10)),
		4:  []byte(kupainMovementType),
		5:  t.date,
		6:  t.date,
		7:  t.remarks,
		12: t.amount,
		16: []byte(strconv.Itoa(req.Installments)),
		17: cardName,
		20: t.agent,
		22: t.confirmation,
		23: t.uid,
		24: t.name,
		25: t.address,
		27: t.phone,
		28: t.last4,
		42: t.email,
		43: t.confirmation,
	}

	doc := make([]byte, 0, kupainRecordLength+1)
	for _, f := range kupainFields {
		if f.width == 0 {
			continue
		}
		v := values[f.num]
		if len(v) > f.width {
			// Unreachable after encodeFields; kept so a layout edit cannot
			// silently overflow into the next field.
			return nil, nil, fmt.Errorf("F%d is %d bytes, wider than %d", f.num, len(v), f.width)
		}
		doc = append(doc, v...)
		for i := len(v); i <= f.width; i++ {
			doc = append(doc, ' ')
		}
	}
	doc = append(doc, '\n')
	return doc, warnings, nil
}

// kupainPrm is built once: it depends only on the layout.
var kupainPrm = func() []byte {
	var sb strings.Builder
	pos := 1
	for _, f := range kupainFields {
		if f.width == 0 {
			fmt.Fprintf(&sb, "0 0 ;%s\n", f.title)
			continue
		}
		end := pos + f.width - 1
		fmt.Fprintf(&sb, "%d %d ;%s\n", pos, end, f.title)
		pos = end + 2
	}
	text := fmt.Sprintf("%d ;%s\n", pos, "אורך רשומה - 1") + sb.String()
	b, err := encodeW1255Strict(text)
	if err != nil {
		panic("KUPAIN.prm titles must be Windows-1255: " + err.Error())
	}
	return b
}()

// KUPAINPrm returns the contents of KUPAIN.prm, the same for every receipt.
func KUPAINPrm() []byte {
	return append([]byte(nil), kupainPrm...)
}
