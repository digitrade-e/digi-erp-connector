package imovcompare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
)

// Manifest is the operator's list of orders to compare (manifest.json).
// Paths are relative to the manifest's folder.
type Manifest struct {
	// Allow: PRM lines whose differences are reported but do not fail, e.g.
	// [9, 10, 36] (the dates) for orders re-keyed on a later day. Empty for
	// the acceptance run.
	Allow []int  `json:"allow"`
	Cases []Case `json:"cases"`
}

// Case is one order.
type Case struct {
	Name string `json:"name"`
	// Request: the POST /api/sendOrder body erp-manager sends for the order.
	Request string `json:"request"`
	// Legacy: the legacy app's file for the same order, under its own name
	// (IMOVEIN_<n>.doc, TMP_IMOVEIN_<n>.doc for a return).
	Legacy string `json:"legacy"`
	// Number: the order number to issue; 0 = the legacy file's MyID (field 3).
	Number int64 `json:"number"`
	// Account: the Accounts row for a body that has no "account" (there is
	// no Hashavshevet database here), as a sendOrder "account" object.
	Account json.RawMessage `json:"account"`
}

// CaseReport is one case's outcome.
type CaseReport struct {
	Name        string
	Number      int64
	FileName    string // the connector's
	NameProblem string // the connector named the file otherwise than the legacy
	Result      Result
	Err         error // the order could not be rendered (or a file read)
	Log         []string
}

// OK: rendered, named as the legacy, and matching.
func (c CaseReport) OK() bool { return c.Err == nil && c.NameProblem == "" && c.Result.Match() }

// Report is one manifest run.
type Report struct{ Cases []CaseReport }

// OK: every case is.
func (r Report) OK() bool {
	for _, c := range r.Cases {
		if !c.OK() {
			return false
		}
	}
	return len(r.Cases) > 0
}

func (r Report) String() string {
	var b strings.Builder
	ok := 0
	for _, c := range r.Cases {
		verdict := "FAIL"
		if c.OK() {
			verdict, ok = "OK", ok+1
		}
		fmt.Fprintf(&b, "== %s: %s (order %d", c.Name, verdict, c.Number)
		if c.FileName != "" {
			fmt.Fprintf(&b, ", connector file %s", c.FileName)
		}
		b.WriteString(")\n")
		if c.Err != nil {
			fmt.Fprintf(&b, "  error: %v\n", c.Err)
			for _, l := range c.Log {
				fmt.Fprintf(&b, "  log: %s\n", l)
			}
			continue
		}
		if c.NameProblem != "" {
			fmt.Fprintf(&b, "  problem: %s\n", c.NameProblem)
		}
		for _, l := range strings.Split(strings.TrimRight(c.Result.String(), "\n"), "\n") {
			fmt.Fprintf(&b, "  %s\n", l)
		}
	}
	fmt.Fprintf(&b, "%d of %d cases match\n", ok, len(r.Cases))
	return b.String()
}

// LoadManifest reads and checks a manifest.
func LoadManifest(path string) (Manifest, error) {
	var m Manifest
	b, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("%s: %w", path, err)
	}
	if len(m.Cases) == 0 {
		return m, errors.New("the manifest has no cases")
	}
	written := map[int]bool{}
	for _, f := range hasavshevet.LegacyLayout() {
		written[f.Line] = true
	}
	for _, l := range m.Allow {
		if !written[l] {
			return m, fmt.Errorf("allow: %d is not a field the legacy layout writes", l)
		}
	}
	seen := map[string]bool{}
	for i, c := range m.Cases {
		if c.Name == "" || c.Request == "" || c.Legacy == "" {
			return m, fmt.Errorf("case %d: name, request and legacy are required", i+1)
		}
		if seen[c.Name] {
			return m, fmt.Errorf("case %q twice", c.Name)
		}
		seen[c.Name] = true
	}
	return m, nil
}

// RunManifest renders and compares every case of the manifest at path. A
// case's own failure is in its report; the error is for the manifest itself.
// With out set, each connector file is kept there as <name>.<file name>.
func RunManifest(ctx context.Context, path, out string) (Report, error) {
	m, err := LoadManifest(path)
	if err != nil {
		return Report{}, err
	}
	allow := map[int]bool{}
	for _, l := range m.Allow {
		allow[l] = true
	}
	base := filepath.Dir(path)
	var rep Report
	for _, c := range m.Cases {
		rep.Cases = append(rep.Cases, runCase(ctx, base, out, c, allow))
	}
	return rep, nil
}

func runCase(ctx context.Context, base, out string, c Case, allow map[int]bool) CaseReport {
	cr := CaseReport{Name: c.Name, Number: c.Number}
	legacy, err := os.ReadFile(filepath.Join(base, c.Legacy))
	if err != nil {
		cr.Err = err
		return cr
	}
	body, err := os.ReadFile(filepath.Join(base, c.Request))
	if err != nil {
		cr.Err = err
		return cr
	}
	if cr.Number == 0 {
		if cr.Number, err = LegacyNumber(legacy); err != nil {
			cr.Err = err
			return cr
		}
	}
	got, err := Render(ctx, body, cr.Number, c.Account)
	cr.Log = got.Log
	if err != nil {
		cr.Err = err
		return cr
	}
	cr.FileName = got.FileName
	if want := filepath.Base(c.Legacy); want != got.FileName {
		cr.NameProblem = fmt.Sprintf("the connector named the file %s, the legacy %s", got.FileName, want)
	}
	cr.Result = Compare(legacy, got.Doc, allow)
	if out != "" {
		if err := os.WriteFile(filepath.Join(out, c.Name+"."+got.FileName), got.Doc, 0o600); err != nil {
			cr.Err = err
		}
	}
	return cr
}

// LegacyNumber is the order number (MyID, field 3) of a legacy file's first
// record.
func LegacyNumber(doc []byte) (int64, error) {
	for _, f := range hasavshevet.LegacyLayout() {
		if f.Line != 3 {
			continue
		}
		if len(doc) < f.Offset+f.Width {
			return 0, errors.New("the legacy file is too short to hold an order number")
		}
		text := strings.TrimSpace(string(doc[f.Offset : f.Offset+f.Width]))
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil || n < 1 {
			return 0, fmt.Errorf("the legacy file's field 3 is %q, not an order number", text)
		}
		return n, nil
	}
	return 0, errors.New("the legacy layout has no field 3")
}
