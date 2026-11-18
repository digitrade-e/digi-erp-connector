package imovcompare

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
)

// The legacy writer's own output (DIG-1297's goldens, made by running the
// legacy PHP): a two-record order.
func legacyGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "erp", "hasavshevet", "testdata", "imovein-legacy", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func field(t *testing.T, line int) hasavshevet.LegacyField {
	t.Helper()
	for _, f := range hasavshevet.LegacyLayout() {
		if f.Line == line {
			return f
		}
	}
	t.Fatalf("no written field %d", line)
	return hasavshevet.LegacyField{}
}

// put writes text, padded with spaces, into field line of record rec (1-based).
func put(t *testing.T, doc []byte, rec, line int, text string) []byte {
	t.Helper()
	out := bytes.Clone(doc)
	f := field(t, line)
	start := (rec-1)*(hasavshevet.LegacyRecordLength+1) + f.Offset
	copy(out[start:start+f.Width], bytes.Repeat([]byte{' '}, f.Width))
	copy(out[start:], text)
	return out
}

func TestCompare_IdenticalFiles(t *testing.T) {
	doc := legacyGolden(t, "order.doc")
	r := Compare(doc, doc, nil)
	if !r.Identical || !r.Match() || len(r.Diffs) != 0 || len(r.Problems) != 0 {
		t.Fatalf("identical files: %+v", r)
	}
	if r.Records != 2 {
		t.Fatalf("records = %d, want 2", r.Records)
	}
}

// A difference is named by record, PRM line and title, with both values
// decoded from Windows-1255 and the byte offset in the file.
func TestCompare_NamesTheField(t *testing.T) {
	want := legacyGolden(t, "order.doc")
	got := put(t, want, 2, 24, "11.00")
	r := Compare(want, got, nil)
	if r.Identical || r.Match() {
		t.Fatalf("a changed price matched: %+v", r)
	}
	if len(r.Diffs) != 1 {
		t.Fatalf("diffs = %+v, want one", r.Diffs)
	}
	d := r.Diffs[0]
	f := field(t, 24)
	if d.Record != 2 || d.Line != 24 || d.Title != f.Title || d.Got != "11.00" || d.Want != "10.00" {
		t.Fatalf("diff = %+v", d)
	}
	if d.FileOffset != hasavshevet.LegacyRecordLength+1+f.Offset {
		t.Fatalf("file offset = %d", d.FileOffset)
	}
	if !strings.Contains(r.String(), "record 2 field 24") {
		t.Fatalf("report does not name it:\n%s", r.String())
	}
}

// Hebrew text is shown decoded, not as bytes.
func TestCompare_DecodesHebrew(t *testing.T) {
	want := legacyGolden(t, "order.doc")
	got := put(t, want, 1, 28, "\xee\xe5\xf6\xf8") // מוצר
	r := Compare(want, got, nil)
	if len(r.Diffs) != 1 || r.Diffs[0].Got != "מוצר" || r.Diffs[0].Want != "מוצר א" {
		t.Fatalf("diffs = %+v", r.Diffs)
	}
}

// An allowed field (e.g. the date of an order re-keyed on another day) is
// reported but does not fail the match; the files are still not identical.
func TestCompare_AllowedField(t *testing.T) {
	want := legacyGolden(t, "order.doc")
	got := want
	for rec := 1; rec <= 2; rec++ {
		got = put(t, got, rec, 9, "18/11/2026")
	}
	r := Compare(want, got, map[int]bool{9: true})
	if r.Identical || !r.Match() || len(r.Diffs) != 0 || len(r.Allowed) != 2 {
		t.Fatalf("allowed: %+v", r)
	}
	r = Compare(want, got, nil)
	if r.Match() || len(r.Diffs) != 2 {
		t.Fatalf("not allowed: %+v", r)
	}
}

func TestCompare_StructuralProblems(t *testing.T) {
	order := legacyGolden(t, "order.doc")
	firstRecord := order[:hasavshevet.LegacyRecordLength+1]
	sep := field(t, 2).Offset + field(t, 2).Width
	badSep := bytes.Clone(order)
	badSep[sep] = 'X'
	crlf := bytes.ReplaceAll(order, []byte("\n"), []byte("\r\n"))
	official, err := os.ReadFile(filepath.Join("..", "erp", "hasavshevet", "testdata", "imovein", "order-ils.doc"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name      string
		want, got []byte
		problem   string
	}{
		{"record count", order, firstRecord, "2 records, got 1"},
		{"no final LF", order, order[:len(order)-1], "does not end with LF"},
		{"CRLF", order, crlf, "CR"},
		{"separator", order, badSep, "separator after field 2"},
		{"official layout", order, official, "not the legacy layout"},
		{"empty", order, nil, "empty"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := Compare(c.want, c.got, nil)
			if r.Match() || r.Identical {
				t.Fatalf("matched: %+v", r)
			}
			if !strings.Contains(strings.Join(r.Problems, "\n"), c.problem) {
				t.Fatalf("problems = %q, want one with %q", r.Problems, c.problem)
			}
		})
	}
}
