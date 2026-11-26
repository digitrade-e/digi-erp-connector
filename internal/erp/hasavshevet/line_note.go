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

// lineNoteValue is a line note as it is written. ValidateLineNotes admits only
// lineNoteAllowed characters, so nothing is converted: the note is written as
// sent, edge spaces trimmed.
func lineNoteValue(s string) string {
	return strings.TrimSpace(s)
}

// lineNoteAllowed reports whether r may appear in a line note (DIG-1551):
// Hebrew letters, Latin letters, digits, a space and . , - / ( ) + :
//
// Field 71 is read by Hashavshevet's importer, which a BAT runs. Operators of
// that importer report that a line break, a quote or another special character
// in a free text breaks the run (Digitrade, 2026-11-26), so a note carries none:
// refused, never stripped or converted, so the caller learns it.
func lineNoteAllowed(r rune) bool {
	switch {
	case r >= 'א' && r <= 'ת':
		return true
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune(" .,-/()+:", r)
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
// Only lineNoteAllowed characters are admitted: a line break, a quote, a tab or
// any other special character is refused, naming the first one. Every admitted
// character is one Windows-1255 byte, so the width is the character count after
// the edge spaces are trimmed. A note of spaces only is refused too.
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
		for _, r := range v {
			if !lineNoteAllowed(r) {
				return &FieldError{Field: lineNoteField(i), Reason: fmt.Sprintf(
					"contains %q; a line note takes only letters, digits, spaces and . , - / ( ) + : (no line breaks, quotes or other special characters)", r)}
			}
		}
		if n := utf8.RuneCountInString(lineNoteValue(v)); n > w {
			return &FieldError{Field: lineNoteField(i), Reason: fmt.Sprintf(
				"is %d characters; IMOVEIN field %d holds %d", n, lineNoteLine, w)}
		}
	}
	return nil
}
