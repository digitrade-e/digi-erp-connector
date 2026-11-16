package link

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Plan 088 review m8 (v1.0.32): link\session.json as the upgrade runner of an
// OLDER build reads it (testdata/contract): a build that drops or renames a
// field it reads, or changes its type, fails here — not in the field, where
// the build before would roll the new one back for it. The marker is written
// by the link's own writer (writeSession) and read back from disk.
func TestContract_SessionMarkerForTheOlderRunner(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "contract", "session-runner-v1.0.31.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Reader string            `json:"reader"`
		Paths  map[string]string `json:"paths"`
	}
	if err := json.Unmarshal(b, &c); err != nil || len(c.Paths) == 0 {
		t.Fatal(err)
	}
	dir := t.TempDir()
	now := time.Date(2026, 11, 17, 4, 0, 0, 0, time.UTC)
	m := SessionMarker{Build: "v1.0.32", Connected: true, Epoch: 7, Relay: "wss://relay.example.org/link", ConnectedAt: now, LiveAt: now.Add(time.Minute)}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeLinkFile(dir, sessionPath(dir), raw); err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(sessionPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(onDisk, &doc); err != nil {
		t.Fatal(err)
	}
	for path, typ := range c.Paths {
		v, ok := doc[path]
		if !ok {
			t.Errorf("session.json no longer holds %s, which the older runner reads (%s)", path, c.Reader)
			continue
		}
		ok = false
		switch typ {
		case "bool":
			_, ok = v.(bool)
		case "string":
			_, ok = v.(string)
		case "number":
			_, ok = v.(float64)
		case "time":
			if s, isString := v.(string); isString {
				_, err := time.Parse(time.RFC3339Nano, s)
				ok = err == nil
			}
		}
		if !ok {
			t.Errorf("session.json's %s is %T (%v), the older runner reads a %s", path, v, v, typ)
		}
	}
	// And the marker reads back as the runner reads it.
	if got, err := ReadSessionMarker(dir); err != nil || !got.Connected || got.Build != "v1.0.32" || !got.LiveAt.Equal(now.Add(time.Minute)) {
		t.Errorf("read back %+v %v", got, err)
	}
}
