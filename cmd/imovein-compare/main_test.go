package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun_ExitCodes(t *testing.T) {
	golden := filepath.Join("..", "..", "internal", "erp", "hasavshevet", "testdata", "imovein-legacy")
	order, ret := filepath.Join(golden, "order.doc"), filepath.Join(golden, "return.doc")
	empty := filepath.Join(t.TempDir(), "m.json")
	if err := os.WriteFile(empty, []byte(`{"cases":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		args []string
		code int
		out  string
	}{
		{"same file", []string{"diff", order, order}, 0, "IDENTICAL"},
		{"different", []string{"diff", order, ret}, 1, "DIFFERENT"},
		{"no args", nil, 2, ""},
		{"one file", []string{"diff", order}, 2, ""},
		{"bad allow", []string{"diff", "-allow", "x", order, order}, 2, ""},
		{"missing file", []string{"diff", order, "/nonexistent"}, 2, ""},
		{"empty manifest", []string{"run", empty}, 2, ""},
		{"unknown", []string{"render"}, 2, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := run(c.args, &out, &errb); code != c.code {
				t.Fatalf("exit %d, want %d; stdout %s stderr %s", code, c.code, out.String(), errb.String())
			}
			if !strings.Contains(out.String(), c.out) {
				t.Fatalf("stdout %q lacks %q", out.String(), c.out)
			}
		})
	}
}
