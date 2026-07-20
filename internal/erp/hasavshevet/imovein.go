package hasavshevet

import (
	"bytes"
	"fmt"
	"strings"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/transform"
)

// imoveInFieldLengths is the canonical fixed-length mapping for IMOVEIN import files.
// Source: Hasavshevet text import documentation (official, 2026-02-22 scan).
// line63 is explicitly 0 (temporarily unused per docs).
// Total active record length (line2..line87, excluding line63=0): 2891.
var imoveInFieldLengths = map[string]int{
	"line2":  15,
	"line3":  9,
	"line4":  2,
	"line5":  50,
	"line6":  50,
	"line7":  50,
	"line8":  9,
	"line9":  10,
	"line10": 10,
	"line11": 9,
	"line12": 9,
	"line13": 250,
	"line14": 9,
	"line15": 9,
	"line16": 1,
	"line17": 5,
	"line18": 5,
	"line19": 2,
	"line20": 4,
	"line21": 10,
	"line22": 20,
	"line23": 10,
	"line24": 10,
	"line25": 4,
	"line26": 5,
	"line27": 10,
	"line28": 100,
	"line29": 5,
	"line30": 6,
	"line31": 20,
	"line32": 5,
	"line33": 10,
	"line34": 1,
	"line35": 30,
	"line36": 10,
	"line37": 250,
	"line38": 9,
	"line39": 8,
	"line40": 10,
	"line41": 20,
	"line42": 20,
	"line43": 10,
	"line44": 20,
	"line45": 50,
	"line46": 50,
	"line47": 50,
	"line48": 50,
	"line49": 50,
	"line50": 13,
	"line51": 13,
	"line52": 13,
	"line53": 9,
	"line54": 9,
	"line55": 9,
	"line56": 50,
	"line57": 50,
	"line58": 13,
	"line59": 13,
	"line60": 9,
	"line61": 9,
	"line62": 50,
	"line63": 0, // temporarily unused per Hasavshevet docs
	"line64": 100,
	"line65": 100,
	"line66": 50,
	"line67": 8,
	"line68": 100,
	"line69": 15,
	"line70": 10,
	"line71": 20,
	"line72": 9,
	"line73": 5,
	"line74": 250,
	"line75": 250,
	"line76": 3,
	"line77": 3,
	"line78": 5,
	"line79": 10,
	"line80": 10,
	"line81": 13,
	"line82": 13,
	"line83": 10,
	"line84": 10,
	"line85": 13,
	"line86": 13,
	"line87": 250,
}

// imoveInFieldTitles maps PRM line keys to Hebrew descriptions.
// Source: Hasavshevet fieldTitlesPRM spec.
var imoveInFieldTitles = map[string]string{
	"line2":  "מפתח לקוח - 2",
	"line3":  "אסמכתא - 3",
	"line4":  "סוג מסמך - 4",
	"line5":  "שם לקוח - 5",
	"line6":  "כתובת 1 - 6",
	"line7":  "כתובת 2 - 7",
	"line8":  "אסמכתא 2 - 8",
	"line9":  "תאריך אסמכתא - 9",
	"line10": "תאריך ערך - 10",
	"line11": "סוכן - 11",
	"line12": "מחסן - 12",
	"line13": "פרטים - 13",
	"line14": "מחסן לבנים - 14",
	"line15": "סוכן לבנים - 15",
	"line16": "מחירון לבנים - 16",
	"line17": "הנחה כללית % - 17",
	"line18": "מע\"מ  %- 18",
	"line19": "מס' עותקים - 19",
	"line20": "מטבע - 20",
	"line21": "שער - 21",
	"line22": "מפתח פריט - 22",
	"line23": "כמות - 23",
	"line24": "מחיר - 24",
	"line25": "מטבע - 25",
	"line26": "הנחה % - 26",
	"line27": "שער - 27",
	"line28": "שם פריט - 28",
	"line29": "יחידת מידה - 29",
	"line30": "מס קניה - 30 %",
	"line31": "מפתח חליפי - 31",
	"line32": "32-עמלה %",
	"line33": "אריזות - 33",
	"line34": "פטור ממע\"מ- 34",
	"line35": "טלפון - 35",
	"line36": "תאריך נוסף - 36",
	"line37": "הערות - 37",
	"line38": "38-אסמכתא 3",
	"line39": "קוד תמחיר - 39",
	"line40": "40-תאריך תפוגה",
	"line41": "אצווה - 41",
	"line42": "איתור - 42",
	"line43": "פג תוקף - 43",
	"line44": "מספר טבוע - 44",
	"line45": "טקסט נוסף כותרת1 -45",
	"line46": "טקסט נוסף כותרת2 -46",
	"line47": "טקסט נוסף כותרת 3 -47",
	"line48": "48- טקסט נוסף כותרת 4",
	"line49": "49- טקסט נוסף כותרת 5",
	"line50": "50 -סכום נוסף כותרת 1",
	"line51": "סכום נוסף כותרת 2 -51",
	"line52": "סכום נוסף כותרת 3 - 52",
	"line53": "53 -קו הפצה",
	"line54": "54 -מספר שורה",
	"line55": "55 -מספר שורת בסיס",
	"line56": "56 - הערה נוספת בתנועה1",
	"line57": "57 - הערה נוספת בתנועה 2",
	"line58": "58 - סכום נוסף בתנועה 1",
	"line59": "59 - סכום נוסף בתנועה 2",
	"line60": "60 -עוסק מורשה - 874",
	"line61": "61 -מזהה תנועה בסיס",
	"line62": "62 -דוא\"ל איש קשר",
	"line63": "63 -refnum",
	"line64": "64-קובץ כותרת",
	"line65": "65 -קובץ תנועה",
	"line66": "66 -איש קשר",
	"line67": "67 -קוד תמחיר תנועה",
	"line68": "68 -שם פריט חלופי",
	"line69": "69\t-כרטיס הכנסות / הוצאות",
	"line70": "70 -תאריך ערך בתנועה",
	"line71": "71 -פרטים בתנועה",
	"line72": "72 -מחסן בתנועה",
	"line73": "73 -מטבע שערוך",
	"line74": "74 -כתובת לייצוא",
	"line75": "75 -הערות לייצוא",
	"line76": "76 -סוג תנועה",
	"line77": "77 -סוג תנועה פטור",
	"line78": "78 -שער לשערוך",
	"line79": "79 תאריך נוסף 1 כותרת-",
	"line80": "80 תאריך נוסף 2 כותרת-",
	"line81": "81 -מספר נוסף 1 כותרת",
	"line82": "82 -מספר נוסף 2 כותרת",
	"line83": "83 תאריך נוסף 1 תנועה-",
	"line84": "84 -תאריך נוסף 2 תנועה",
	"line85": "85 מספר נוסף 1 תנועה-",
	"line86": "86 -מספר נוסף 2 תנועה",
	"line87": "87 -מספר הקצאה",
}

// prmTotalRecordLength is the sum of all active field lengths (line2..line87, line63=0).
// Verified against official Hasavshevet docs baseline.
const prmTotalRecordLength = 2891

// stockHeader holds per-document header fields repeated on every DOC record.
type stockHeader struct {
	AccountKey  string
	MyID        int64
	DocumentID  int
	AccountName string
	Address     string
	City        string
	Asmahta2    string
	ShortDate   string
	Agent       string
	WareHouse   int
	DiscountPrcR string
	VatPrc      string
	Copies      string
	Currency    string
	Rate        string
	Phone       string
	Remarks     string
	HProtect    string
	ExtraText1  string
	ExtraText2  string
	ExtraText3  string
	ExtraText4  string
	ExtraText5  string
	ExtraSum1   string
	ExtraSum2   string
	ExtraSum3   string
	ExtraDate1  string
	ExtraDate2  string
	ExtraNum1   string
	ExtraNum2   string
}

// stockMove holds per-line item fields for one DOC record.
// Only the fields written to the DOC are included.
type stockMove struct {
	ItemKey     string
	ItemName    string
	Quantity    string
	Packs       string // number of packages (line33 / אריזות)
	Price       string // originalPrice (line24)
	DiscountPrc string
	Unit        string
}

// generateDOC generates the IMOVEIN.doc binary content (Windows-1255 encoded).
// Each row corresponds to one stock move; header fields repeat on every row.
// Row size = prmTotalRecordLength bytes + LF.
func generateDOC(hdr stockHeader, moves []stockMove) ([]byte, error) {
	var buf bytes.Buffer
	for _, move := range moves {
		record := buildDOCRecord(hdr, move)
		buf.Write(record)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// buildDOCRecord builds a single fixed-length Windows-1255 record.
// Field order and widths match the canonical imoveInFieldLengths mapping.
func buildDOCRecord(h stockHeader, m stockMove) []byte {
	f := imoveInFieldLengths
	w := padW1255

	var buf bytes.Buffer
	buf.Write(w(h.AccountKey, f["line2"]))
	buf.Write(w(fmt.Sprintf("%d", h.MyID), f["line3"]))
	buf.Write(w(fmt.Sprintf("%d", h.DocumentID), f["line4"]))
	buf.Write(w(h.AccountName, f["line5"]))
	buf.Write(w(h.Address, f["line6"]))
	buf.Write(w(h.City, f["line7"]))
	buf.Write(w(h.Asmahta2, f["line8"]))
	buf.Write(w(h.ShortDate, f["line9"]))
	buf.Write(w(h.ShortDate, f["line10"]))
	buf.Write(w(h.Agent, f["line11"]))
	buf.Write(w(fmt.Sprintf("%d", h.WareHouse), f["line12"]))
	buf.Write(w("", f["line13"]))
	buf.Write(w("", f["line14"]))
	buf.Write(w("", f["line15"]))
	buf.Write(w("", f["line16"]))
	buf.Write(w(h.DiscountPrcR, f["line17"]))
	buf.Write(w(h.VatPrc, f["line18"]))
	buf.Write(w(h.Copies, f["line19"]))
	buf.Write(w(h.Currency, f["line20"]))
	buf.Write(w(h.Rate, f["line21"]))
	buf.Write(w(m.ItemKey, f["line22"]))
	buf.Write(w(m.Quantity, f["line23"]))
	buf.Write(w(m.Price, f["line24"]))
	buf.Write(w(h.Currency, f["line25"]))
	buf.Write(w(m.DiscountPrc, f["line26"]))
	buf.Write(w(h.Rate, f["line27"]))
	buf.Write(w(m.ItemName, f["line28"]))
	buf.Write(w(m.Unit, f["line29"]))
	buf.Write(w("", f["line30"]))
	buf.Write(w("", f["line31"]))
	buf.Write(w("", f["line32"]))
	buf.Write(w(m.Packs, f["line33"])) // אריזות / packs
	buf.Write(w("0", f["line34"]))
	buf.Write(w(h.Phone, f["line35"]))
	buf.Write(w(h.ShortDate, f["line36"]))
	buf.Write(w(h.Remarks, f["line37"]))
	buf.Write(w("", f["line38"]))
	buf.Write(w("", f["line39"]))
	buf.Write(w("", f["line40"]))
	buf.Write(w("", f["line41"]))
	buf.Write(w("", f["line42"]))
	buf.Write(w("", f["line43"]))
	buf.Write(w("", f["line44"]))
	buf.Write(w(h.ExtraText1, f["line45"]))
	buf.Write(w(h.ExtraText2, f["line46"]))
	buf.Write(w(h.ExtraText3, f["line47"]))
	buf.Write(w(h.ExtraText4, f["line48"]))
	buf.Write(w(h.ExtraText5, f["line49"]))
	buf.Write(w(h.ExtraSum1, f["line50"]))
	buf.Write(w(h.ExtraSum2, f["line51"]))
	buf.Write(w(h.ExtraSum3, f["line52"]))
	buf.Write(w("", f["line53"]))
	buf.Write(w("", f["line54"]))
	buf.Write(w("", f["line55"]))
	buf.Write(w("", f["line56"]))
	buf.Write(w("", f["line57"]))
	buf.Write(w("", f["line58"]))
	buf.Write(w("", f["line59"]))
	buf.Write(w(h.HProtect, f["line60"]))
	buf.Write(w("", f["line61"]))
	buf.Write(w("", f["line62"]))
	// line63: length 0, omitted from record
	buf.Write(w("", f["line64"]))
	buf.Write(w("", f["line65"]))
	buf.Write(w("", f["line66"]))
	buf.Write(w("", f["line67"]))
	buf.Write(w("", f["line68"]))
	buf.Write(w("", f["line69"]))
	buf.Write(w("", f["line70"]))
	buf.Write(w("", f["line71"]))
	buf.Write(w("", f["line72"]))
	buf.Write(w("", f["line73"]))
	buf.Write(w("", f["line74"]))
	buf.Write(w("", f["line75"]))
	buf.Write(w("", f["line76"]))
	buf.Write(w("", f["line77"]))
	buf.Write(w("", f["line78"]))
	buf.Write(w(h.ExtraDate1, f["line79"]))
	buf.Write(w(h.ExtraDate2, f["line80"]))
	buf.Write(w(h.ExtraNum1, f["line81"]))
	buf.Write(w(h.ExtraNum2, f["line82"]))
	buf.Write(w("", f["line83"]))
	buf.Write(w("", f["line84"]))
	buf.Write(w("", f["line85"]))
	buf.Write(w("", f["line86"]))
	buf.Write(w("", f["line87"]))
	return buf.Bytes()
}

// generatePRM generates the IMOVEIN.prm binary content (Windows-1255 encoded).
// Format: line1 = total record length; lines 2..87 = "start end ;title".
// Zero-length fields (line63) emit "0 0 ;title".
func generatePRM() []byte {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%d ;אורך רשומה - 1\n", prmTotalRecordLength))
	pos := 1
	for i := 2; i <= 87; i++ {
		key := fmt.Sprintf("line%d", i)
		length := imoveInFieldLengths[key]
		title := imoveInFieldTitles[key]
		if title == "" {
			title = fmt.Sprintf("Unknown - %d", i)
		}
		if length == 0 {
			sb.WriteString(fmt.Sprintf("0 0 ;%s\n", title))
		} else {
			end := pos + length - 1
			sb.WriteString(fmt.Sprintf("%d %d ;%s\n", pos, end, title))
			pos = end + 1
		}
	}
	return encodeWindows1255(sb.String())
}

// padW1255 encodes value to Windows-1255, truncates to width bytes if needed,
// then pads with ASCII spaces to exactly width bytes.
// Returns nil if width <= 0.
// Unmappable characters are replaced with '?'.
func padW1255(value string, width int) []byte {
	if width <= 0 {
		return nil
	}
	result := make([]byte, 0, width)
	for _, r := range value {
		if len(result) >= width {
			break
		}
		result = append(result, encodeRune1255(r))
	}
	for len(result) < width {
		result = append(result, ' ')
	}
	return result
}

// encodeRune1255 encodes a single Unicode rune to its Windows-1255 byte.
// Returns '?' for characters that cannot be represented in Windows-1255.
func encodeRune1255(r rune) byte {
	enc := charmap.Windows1255.NewEncoder()
	b, _, err := transform.Bytes(enc, []byte(string(r)))
	if err != nil || len(b) == 0 {
		return '?'
	}
	return b[0]
}

// encodeWindows1255 encodes a UTF-8 string to Windows-1255 bytes,
// replacing unmappable characters with '?'.
func encodeWindows1255(s string) []byte {
	result := make([]byte, 0, len(s))
	for _, r := range s {
		result = append(result, encodeRune1255(r))
	}
	return result
}
