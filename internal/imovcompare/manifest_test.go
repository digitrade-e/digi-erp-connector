package imovcompare

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCase lays out one case the way the operator does: a body file, a
// legacy file under its legacy name, and the manifest beside them.
func writeManifest(t *testing.T, dir string, m map[string]any) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeFile(t *testing.T, dir, name string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The number comes from the legacy file's MyID (field 3) when the case does
// not give one; paths are relative to the manifest; the connector's file is
// kept in out for a look.
func TestRunManifest_MatchAndNumberFromTheLegacyFile(t *testing.T) {
	dir, out := t.TempDir(), t.TempDir()
	writeFile(t, dir, "req/order.json", goldenBody(t, "order", false))
	writeFile(t, dir, "req/return.json", goldenBody(t, "return", true))
	writeFile(t, dir, "legacy/IMOVEIN_100001.doc", legacyGolden(t, "order.doc"))
	writeFile(t, dir, "legacy/TMP_IMOVEIN_100003.doc", legacyGolden(t, "return.doc"))
	p := writeManifest(t, dir, map[string]any{"cases": []map[string]any{
		{"name": "order", "request": "req/order.json", "legacy": "legacy/IMOVEIN_100001.doc", "account": json.RawMessage(goldenAccount)},
		{"name": "return", "request": "req/return.json", "legacy": "legacy/TMP_IMOVEIN_100003.doc"},
	}})
	rep, err := RunManifest(context.Background(), p, out)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() || len(rep.Cases) != 2 {
		t.Fatalf("report:\n%s", rep)
	}
	for _, c := range rep.Cases {
		if !c.Result.Identical || c.Number == 0 {
			t.Fatalf("case %s: %+v", c.Name, c)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "return.TMP_IMOVEIN_100003.doc")); err != nil {
		t.Fatalf("connector file not kept: %v", err)
	}
}

// A file name that is not the legacy's (a return without TMP_) fails even
// when the bytes match; a render error fails its case, not the run; allowed
// fields come from the manifest.
func TestRunManifest_Failures(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "order.json", goldenBody(t, "order", true))
	writeFile(t, dir, "noacc.json", goldenBody(t, "order", false))
	writeFile(t, dir, "TMP_IMOVEIN_100001.doc", legacyGolden(t, "order.doc"))
	writeFile(t, dir, "IMOVEIN_100001.doc", put(t, put(t, legacyGolden(t, "order.doc"), 1, 9, "01/01/2026"), 2, 9, "01/01/2026"))
	p := writeManifest(t, dir, map[string]any{"allow": []int{9}, "cases": []map[string]any{
		{"name": "wrong-name", "request": "order.json", "legacy": "TMP_IMOVEIN_100001.doc"},
		{"name": "no-account", "request": "noacc.json", "legacy": "IMOVEIN_100001.doc"},
		{"name": "date-allowed", "request": "order.json", "legacy": "IMOVEIN_100001.doc"},
	}})
	rep, err := RunManifest(context.Background(), p, "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() {
		t.Fatalf("report OK:\n%s", rep)
	}
	byName := map[string]CaseReport{}
	for _, c := range rep.Cases {
		byName[c.Name] = c
	}
	if c := byName["wrong-name"]; c.OK() || !strings.Contains(c.NameProblem, "TMP_IMOVEIN_100001.doc") {
		t.Fatalf("wrong-name: %+v", c)
	}
	if c := byName["no-account"]; c.OK() || c.Err == nil {
		t.Fatalf("no-account: %+v", c)
	}
	if c := byName["date-allowed"]; !c.OK() || c.Result.Identical || len(c.Result.Allowed) != 2 {
		t.Fatalf("date-allowed: %+v", c)
	}
	s := rep.String()
	for _, want := range []string{"wrong-name", "no-account", "date-allowed", "1 of 3"} {
		if !strings.Contains(s, want) {
			t.Fatalf("report lacks %q:\n%s", want, s)
		}
	}
}

func TestRunManifest_BadManifest(t *testing.T) {
	dir := t.TempDir()
	for name, m := range map[string]map[string]any{
		"no cases":     {"cases": []any{}},
		"duplicate":    {"cases": []map[string]any{{"name": "a", "request": "x", "legacy": "y"}, {"name": "a", "request": "x", "legacy": "y"}}},
		"missing path": {"cases": []map[string]any{{"name": "a", "legacy": "y"}}},
		"bad allow":    {"allow": []int{14}, "cases": []map[string]any{{"name": "a", "request": "x", "legacy": "y"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := RunManifest(context.Background(), writeManifest(t, dir, m), ""); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}
