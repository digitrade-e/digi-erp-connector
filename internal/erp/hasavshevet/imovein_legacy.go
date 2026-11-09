package hasavshevet

import (
	"bytes"
	"fmt"
	"strings"
)

// The legacy IMOVEIN layout (DIG-1297, plan 075 P1-3): the one the old
// Digitrade app writes (global_api.php prepareForHerpDoc :5186, WriteDocSpaces
// :5282, getHerpOrderConstantLengths :5927, CreatePrmDoc :4486), and so the
// one a Hashavshevet scheduler set up beside it reads — on BFL, WizSched with
// the static imovein.prm in herp. It is not the official layout imovein.go
// writes: only 37 fields are written; each is the Windows-1255 value cut to
// its width, padded with spaces to it, plus ONE separator space; several
// widths differ (17: 6, 18: 4, 23: 9, 24: 9, 26: 6, 50-52: 12); fields 20,
// 21, 25, 27, 33, 60, 79, 80 and 87 are absent. Its PRM lists fields 2-86 as
// "start end ;title" with the next start two past the end, "0 0" for a field
// not written, and the next start after the last field (1297) as line 1.
//
// Only the layout is the legacy's. What goes into each field is the
// request's, built by buildIMOVEIN like the official layout (DIG-1295 carries
// the tenant's document fields); the goldens in testdata/imovein-legacy were
// written by the legacy functions themselves (generate.php).

// legacyField is one PRM line of the legacy layout.
type legacyField struct {
	line  int    // 2..86
	width int    // 0 = not written (PRM "0 0")
	title string // verbatim from CreatePrmDoc
}

// legacyRecordLength is one legacy DOC line without its LF: the written
// widths plus one separator space each.
const legacyRecordLength = 1296

// legacyFields is the legacy PRM, in order. Taken from the PRM the legacy
// writer produced (testdata/imovein-legacy/IMOVEIN.prm), titles included —
// line 69's carries a TAB, as the legacy's does.
var legacyFields = []legacyField{
	{2, 15, "מפתח לקוח - 2"},
	{3, 9, "אסמכתא - 3"},
	{4, 2, "סוג מסמך - 4"},
	{5, 50, "שם לקוח - 5"},
	{6, 50, "כתובת 1 - 6"},
	{7, 50, "כתובת 2 - 7"},
	{8, 9, "אסמכתא 2 - 8"},
	{9, 10, "תאריך אסמכתא - 9"},
	{10, 10, "תאריך ערך - 10"},
	{11, 9, "סוכן - 11"},
	{12, 9, "מחסן - 12"},
	{13, 250, "פרטים - 13"},
	{14, 0, "מחסן לבנים - 14"},
	{15, 0, "סוכן לבנים - 15"},
	{16, 0, "מחירון לבנים - 16"},
	{17, 6, "הנחה כללית % - 17"},
	{18, 4, "מע\"מ  %- 18"},
	{19, 2, "מס' עותקים - 19"},
	{20, 0, "מטבע - 20"},
	{21, 0, "שער - 21"},
	{22, 20, "מפתח פריט - 22"},
	{23, 9, "כמות - 23"},
	{24, 9, "מחיר - 24"},
	{25, 0, "מטבע - 25"},
	{26, 6, "הנחה % - 26"},
	{27, 0, "שער - 27"},
	{28, 100, "שם פריט - 28"},
	{29, 5, "יחידת מידה - 29"},
	{30, 0, "מס קניה - 30 %"},
	{31, 0, "מפתח חליפי - 31"},
	{32, 0, "32-עמלה %"},
	{33, 0, "אריזות - 33"},
	{34, 1, "פטור ממע\"מ- 34"},
	{35, 30, "טלפון - 35"},
	{36, 10, "תאריך נוסף - 36"},
	{37, 250, "הערות - 37"},
	{38, 0, "38-אסמכתא 3"},
	{39, 0, "קוד תמחיר - 39"},
	{40, 0, "40-תאריך תפוגה"},
	{41, 0, "אצווה - 41"},
	{42, 0, "איתור - 42"},
	{43, 0, "פג תוקף - 43"},
	{44, 0, "מספר טבוע - 44"},
	{45, 50, "טקסט נוסף כותרת1 -45"},
	{46, 50, "טקסט נוסף כותרת2 -46"},
	{47, 50, "טקסט נוסף כותרת 3 -47"},
	{48, 50, "48- טקסט נוסף כותרת 4"},
	{49, 50, "49- טקסט נוסף כותרת 5"},
	{50, 12, "50 -סכום נוסף כותרת 1"},
	{51, 12, "סכום נוסף כותרת 2 -51"},
	{52, 12, "סכום נוסף כותרת 3 - 52"},
	{53, 0, "53 -קו הפצה"},
	{54, 0, "54 -מספר שורה"},
	{55, 0, "55 -מספר שורת בסיס"},
	{56, 0, "56 - הערה נוספת בתנועה1"},
	{57, 0, "57 - הערה נוספת בתנועה 2"},
	{58, 12, "58 - סכום נוסף בתנועה 1"},
	{59, 12, "59 - סכום נוסף בתנועה 2"},
	{60, 0, "60 -עוסק מורשה - 874"},
	{61, 0, "61 -מזהה תנועה בסיס"},
	{62, 0, "62 -דוא\"ל איש קשר"},
	{63, 0, "63 -refnum"},
	{64, 0, "64-קובץ כותרת"},
	{65, 0, "65 -קובץ תנועה"},
	{66, 0, "66 -איש קשר"},
	{67, 0, "67 -קוד תמחיר תנועה"},
	{68, 0, "68 -שם פריט חלופי"},
	{69, 0, "69\t-כרטיס הכנסות / הוצאות"},
	{70, 0, "70 -תאריך ערך בתנועה"},
	{71, 0, "71 -פרטים בתנועה"},
	{72, 0, "72 -מחסן בתנועה"},
	{73, 0, "73 -מטבע שערוך"},
	{74, 0, "74 -כתובת לייצוא"},
	{75, 0, "75 -הערות לייצוא"},
	{76, 0, "76 -סוג תנועה"},
	{77, 0, "77 -סוג תנועה פטור"},
	{78, 0, "78 -שער לשערוך"},
	{79, 0, "79 תאריך נוסף 1 כותרת-"},
	{80, 0, "80 תאריך נוסף 2 כותרת-"},
	{81, 12, "81 -מספר נוסף 1 כותרת"},
	{82, 12, "82 -מספר נוסף 2 כותרת"},
	{83, 0, "83 תאריך נוסף 1 תנועה-"},
	{84, 0, "84 -תאריך נוסף 2 תנועה"},
	{85, 0, "85 מספר נוסף 1 תנועה-"},
	{86, 0, "86 -מספר נוסף 2 תנועה"},
}

// legacyFieldValue is the text written in a legacy field. Every written line
// has a case; the blank ones (13, 58, 59) are written as spaces, like the
// legacy's WriteDocSpaces of an empty string.
func legacyFieldValue(line int, h stockHeader, m stockMove) string {
	switch line {
	case 2:
		return h.AccountKey
	case 3:
		return fmt.Sprintf("%d", h.MyID)
	case 4:
		return fmt.Sprintf("%d", h.DocumentID)
	case 5:
		return h.AccountName
	case 6:
		return h.Address
	case 7:
		return h.City
	case 8:
		return h.Asmahta2
	case 9, 10, 36:
		return h.ShortDate
	case 11:
		return h.Agent
	case 12:
		return fmt.Sprintf("%d", h.WareHouse)
	case 17:
		return h.DiscountPrcR
	case 18:
		return h.VatPrc
	case 19:
		return h.Copies
	case 22:
		return m.ItemKey
	case 23:
		return m.Quantity
	case 24:
		return m.Price
	case 26:
		return m.DiscountPrc
	case 28:
		return m.ItemName
	case 29:
		return m.Unit
	case 34:
		return "0"
	case 35:
		return h.Phone
	case 37:
		return h.Remarks
	case 45:
		return h.ExtraText1
	case 46:
		return h.ExtraText2
	case 47:
		return h.ExtraText3
	case 48:
		return h.ExtraText4
	case 49:
		return h.ExtraText5
	case 50:
		return h.ExtraSum1
	case 51:
		return h.ExtraSum2
	case 52:
		return h.ExtraSum3
	case 81:
		return h.ExtraNum1
	case 82:
		return h.ExtraNum2
	}
	return "" // 13, 58, 59
}

// generateLegacyDOC is generateDOC in the legacy layout: one LF-terminated
// line per stock move, header fields repeated on each.
func generateLegacyDOC(h stockHeader, moves []stockMove) []byte {
	var buf bytes.Buffer
	for _, m := range moves {
		for _, f := range legacyFields {
			if f.width == 0 {
				continue
			}
			buf.Write(padW1255(legacyFieldValue(f.line, h, m), f.width))
			buf.WriteByte(' ')
		}
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// generateLegacyPRM is the PRM that describes generateLegacyDOC's lines,
// byte for byte the legacy writer's.
func generateLegacyPRM() []byte {
	var body strings.Builder
	next := 1
	for _, f := range legacyFields {
		if f.width == 0 {
			fmt.Fprintf(&body, "0 0 ;%s\n", f.title)
			continue
		}
		end := next + f.width - 1
		fmt.Fprintf(&body, "%d %d ;%s\n", next, end, f.title)
		next = end + 2
	}
	return encodeWindows1255(fmt.Sprintf("%d ;אורך רשומה - 1\n", next) + body.String())
}

// legacyWidth is the width of a written legacy field.
func legacyWidth(line int) int {
	for _, f := range legacyFields {
		if f.line == line {
			return f.width
		}
	}
	return 0
}

// ValidateLegacyFit refuses a request whose numbers the legacy layout cannot
// hold as written. Its widths are narrower than the official ones for the VAT
// rate (4), the quantity (9) and the price (9), and the legacy writer cuts a
// longer value silently. A cut that drops only trailing zeros after the
// decimal point keeps the number ("18.00" → "18.0", what the legacy writes for
// 18 % VAT) and is allowed; any other is a *FieldError naming the wire field,
// before a number is reserved (the route) or a file written (the writer). The
// text fields are cut like the legacy's: a name or remark longer than its
// field loses its end, as it always has.
func ValidateLegacyFit(req OrderRequest) error {
	h, moves := buildIMOVEIN(1, accountInfo{}, req, 1)
	fits := func(field string, line int, text string) error {
		if keepsNumber(text, legacyWidth(line)) {
			return nil
		}
		return &FieldError{Field: field, Reason: fmt.Sprintf(
			"%s does not fit the legacy IMOVEIN field %d (%d characters) without changing the number", text, line, legacyWidth(line))}
	}
	if err := fits(fieldVatPercent, 18, h.VatPrc); err != nil {
		return err
	}
	if err := fits("discount", 17, h.DiscountPrcR); err != nil {
		return err
	}
	for i, m := range moves {
		for _, c := range []struct {
			name string
			line int
			text string
		}{
			{"quantity", 23, m.Quantity},
			{"originalPrice", 24, m.Price},
			{"discount", 26, m.DiscountPrc},
		} {
			if err := fits(fmt.Sprintf("details[%d].%s", i, c.name), c.line, c.text); err != nil {
				return err
			}
		}
	}
	return nil
}

// keepsNumber reports whether cutting the number text s to width characters
// leaves the same number: it fits, or the cut part is only zeros after the
// decimal point.
func keepsNumber(s string, width int) bool {
	if len(s) <= width {
		return true
	}
	kept, cut := s[:width], s[width:]
	return strings.Contains(kept, ".") && strings.Trim(cut, "0") == ""
}
