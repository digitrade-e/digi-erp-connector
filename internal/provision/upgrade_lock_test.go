package provision

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/link"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/filelock"
)

// Review M1: while an upgrade over the link is pending (its job written, its runner alive or about to start) no
// provisioning run starts — WinRM's included — and the link and provision agree on the one lock file.
func TestNoProvisionRunDuringAnUpgrade(t *testing.T) {
	dir := t.TempDir()
	if got, want := link.ProvisionLockPath(dir), filepath.Join(provisionDir(dir), ".lock"); got != want {
		t.Fatalf("the link locks %s, provision %s", got, want)
	}
	unlock, err := lockRuns(dir)
	if err != nil {
		t.Fatal(err)
	}
	unlock()

	ticket := strings.Repeat("ab12", 8)
	tdir := filepath.Join(dir, "upgrade", ticket)
	if err := os.MkdirAll(tdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tdir, "job.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = lockRuns(dir)
	var r *Refusal
	if !errors.As(err, &r) || r.Code != CodeUpgradeRunning || !strings.Contains(r.Message, ticket) {
		t.Fatalf("a run started during an upgrade: %v", err)
	}
	// the refused run let the lock go: the runner takes it
	h, err := filelock.Lock(link.ProvisionLockPath(dir))
	if err != nil {
		t.Fatalf("the refused run kept the lock: %v", err)
	}
	_ = h.Unlock()
	// the runner ended long ago without a result: provision runs again
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(filepath.Join(tdir, "job.json"), old, old)
	unlock, err = lockRuns(dir)
	if err != nil {
		t.Fatalf("a lost upgrade still blocks: %v", err)
	}
	unlock()
}
