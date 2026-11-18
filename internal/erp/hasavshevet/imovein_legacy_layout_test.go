package hasavshevet

import (
	"bytes"
	"fmt"
	"testing"
)

// LegacyLayout is what the byte-compare harness (DIG-1304, internal/imovcompare)
// reads a legacy DOC line by. It must describe exactly the line
// generateLegacyDOC writes and the PRM says: every written field once, in PRM
// order, at the PRM's start, with one separator space after it.
func TestLegacyLayout_MatchesTheWriterAndThePRM(t *testing.T) {
	layout := LegacyLayout()
	if len(layout) != 37 {
		t.Fatalf("written fields = %d, want 37", len(layout))
	}
	next := 0
	for _, f := range layout {
		if f.Offset != next {
			t.Fatalf("field %d at offset %d, want %d", f.Line, f.Offset, next)
		}
		if f.Width <= 0 || f.Title == "" {
			t.Fatalf("field %d: width %d, title %q", f.Line, f.Width, f.Title)
		}
		next = f.Offset + f.Width + 1
	}
	if next != LegacyRecordLength || LegacyRecordLength != 1296 {
		t.Fatalf("record = %d, LegacyRecordLength = %d, want 1296", next, LegacyRecordLength)
	}

	// The PRM's "start end" for each written field is offset+1 .. offset+width.
	prm := generateLegacyPRM()
	for _, f := range layout {
		want := []byte(fmt.Sprintf("\n%d %d ;", f.Offset+1, f.Offset+f.Width))
		if !bytes.Contains(prm, want) {
			t.Errorf("PRM has no %q for field %d", want, f.Line)
		}
	}

	// A value written into one field shows at its span and nowhere else.
	h := stockHeader{AccountKey: "K", Remarks: "R"}
	doc := generateLegacyDOC(h, []stockMove{{ItemKey: "I"}})
	for _, c := range []struct {
		line int
		want byte
	}{{2, 'K'}, {37, 'R'}, {22, 'I'}} {
		for _, f := range layout {
			if f.Line == c.line && doc[f.Offset] != c.want {
				t.Errorf("field %d starts with %q, want %q", c.line, doc[f.Offset], c.want)
			}
		}
	}
}

// LegacyLayout hands out a copy: a caller cannot change the writer's table.
func TestLegacyLayout_IsACopy(t *testing.T) {
	a := LegacyLayout()
	a[0].Width = 99
	if LegacyLayout()[0].Width == 99 {
		t.Fatal("LegacyLayout returned the writer's own table")
	}
}
