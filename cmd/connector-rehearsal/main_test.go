package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun_SnapshotAndCheck(t *testing.T) {
	data, send, out := t.TempDir(), t.TempDir(), t.TempDir()
	counter := filepath.Join(send, "lastOrderNumber.json")
	if err := os.WriteFile(counter, []byte(`{"lastOrderNumber": 7}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before, after := filepath.Join(out, "before.json"), filepath.Join(out, "after.json")
	var o, e bytes.Buffer
	if code := run([]string{"snapshot", "-data", data, "-send", send, "-out", before}, &o, &e); code != 0 {
		t.Fatalf("snapshot: %d %s", code, e.String())
	}
	if code := run([]string{"check", before, before}, &o, &e); code != 0 {
		t.Fatalf("check same: %d %s", code, o.String())
	}
	if err := os.WriteFile(counter, []byte(`{"lastOrderNumber": 3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"snapshot", "-data", data, "-send", send, "-out", after}, &o, &e); code != 0 {
		t.Fatalf("snapshot: %d %s", code, e.String())
	}
	o.Reset()
	if code := run([]string{"check", before, after}, &o, &e); code != 1 || !strings.Contains(o.String(), "went backwards: 7 → 3") {
		t.Fatalf("check backwards: %d %s", code, o.String())
	}
	for _, args := range [][]string{nil, {"snapshot", "-data", data}, {"snapshot", "-data", "/nonexistent", "-send", send, "-out", before}, {"check", before}, {"check", before, "/nonexistent"}, {"restore"}} {
		if code := run(args, &o, &e); code != 2 {
			t.Fatalf("%v: exit %d, want 2", args, code)
		}
	}
}
