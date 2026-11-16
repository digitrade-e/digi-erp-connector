package provision

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// contract is a golden file: the JSON paths an OLDER build reads from what
// this build writes, and their types (bool, string, number, time).
type contract struct {
	Reader string            `json:"reader"`
	Paths  map[string]string `json:"paths"`
}

func readContract(t *testing.T, name string) contract {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "contract", name))
	if err != nil {
		t.Fatal(err)
	}
	var c contract
	if err := json.Unmarshal(b, &c); err != nil || len(c.Paths) == 0 {
		t.Fatalf("%s: %v", name, err)
	}
	return c
}

// at is the value at a dotted path of a decoded JSON document.
func at(doc any, path string) (any, bool) {
	for _, k := range strings.Split(path, ".") {
		m, ok := doc.(map[string]any)
		if !ok {
			return nil, false
		}
		if doc, ok = m[k]; !ok {
			return nil, false
		}
	}
	return doc, true
}

// typeOK says whether v is of the contract's type.
func typeOK(v any, want string) bool {
	switch want {
	case "bool":
		_, ok := v.(bool)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "time":
		s, ok := v.(string)
		if !ok {
			return false
		}
		_, err := time.Parse(time.RFC3339Nano, s)
		return err == nil
	}
	return false
}

// Plan 088 review m8 (v1.0.32): the contract with the upgrade runner of an
// OLDER build. It runs this build's provision.exe state to judge the
// upgrade, so every field it reads must stay, under its name and type: the
// golden file is that reader's view, and the test fails when a build drops
// or renames one. Two states cover every field (the error only when the
// daemon refuses): each path present in one at least, and of its type
// wherever present.
func TestContract_StateForTheOlderRunner(t *testing.T) {
	c := readContract(t, "state-runner-v1.0.31.json")
	sys, _, _ := handInstallBox(t)
	sys.daemon.set(func(d *fakeDaemon) { d.queued, d.running, d.health = 2, 1, 200 })
	_, _, asked := runState(t, stateSys{fakeSys: sys}, testEnv(sys, &fakeDB{}), `{}`)
	sys.daemon.set(func(d *fakeDaemon) { d.pass = "another" })
	_, _, refused := runState(t, stateSys{fakeSys: sys}, testEnv(sys, &fakeDB{}), `{}`)
	seen := map[string]bool{}
	for _, out := range [][]byte{asked, refused} {
		var doc any
		if err := json.Unmarshal(out, &doc); err != nil {
			t.Fatalf("state printed %s: %v", out, err)
		}
		for path, typ := range c.Paths {
			v, ok := at(doc, path)
			if !ok {
				continue
			}
			seen[path] = true
			if !typeOK(v, typ) {
				t.Errorf("%s is %T (%v), the older runner reads a %s", path, v, v, typ)
			}
		}
	}
	for path := range c.Paths {
		if !seen[path] {
			t.Errorf("state no longer writes %s, which the older runner reads (%s)", path, c.Reader)
		}
	}
}
