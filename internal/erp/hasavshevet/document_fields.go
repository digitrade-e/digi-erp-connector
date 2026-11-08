package hasavshevet

import (
	"fmt"
	"strconv"
	"strings"
)

// Document fields (DIG-1295, plan 075 P1-2). The IMOVEIN header's ERP details
// — which document type, which warehouse, which VAT rate, which external
// reference — are per-tenant erp-manager configuration carried by the request,
// never connector constants. Each one left out keeps today's value, so a
// request without them writes exactly the bytes it always did:
//
//	documentId  line4  (2 digits)  absent: ORDER 30 (ש"ח) / 32, QUOATE 40, RETURN 74
//	warehouse   line12 (9 digits)  absent: 1
//	vatPercent  line18 (5 chars)   absent: "18.00"
//	asmahta2    line8  (9 digits)  absent: the order number
//
// MyID (line3) is always the order number. A value that does not fit its
// field is refused, never cut: padW1255 would truncate it silently.

// Wire names of the document fields, as FieldError.Field.
const (
	fieldDocumentID = "documentId"
	fieldWarehouse  = "warehouse"
	fieldVatPercent = "vatPercent"
	fieldAsmahta2   = "asmahta2"
)

// Defaults: today's values, written when a field is absent.
const (
	defaultWarehouse  = 1
	defaultVatPercent = "18.00"
)

// maxForWidth is the largest whole number that fits an IMOVEIN field of the
// given width: 99 for line4, 999 999 999 for line8 and line12.
func maxForWidth(line string) int64 {
	n := int64(1)
	for i := 0; i < imoveInFieldLengths[line]; i++ {
		n *= 10
	}
	return n - 1
}

// ValidateDocumentFields checks the document fields of req in the form the
// IMOVEIN writer takes them (zero / "" = absent). It returns a *FieldError
// naming the wire field, or nil. The route calls it before anything is
// reserved; the writer calls it again for every other caller.
func ValidateDocumentFields(req OrderRequest) error {
	if req.DocumentID != 0 {
		if max := maxForWidth("line4"); req.DocumentID < 1 || int64(req.DocumentID) > max {
			return &FieldError{Field: fieldDocumentID, Reason: fmt.Sprintf("must be a whole number from 1 to %d (IMOVEIN line4 holds %d digits)", max, imoveInFieldLengths["line4"])}
		}
	}
	if req.Warehouse != 0 {
		if max := maxForWidth("line12"); req.Warehouse < 1 || int64(req.Warehouse) > max {
			return &FieldError{Field: fieldWarehouse, Reason: fmt.Sprintf("must be a whole number from 1 to %d (IMOVEIN line12 holds %d digits)", max, imoveInFieldLengths["line12"])}
		}
	}
	if req.VatPercent != "" {
		if got, err := FormatVatPercent(req.VatPercent); err != nil || got != req.VatPercent {
			return &FieldError{Field: fieldVatPercent, Reason: "must be written as IMOVEIN's VAT text, e.g. 17.00 (two decimals, at most 99.99)"}
		}
	}
	if req.Asmahta2 != "" {
		if w := imoveInFieldLengths["line8"]; len(req.Asmahta2) > w || !allASCIIDigits(req.Asmahta2) {
			return &FieldError{Field: fieldAsmahta2, Reason: fmt.Sprintf("must be 1 to %d digits (IMOVEIN line8)", w)}
		}
	}
	return nil
}

// FormatVatPercent turns a VAT rate as the caller wrote it — a whole number
// or one with up to two decimals, ASCII digits only ("17", "18.5") — into the
// IMOVEIN line18 text ("17.00", "18.50"). More decimals (it would have to be
// rounded), a sign, an exponent, or a rate whose text does not fit line18
// (100 and over) is a *FieldError.
func FormatVatPercent(s string) (string, error) {
	bad := &FieldError{Field: fieldVatPercent, Reason: fmt.Sprintf(
		"must be a VAT rate from 0 to 99.99 with at most two decimals (IMOVEIN line18 holds %d characters)", imoveInFieldLengths["line18"])}
	whole, frac, hasPoint := strings.Cut(s, ".")
	if whole == "" || !allASCIIDigits(whole) || (hasPoint && (frac == "" || len(frac) > 2 || !allASCIIDigits(frac))) {
		return "", bad
	}
	n, err := strconv.ParseInt(whole, 10, 64) // drops leading zeros: "017" is 17
	if err != nil {
		return "", bad
	}
	for len(frac) < 2 {
		frac += "0"
	}
	out := strconv.FormatInt(n, 10) + "." + frac
	if len(out) > imoveInFieldLengths["line18"] {
		return "", bad
	}
	return out, nil
}

func allASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
