package hasavshevet

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// lineNoteLine is the IMOVEIN field a line's note is written to (DIG-1551):
// 71, פרטים בתנועה — the stock move's own details, 20 characters.
const lineNoteLine = 71

// lineNoteField is the wire path of the i-th line's note in a 400.
func lineNoteField(i int) string { return fmt.Sprintf("details[%d]/note", i) }

// lineNoteValue is a line note as it is written: line breaks become ". " and
// single quotes go, exactly like a header text (headerTextValue).
func lineNoteValue(s string) string {
	if s == "" {
		return ""
	}
	return headerTextValue(s)
}

// lineNoteWidth is how many characters field 71 holds in the official layout.
func lineNoteWidth() int { return imoveInFieldLengths[fmt.Sprintf("line%d", lineNoteLine)] }

// ValidateLineNotes checks every line's note against IMOVEIN field 71. It
// returns a *FieldError naming "details[i]/note", or nil. The route calls it
// before anything is reserved; the writer calls it again for every other
// caller. ValidateLegacyFit refuses any note: the legacy layout has no field 71.
//
// A note is never cut (DIG-1551, decision 2026-11-26): one that does not fit
// is refused, so the caller learns it instead of the ERP holding half a note.
// Widths are counted as written, as for the header texts: one Windows-1255
// byte per character, after line breaks became ". " (a line break counts as
// two). A note of spaces only, or with any other control character (a tab,
// NUL…), is refused: it would sit inside a fixed-width record.
func ValidateLineNotes(lines []OrderLineItem) error {
	w := lineNoteWidth()
	for i, d := range lines {
		v := d.Note
		if v == "" {
			continue
		}
		if strings.TrimSpace(v) == "" {
			return &FieldError{Field: lineNoteField(i), Reason: "must not be only spaces (leave it out instead)"}
		}
		if hasControl(strings.NewReplacer("\r", "", "\n", "").Replace(v)) {
			return &FieldError{Field: lineNoteField(i), Reason: "must not contain control characters (tab, NUL…)"}
		}
		if n := utf8.RuneCountInString(lineNoteValue(v)); n > w {
			return &FieldError{Field: lineNoteField(i), Reason: fmt.Sprintf(
				"is %d characters; IMOVEIN field %d holds %d (line breaks count as two)", n, lineNoteLine, w)}
		}
	}
	return nil
}
