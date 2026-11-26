package hasavshevet

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// DIG-1551: a line's note goes to IMOVEIN line71 (פרטים בתנועה, 20 characters),
// refused — never cut — when it does not fit.

func TestLineNote_FieldIs71And20Wide(t *testing.T) {
	if got := lineNoteWidth(); got != 20 {
		t.Fatalf("line71 width = %d; want 20 (Hasavshevet IMOVEIN פרטים בתנועה)", got)
	}
}

// No note on any line writes exactly the goldens' bytes: today's tenants see no change.
func TestLineNote_NoneIsTheGoldenBytes(t *testing.T) {
	for _, tc := range imoveinGoldenCases() {
		req := tc.req
		for i := range req.Details {
			req.Details[i].Note = ""
		}
		if got, want := renderWith(t, "", req, tc.number), readFile(t, goldenPath(tc.name+".doc")); !bytes.Equal(got, want) {
			t.Errorf("%s: bytes differ from the golden%s", tc.name, firstDiff(got, want))
		}
	}
}

// Each line's note lands in line71 of its own record, Hebrew round-tripping
// through Windows-1255; every other byte is the golden's.
func TestLineNote_LandsInLine71OfItsRecord(t *testing.T) {
	tc := imoveinGoldenCases()[0]
	if len(tc.req.Details) < 1 {
		t.Fatal("golden case has no lines")
	}
	req := tc.req
	req.Details = append([]OrderLineItem(nil), tc.req.Details...)
	notes := make([]string, len(req.Details))
	for i := range req.Details {
		notes[i] = []string{"לעטוף כמתנה", "בלי ורוד", "2 צבעים שונים"}[i%3]
		req.Details[i].Note = notes[i]
	}
	doc := renderWith(t, "", req, tc.number)
	golden := readFile(t, goldenPath(tc.name+".doc"))
	if len(doc) != len(golden) {
		t.Fatalf("length %d; golden %d", len(doc), len(golden))
	}
	rec := prmTotalRecordLength + 1
	start, end := imoveinSpan("line71")
	for r := 0; r*rec < len(doc); r++ {
		if got := headerText1255(t, doc[r*rec+start:r*rec+end]); got != notes[r] {
			t.Errorf("record %d line71 = %q; want %q", r, got, notes[r])
		}
	}
	for i := range doc {
		at := i % rec
		if (at < start || at >= end) && doc[i] != golden[i] {
			t.Fatalf("byte %d (%s) changed outside line71", i, fieldAt(at))
		}
	}
}

func TestLineNote_FullWidthHebrewFillsTheField(t *testing.T) {
	tc := imoveinGoldenCases()[0]
	req := tc.req
	req.Details = append([]OrderLineItem(nil), tc.req.Details...)
	full := strings.Repeat("ש", 20)
	req.Details[0].Note = full
	doc := renderWith(t, "", req, tc.number)
	start, end := imoveinSpan("line71")
	raw := doc[start:end]
	if bytes.IndexByte(raw, ' ') != -1 {
		t.Fatalf("a 20-letter note left padding in a 20-byte field: %q", raw)
	}
	if got := headerText1255(t, raw); got != full {
		t.Fatalf("line71 = %q; want %q", got, full)
	}
}

func TestValidateLineNotes(t *testing.T) {
	lines := func(notes ...string) []OrderLineItem {
		out := make([]OrderLineItem, len(notes))
		for i, n := range notes {
			out[i] = OrderLineItem{SKU: "A", Title: "a", Quantity: 1, Note: n}
		}
		return out
	}
	for _, tc := range []struct {
		name    string
		notes   []string
		wantErr string // "" = accepted; else the field it names
	}{
		{"none", []string{"", ""}, ""},
		{"20 Hebrew letters fit", []string{strings.Repeat("א", 20)}, ""},
		{"20 ASCII fit", []string{strings.Repeat("x", 20)}, ""},
		{"21 refused, never cut", []string{strings.Repeat("א", 21)}, "details[0]/note"},
		{"second line named", []string{"ok", strings.Repeat("x", 21)}, "details[1]/note"},
		{"line break counts as two", []string{strings.Repeat("x", 18) + "\ny"}, "details[0]/note"},
		{"line break that fits", []string{strings.Repeat("x", 17) + "\ny"}, ""},
		{"quotes dropped before counting", []string{strings.Repeat("x", 20) + "'"}, ""},
		{"spaces only", []string{"   "}, "details[0]/note"},
		{"tab", []string{"a\tb"}, "details[0]/note"},
		{"NUL", []string{"a\x00b"}, "details[0]/note"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateLineNotes(lines(tc.notes...))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			var fe *FieldError
			if !errors.As(err, &fe) || fe.Field != tc.wantErr {
				t.Fatalf("err = %v; want a FieldError on %s", err, tc.wantErr)
			}
		})
	}
}

// The writer refuses an over-long note before any file exists (other callers
// than the route), and the order counts as not written.
func TestSender_RefusesAnOverlongLineNoteBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	s := NewSender(nil, config.Config{SendOrderDir: dir}, NewOrderNumberStore(filepath.Join(dir, "lastOrderNumber.json")), noopLogger{})
	req := imoveinGoldenCases()[0].req
	req.Details = append([]OrderLineItem(nil), req.Details...)
	req.Details[0].Note = strings.Repeat("ת", 21)
	_, err := s.processOrderWithNumber(context.Background(), req, 9)
	if err == nil || !strings.Contains(err.Error(), "details[0]/note") {
		t.Fatalf("processOrderWithNumber = %v; want refused on details[0]/note", err)
	}
	if !errors.Is(err, ErrOrderNotWritten) {
		t.Fatalf("not marked ErrOrderNotWritten: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("files were written: %v", entries)
	}
}

// The legacy layout has no field 71: any note is refused, never dropped.
func TestValidateLegacyFit_RefusesALineNote(t *testing.T) {
	tc := legacyRequestCases()[0]
	req := tc.req
	if err := ValidateLegacyFit(req); err != nil {
		t.Fatalf("the legacy case without notes is refused: %v", err)
	}
	req.Details = append([]OrderLineItem(nil), tc.req.Details...)
	req.Details[len(req.Details)-1].Note = "x"
	var fe *FieldError
	if err := ValidateLegacyFit(req); !errors.As(err, &fe) || !strings.HasSuffix(fe.Field, "/note") {
		t.Fatalf("ValidateLegacyFit = %v; want a FieldError on the line's note", err)
	}
}

// A request without notes hashes as before DIG-1551 (the DIG-1291 index
// compares request hashes on a repeat); a note changes it.
func TestOrderRequestHash_UnchangedWithoutLineNotes(t *testing.T) {
	req := imoveinGoldenCases()[0].req
	req.Header = nil
	want := strings.TrimSpace(string(readFile(t, goldenPath("request-sha256.txt"))))
	if got := orderRequestHash(req); got != want {
		t.Fatalf("orderRequestHash = %s; golden %s", got, want)
	}
	req.Details = append([]OrderLineItem(nil), req.Details...)
	req.Details[0].Note = "מתנה"
	if orderRequestHash(req) == want {
		t.Fatal("a line note does not change the hash: a changed repeat would not be noticed")
	}
}
