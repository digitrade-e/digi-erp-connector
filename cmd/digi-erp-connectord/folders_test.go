package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/provision"
)

// linkedBox reads link.yaml as provision.LinkMode does.
func TestLinkedBoxIsProvisionsLinkMode(t *testing.T) {
	for _, content := range []string{"", "box: bfl-read\n", "pairCode: ABCDE-FGHJK\n", "relay: wss://x/link\n", "box: ''\n",
		"box: bfl-read\nfuture: 1\n", "{not yaml", "admin: false\n"} {
		dir := t.TempDir()
		if content != "" {
			if err := os.WriteFile(filepath.Join(dir, "link.yaml"), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if got, want := linkedBox(dir), provision.LinkMode(dir); got != want {
			t.Errorf("link.yaml %q: linkedBox %v, provision.LinkMode %v", content, got, want)
		}
	}
}
