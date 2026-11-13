package hasavshevet

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Header fields (DIG-1315, plan 075 § P3-4). The order's own header details —
// a delivery name, address, city and phone, the PO number, the delivery flags
// — are per-tenant order fields that erp-manager maps onto the Hashavshevet
// document header and sends as the request's optional "header" object. The
// connector knows only the Hashavshevet fields, never what a tenant puts in
// them (BFL's mapping values 99 / 1 / 18 / 17 live in its erp-manager config).
//
//	accountNameDetail  line5   appended to the account name: "<name>|<detail>"
//	address            line6   replaces the account's address
//	city               line7   replaces the account's city
//	phone              line35  replaces the account's phone
//	extraText1..5      line45..49
//	extraSum1..3       line50..52
//	extraNum1..2       line81..82
//
// A field left out, null or blank is written exactly as today (the account's
// value, or blank for the extras), so a request without "header" writes the
// same bytes it always did. A value is refused when it does not fit its field,
// never cut: padW1255 would truncate it silently.

// headerKind is how a header value is checked and written.
type headerKind int

const (
	headerText   headerKind = iota // free text, one Windows-1255 byte per character
	headerNumber                   // a decimal number, written as sent
)

// headerField is one key of the request's "header" object.
type headerField struct {
	Name string // wire key, case-sensitive
	Line int    // IMOVEIN line it is written to
	Kind headerKind
	get  func(*OrderHeader) *string
}

// OrderHeader is the request's "header" object in the form the writer takes it:
// each value as sent (a number as its JSON text), "" = absent. omitempty keeps
// the order's request hash (DIG-1291) independent of fields not sent.
type OrderHeader struct {
	AccountNameDetail string `json:",omitempty"`
	Address           string `json:",omitempty"`
	City              string `json:",omitempty"`
	Phone             string `json:",omitempty"`
	ExtraText1        string `json:",omitempty"`
	ExtraText2        string `json:",omitempty"`
	ExtraText3        string `json:",omitempty"`
	ExtraText4        string `json:",omitempty"`
	ExtraText5        string `json:",omitempty"`
	ExtraSum1         string `json:",omitempty"`
	ExtraSum2         string `json:",omitempty"`
	ExtraSum3         string `json:",omitempty"`
	ExtraNum1         string `json:",omitempty"`
	ExtraNum2         string `json:",omitempty"`
}

// headerFields is every header key, in the order they are echoed.
var headerFields = []headerField{
	{"accountNameDetail", 5, headerText, func(h *OrderHeader) *string { return &h.AccountNameDetail }},
	{"address", 6, headerText, func(h *OrderHeader) *string { return &h.Address }},
	{"city", 7, headerText, func(h *OrderHeader) *string { return &h.City }},
	{"phone", 35, headerText, func(h *OrderHeader) *string { return &h.Phone }},
	{"extraText1", 45, headerText, func(h *OrderHeader) *string { return &h.ExtraText1 }},
	{"extraText2", 46, headerText, func(h *OrderHeader) *string { return &h.ExtraText2 }},
	{"extraText3", 47, headerText, func(h *OrderHeader) *string { return &h.ExtraText3 }},
	{"extraText4", 48, headerText, func(h *OrderHeader) *string { return &h.ExtraText4 }},
	{"extraText5", 49, headerText, func(h *OrderHeader) *string { return &h.ExtraText5 }},
	{"extraSum1", 50, headerNumber, func(h *OrderHeader) *string { return &h.ExtraSum1 }},
	{"extraSum2", 51, headerNumber, func(h *OrderHeader) *string { return &h.ExtraSum2 }},
	{"extraSum3", 52, headerNumber, func(h *OrderHeader) *string { return &h.ExtraSum3 }},
	{"extraNum1", 81, headerNumber, func(h *OrderHeader) *string { return &h.ExtraNum1 }},
	{"extraNum2", 82, headerNumber, func(h *OrderHeader) *string { return &h.ExtraNum2 }},
}

// accountNameSeparator joins the account name and accountNameDetail, as the
// legacy writer did (AccountName = name + "|" + CustName).
const accountNameSeparator = "|"

// HeaderFieldNames is every key the "header" object may carry, in echo order.
func HeaderFieldNames() []string {
	names := make([]string, len(headerFields))
	for i, f := range headerFields {
		names[i] = f.Name
	}
	return names
}

// IsHeaderNumberField reports whether name is a header key whose value is a
// number (extraSum*, extraNum*); false for a text key or an unknown one.
func IsHeaderNumberField(name string) bool {
	for _, f := range headerFields {
		if f.Name == name {
			return f.Kind == headerNumber
		}
	}
	return false
}

// SetHeaderField stores value under the header key name. It reports false for
// a key that is not a header field.
func (h *OrderHeader) SetHeaderField(name, value string) bool {
	for _, f := range headerFields {
		if f.Name == name {
			*f.get(h) = value
			return true
		}
	}
	return false
}

// Applied is the wire names of the fields h carries, in echo order: what the
// connector writes into the document header. Empty for nil.
func (h *OrderHeader) Applied() []string {
	names := []string{}
	if h == nil {
		return names
	}
	for _, f := range headerFields {
		if *f.get(h) != "" {
			names = append(names, f.Name)
		}
	}
	return names
}

// IsEmpty reports whether h carries no field.
func (h *OrderHeader) IsEmpty() bool { return h == nil || len(h.Applied()) == 0 }

// headerFieldPath is the FieldError path of a header key.
func headerFieldPath(name string) string { return "header/" + name }

// headerTextValue is a header text as it is written: line breaks (CR LF, CR,
// LF) become ". " and single quotes go, like every other free text of the
// record (sanitizeField). Edge whitespace is kept, as for the account's own
// fields.
func headerTextValue(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return sanitizeField(s)
}

// headerTextWidth is how many characters of a header text fit its field in the
// official layout: the field's width, less the separator for accountNameDetail.
func headerTextWidth(f headerField) int {
	w := imoveInFieldLengths[fmt.Sprintf("line%d", f.Line)]
	if f.Name == "accountNameDetail" {
		w -= utf8.RuneCountInString(accountNameSeparator)
	}
	return w
}

// ValidateHeader checks the header fields of req (nil = none) against the
// official IMOVEIN widths. It returns a *FieldError naming "header/<key>", or
// nil. The route calls it before anything is reserved; the writer calls it
// again for every other caller. ValidateLegacyFit adds the narrower legacy
// widths of the numbers.
//
// Widths are counted as written: one Windows-1255 byte per character, after
// line breaks became ". " — a character Windows-1255 cannot hold is written
// "?", as in every other text of the record. A text with any other control
// character (a tab, NUL…) is refused: it would sit inside a fixed-width record.
func ValidateHeader(h *OrderHeader) error {
	if h == nil {
		return nil
	}
	for _, f := range headerFields {
		v := *f.get(h)
		if v == "" {
			continue
		}
		switch f.Kind {
		case headerText:
			w := headerTextWidth(f)
			if strings.TrimSpace(v) == "" {
				return &FieldError{Field: headerFieldPath(f.Name), Reason: "must not be only spaces (leave it out instead)"}
			}
			if hasControl(strings.NewReplacer("\r", "", "\n", "").Replace(v)) {
				return &FieldError{Field: headerFieldPath(f.Name), Reason: "must not contain control characters (tab, NUL…)"}
			}
			if n := utf8.RuneCountInString(headerTextValue(v)); n > w {
				return &FieldError{Field: headerFieldPath(f.Name), Reason: fmt.Sprintf(
					"is %d characters; IMOVEIN field %d holds %d (line breaks count as two)", n, f.Line, w)}
			}
		case headerNumber:
			w := imoveInFieldLengths[fmt.Sprintf("line%d", f.Line)]
			if !isHeaderNumber(v) {
				return &FieldError{Field: headerFieldPath(f.Name), Reason: "must be a decimal number such as 99, 1 or 12.50 (digits, an optional leading minus and decimal point; no exponent, no spaces)"}
			}
			if len(v) > w {
				return &FieldError{Field: headerFieldPath(f.Name), Reason: fmt.Sprintf(
					"is %d characters; IMOVEIN field %d holds %d", len(v), f.Line, w)}
			}
		}
	}
	return nil
}

// isHeaderNumber reports whether s is -?digits(.digits)?, ASCII only.
func isHeaderNumber(s string) bool {
	s = strings.TrimPrefix(s, "-")
	whole, frac, hasPoint := strings.Cut(s, ".")
	if !allASCIIDigits(whole) {
		return false
	}
	return !hasPoint || allASCIIDigits(frac)
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return true
		}
	}
	return false
}

// applyHeader writes the fields h carries over the header buildIMOVEIN built
// from the account and the request. A field h does not carry is left as it
// was, which is what keeps a request without "header" byte-identical.
func applyHeader(hdr *stockHeader, h *OrderHeader) {
	if h == nil {
		return
	}
	if v := h.AccountNameDetail; v != "" {
		hdr.AccountName = joinAccountNameDetail(hdr.AccountName, headerTextValue(v), imoveInFieldLengths["line5"])
	}
	set := func(dst *string, v string, kind headerKind) {
		if v == "" {
			return
		}
		if kind == headerText {
			v = headerTextValue(v)
		}
		*dst = v
	}
	set(&hdr.Address, h.Address, headerText)
	set(&hdr.City, h.City, headerText)
	set(&hdr.Phone, h.Phone, headerText)
	set(&hdr.ExtraText1, h.ExtraText1, headerText)
	set(&hdr.ExtraText2, h.ExtraText2, headerText)
	set(&hdr.ExtraText3, h.ExtraText3, headerText)
	set(&hdr.ExtraText4, h.ExtraText4, headerText)
	set(&hdr.ExtraText5, h.ExtraText5, headerText)
	set(&hdr.ExtraSum1, h.ExtraSum1, headerNumber)
	set(&hdr.ExtraSum2, h.ExtraSum2, headerNumber)
	set(&hdr.ExtraSum3, h.ExtraSum3, headerNumber)
	set(&hdr.ExtraNum1, h.ExtraNum1, headerNumber)
	set(&hdr.ExtraNum2, h.ExtraNum2, headerNumber)
}

// joinAccountNameDetail is "<name>|<detail>" in at most width characters. The
// detail is validated to fit with the separator; when the account's name is
// too long for both, the name loses its end — it was always cut to the field —
// and the detail, which the caller sent for this document, is kept whole.
func joinAccountNameDetail(name, detail string, width int) string {
	room := width - utf8.RuneCountInString(accountNameSeparator) - utf8.RuneCountInString(detail)
	if room < 0 {
		room = 0
	}
	if r := []rune(name); len(r) > room {
		name = string(r[:room])
	}
	return name + accountNameSeparator + detail
}
