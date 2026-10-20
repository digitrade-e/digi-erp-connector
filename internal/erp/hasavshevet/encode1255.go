package hasavshevet

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/unicode/norm"
)

// This is the strict Windows-1255 path used by the KUPAIN receipt writer.
// It deliberately does NOT share padW1255 (the IMOVEIN path), which truncates
// silently and turns anything unmappable into '?': a receipt is a legal
// document, so a value that does not fit or cannot be represented is refused
// and the caller is told which field, rather than written damaged.

// strippedRunes are bidi formatting controls and other invisible characters
// (zero-width joiners, BOM, soft hyphen). They are invisible, they reach
// us from copy-pasted Hebrew text, and two of them (LRM, RLM) are even
// representable in Windows-1255 (0xFD, 0xFE) — which would put invisible bytes
// into a fixed-width field. They are removed, never encoded. The apostrophe is
// removed too (decided by plan 005 §3 C1).
var strippedRunes = map[rune]bool{
	'\u200E': true, // LEFT-TO-RIGHT MARK
	'\u200F': true, // RIGHT-TO-LEFT MARK
	'\u202A': true, // LEFT-TO-RIGHT EMBEDDING
	'\u202B': true, // RIGHT-TO-LEFT EMBEDDING
	'\u202C': true, // POP DIRECTIONAL FORMATTING
	'\u202D': true, // LEFT-TO-RIGHT OVERRIDE
	'\u202E': true, // RIGHT-TO-LEFT OVERRIDE
	'\u2066': true, // LEFT-TO-RIGHT ISOLATE
	'\u2067': true, // RIGHT-TO-LEFT ISOLATE
	'\u2068': true, // FIRST STRONG ISOLATE
	'\u2069': true, // POP DIRECTIONAL ISOLATE
	'\u061C': true, // ARABIC LETTER MARK
	'\u200C': true, // ZERO WIDTH NON-JOINER
	'\u200D': true, // ZERO WIDTH JOINER
	'\uFEFF': true, // BYTE ORDER MARK / ZERO WIDTH NO-BREAK SPACE
	'\u00AD': true, // SOFT HYPHEN
	'\'':     true,
}

// normalize1255 prepares a value for Windows-1255: NFC (so precomposed and
// decomposed input encode to the same bytes, and Hebrew presentation forms
// become base letter + mark), bidi controls and apostrophes removed, every
// line break (\r\n, \r, \n) written as ". " so a value can never break the
// one-record-per-line file, and a tab or no-break space written as a space.
func normalize1255(s string) string {
	s = norm.NFC.String(s)
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '\r' && strings.HasPrefix(s[i+size:], "\n"):
			b.WriteString(". ")
			size++
		case r == '\r' || r == '\n':
			b.WriteString(". ")
		case r == '\t' || r == '\u00A0': // tab, NO-BREAK SPACE
			b.WriteByte(' ')
		case strippedRunes[r]:
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// trimEdges removes what surrounds a value and is never part of it: whitespace
// (line breaks included) and the invisible characters normalize1255 strips.
// It runs on the RAW value, before normalize1255 — which writes a line break as
// ". " — so a trailing "\n" is dropped rather than written, and a value that is
// nothing but breaks and marks is empty.
func trimEdges(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || strippedRunes[r] })
}

// UnencodableError reports the first character that has no Windows-1255 byte
// (or is a control character, or is not valid UTF-8).
type UnencodableError struct {
	Rune   rune
	Offset int // byte offset in the input
}

// Error names the character by its code point only: the message reaches the
// caller in a 400, which never echoes the value itself.
func (e *UnencodableError) Error() string {
	return fmt.Sprintf("character %U at byte %d cannot be written in Windows-1255", e.Rune, e.Offset)
}

// encode1255Strict encodes s to Windows-1255, one byte per character, and
// fails on the first character it cannot represent. It never substitutes '?'.
// Control characters (U+0000-U+001F, U+007F) are refused as well: Windows-1255
// can carry them, but they have no business inside a fixed-width record.
// Callers normalise first (normalize1255), which turns line breaks and tabs
// into printable text.
func encode1255Strict(s string) ([]byte, error) {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 {
			return nil, &UnencodableError{Rune: utf8.RuneError, Offset: i}
		}
		if r < 0x20 || r == 0x7F {
			return nil, &UnencodableError{Rune: r, Offset: i}
		}
		b, ok := charmap.Windows1255.EncodeRune(r)
		if !ok {
			return nil, &UnencodableError{Rune: r, Offset: i}
		}
		out = append(out, b)
		i += size
	}
	return out, nil
}

// encodeField normalises and strictly encodes one value for a field that may
// hold at most maxBytes, naming the field in any error. It does not pad.
func encodeField(field, value string, maxBytes int) ([]byte, error) {
	b, err := encode1255Strict(normalize1255(value))
	if err != nil {
		return nil, &FieldError{Field: field, Reason: err.Error()}
	}
	if len(b) > maxBytes {
		return nil, &FieldError{
			Field:  field,
			Reason: fmt.Sprintf("is %d bytes in Windows-1255, at most %d allowed", len(b), maxBytes),
		}
	}
	return b, nil
}
