// Package imovcompare is the byte-compare harness of DIG-1304 (plan 075
// P1-6): before BFL's first live order through the platform, the IMOVEIN file
// the connector writes for an order must be byte for byte the file the legacy
// Digitrade app wrote for the same order (sync/herp_files/<sifrur>/
// IMOVEIN_<n>.doc on the legacy server).
//
// Compare reads both files by the legacy layout (hasavshevet.LegacyLayout)
// and names every difference by record, PRM line and title. Render runs a
// sendOrder body through the connector's real route, queue and writer in a
// throwaway folder and returns the file it published. The CLI is
// cmd/imovein-compare; the procedure is docs/order-cutover-proof.md.
//
// Real legacy files carry customer data and a client's prices: they stay on
// the operator's machine and are never committed (the repo is public).
package imovcompare

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/text/encoding/charmap"

	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
)

// FieldDiff is one field that differs: in which record (1-based) and field
// (the PRM line), at which byte of the file, and both values decoded from
// Windows-1255 with the padding trimmed.
type FieldDiff struct {
	Record     int
	Line       int
	Title      string
	FileOffset int
	Want       string
	Got        string
}

// Result is one comparison.
type Result struct {
	// Identical: the two files are the same bytes.
	Identical bool
	// Records is the number of records in the wanted (legacy) file.
	Records int
	// Problems are differences of shape: record count, line length, line
	// ends, separators. Any one fails the match.
	Problems []string
	// Diffs are field differences outside the allowed fields.
	Diffs []FieldDiff
	// Allowed are field differences in fields the caller allowed (e.g. the
	// date of an order re-keyed on another day). Reported, never failing.
	Allowed []FieldDiff
}

// Match: no problem and no difference outside the allowed fields.
func (r Result) Match() bool { return r.Identical || (len(r.Problems) == 0 && len(r.Diffs) == 0) }

// String is the human report.
func (r Result) String() string {
	var b strings.Builder
	switch {
	case r.Identical:
		fmt.Fprintf(&b, "IDENTICAL (%d records)\n", r.Records)
		return b.String()
	case r.Match():
		fmt.Fprintf(&b, "MATCH except allowed fields (%d records)\n", r.Records)
	default:
		fmt.Fprintf(&b, "DIFFERENT (%d records in the legacy file)\n", r.Records)
	}
	for _, p := range r.Problems {
		fmt.Fprintf(&b, "  problem: %s\n", p)
	}
	for _, d := range r.Diffs {
		fmt.Fprintf(&b, "  record %d field %d (%s) at byte %d: legacy %q, connector %q\n", d.Record, d.Line, d.Title, d.FileOffset, d.Want, d.Got)
	}
	for _, d := range r.Allowed {
		fmt.Fprintf(&b, "  allowed: record %d field %d (%s): legacy %q, connector %q\n", d.Record, d.Line, d.Title, d.Want, d.Got)
	}
	return b.String()
}

// Compare compares the connector's file (got) with the legacy one (want).
// allow holds PRM line numbers whose differences are reported but allowed.
func Compare(want, got []byte, allow map[int]bool) Result {
	r := Result{Identical: len(want) > 0 && bytes.Equal(want, got)}
	wantRecs, wantProblems := records(want)
	r.Records = len(wantRecs)
	if r.Identical {
		return r
	}
	gotRecs, gotProblems := records(got)
	for _, p := range wantProblems {
		r.Problems = append(r.Problems, "legacy file: "+p)
	}
	for _, p := range gotProblems {
		r.Problems = append(r.Problems, "connector file: "+p)
	}
	if len(wantRecs) != len(gotRecs) {
		r.Problems = append(r.Problems, fmt.Sprintf("record count: legacy %d records, got %d", len(wantRecs), len(gotRecs)))
	}
	layout := hasavshevet.LegacyLayout()
	stride := hasavshevet.LegacyRecordLength + 1
	for i := 0; i < len(wantRecs) && i < len(gotRecs); i++ {
		w, g := wantRecs[i], gotRecs[i]
		if len(w) != hasavshevet.LegacyRecordLength || len(g) != hasavshevet.LegacyRecordLength {
			continue // already a problem; its fields cannot be placed
		}
		for _, f := range layout {
			wv, gv := w[f.Offset:f.Offset+f.Width], g[f.Offset:f.Offset+f.Width]
			if !bytes.Equal(wv, gv) {
				d := FieldDiff{
					Record: i + 1, Line: f.Line, Title: f.Title, FileOffset: i*stride + f.Offset,
					Want: decode(wv), Got: decode(gv),
				}
				if allow[f.Line] {
					r.Allowed = append(r.Allowed, d)
				} else {
					r.Diffs = append(r.Diffs, d)
				}
			}
		}
	}
	if !r.Match() || len(r.Allowed) > 0 {
		return r
	}
	// Every field equal, no problem, yet the bytes differ: name it rather
	// than call it a match.
	r.Problems = append(r.Problems, "the files differ outside every field (a byte the layout does not cover)")
	return r
}

// records splits a legacy DOC into its records and names what is not the
// legacy shape: LF-terminated lines of LegacyRecordLength bytes, one space
// after every field.
func records(doc []byte) ([][]byte, []string) {
	if len(doc) == 0 {
		return nil, []string{"empty"}
	}
	var problems []string
	if bytes.IndexByte(doc, '\r') >= 0 {
		problems = append(problems, "has CR bytes (CRLF line ends?); the legacy writes LF only")
	}
	if doc[len(doc)-1] != '\n' {
		problems = append(problems, "does not end with LF")
	}
	lines := bytes.Split(bytes.TrimSuffix(doc, []byte("\n")), []byte("\n"))
	lengths := map[int][]int{}
	for i, l := range lines {
		l = bytes.TrimSuffix(l, []byte("\r"))
		lines[i] = l
		if len(l) != hasavshevet.LegacyRecordLength {
			lengths[len(l)] = append(lengths[len(l)], i+1)
		}
	}
	if len(lengths) > 0 {
		var ls []int
		for n := range lengths {
			ls = append(ls, n)
		}
		sort.Ints(ls)
		for _, n := range ls {
			problems = append(problems, fmt.Sprintf("not the legacy layout: records %v are %d bytes, not %d", lengths[n], n, hasavshevet.LegacyRecordLength))
		}
		return lines, problems
	}
	for i, l := range lines {
		for _, f := range hasavshevet.LegacyLayout() {
			if s := f.Offset + f.Width; l[s] != ' ' {
				problems = append(problems, fmt.Sprintf("record %d: separator after field %d is %q, not a space", i+1, f.Line, l[s]))
			}
		}
	}
	return lines, problems
}

// decode shows a field's bytes as text: Windows-1255, trailing padding off.
func decode(b []byte) string {
	b = bytes.TrimRight(b, " ")
	s, err := charmap.Windows1255.NewDecoder().Bytes(b)
	if err != nil {
		return fmt.Sprintf("%x", b)
	}
	return string(s)
}

// Show lists every written field of every record of a legacy DOC, decoded,
// one per line; an error when the file is not the legacy shape.
func Show(doc []byte) (string, error) {
	recs, problems := records(doc)
	if len(problems) > 0 {
		return "", errors.New(strings.Join(problems, "; "))
	}
	var b strings.Builder
	for i, r := range recs {
		fmt.Fprintf(&b, "record %d of %d\n", i+1, len(recs))
		for _, f := range hasavshevet.LegacyLayout() {
			fmt.Fprintf(&b, "  field %d (%s): %s\n", f.Line, f.Title, decode(r[f.Offset:f.Offset+f.Width]))
		}
	}
	return b.String(), nil
}
