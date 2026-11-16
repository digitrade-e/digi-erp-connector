package hasavshevet

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
)

// guardBox is a box with every kind of document folder, and a writers reader
// the test drives.
type guardBox struct {
	cfg  config.Config
	mu   sync.Mutex
	who  map[string][]dataacl.Writer // by folder base name
	errs map[string]error
	gate chan struct{} // when set, reads of "slow" wait on it
	log  *recordingLogger
	now  time.Time
}

var (
	usersW     = dataacl.Writer{SID: "S-1-5-32-545", Name: `BUILTIN\Users`}
	schedulerW = dataacl.Writer{SID: "S-1-5-21-1-2-3-1105", Name: `BFL\wizsched`}
)

func newGuardBox(t *testing.T) *guardBox {
	root := t.TempDir()
	b := &guardBox{log: &recordingLogger{}, who: map[string][]dataacl.Writer{}, errs: map[string]error{}, now: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	b.cfg = config.Config{
		ERP: config.ERPHasavshevet, SendOrderDir: filepath.Join(root, "orders"), SendOrderPickupDir: filepath.Join(root, "pickup"),
		SendOrderFormat: config.OrderFormatLegacy,
		Documents: config.DocumentsConfig{
			Folders: map[string]config.FolderConfig{
				"herp":          {Kind: config.DocumentKindOrders, Path: filepath.Join(root, "herp"), NamePattern: "IMOVEIN_{n}.doc", Layout: config.OrderFormatLegacy},
				"herp_test":     {Kind: config.DocumentKindOrders, Path: filepath.Join(root, "herp_test"), NamePattern: "IMOVEIN_{n}.doc", Layout: config.OrderFormatLegacy, Test: true},
				"slow":          {Kind: config.DocumentKindOrders, Path: filepath.Join(root, "slow"), NamePattern: "S_{n}.doc", Layout: config.OrderFormatLegacy},
				"herp_receipts": {Kind: config.DocumentKindReceipts, Path: filepath.Join(root, "kupain"), NamePattern: "KUPAIN_{n}.doc"},
			},
			Receipts:       config.DocumentReceiptsConfig{Folder: "herp_receipts"},
			TrustedWriters: []config.TrustedWriter{{SID: schedulerW.SID, Name: schedulerW.Name}},
		},
	}
	return b
}

func (b *guardBox) writers(dir string) ([]dataacl.Writer, error) {
	base := filepath.Base(dir)
	b.mu.Lock()
	gate := b.gate
	b.mu.Unlock()
	if base == "slow" && gate != nil {
		<-gate
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.who[base], b.errs[base]
}

func (b *guardBox) guard(linked bool) *FolderGuard {
	g := NewFolderGuard(b.cfg, linked, b.writers, b.log)
	g.now = func() time.Time { b.mu.Lock(); defer b.mu.Unlock(); return b.now }
	return g
}

func (b *guardBox) set(f func(b *guardBox)) { b.mu.Lock(); f(b); b.mu.Unlock() }

// Review R3: until it is read a typed folder behind the link is refused (fail
// closed); a refused folder is read again at most once a minute, lazily, when
// an order or a receipt needs it; an allowed one every ten minutes. Trusted
// writers never count; the folders that predate plan 087 are never refused.
func TestFolderGuard(t *testing.T) {
	b := newGuardBox(t)
	b.who = map[string][]dataacl.Writer{
		"herp": {schedulerW}, "herp_test": {usersW}, "kupain": {schedulerW}, "orders": {usersW}, "pickup": {usersW},
	}
	g := b.guard(true)
	if err := g.Allowed("herp"); !errors.Is(err, ErrFolderWritable) || !strings.Contains(err.Error(), "not been read yet") {
		t.Fatalf("before the first read: %v, want refused (fail closed)", err)
	}
	g.checkAll()
	for key, refused := range map[string]bool{"herp": false, "herp_receipts": false, "herp_test": true, "sendOrderDir": false, "sendOrderPickupDir": false, "nope": false} {
		if err := g.Allowed(key); errors.Is(err, ErrFolderWritable) != refused {
			t.Errorf("%s: %v, refused want %v", key, err, refused)
		}
	}
	if err := g.ReceiptCheck(b.cfg)(); err != nil {
		t.Errorf("the bound receipt folder (its scheduler trusted): %v", err)
	}
	if !b.log.warned("FOLDER WRITABLE: sendOrderDir") || !b.log.errored("FOLDER REFUSED: folder herp_test") {
		t.Errorf("warnings %v errors %v", b.log.warns, b.log.errors)
	}

	// herp_test is locked down: within the minute nothing is read again...
	b.set(func(b *guardBox) { b.who["herp_test"] = nil; b.now = b.now.Add(30 * time.Second) })
	if err := g.Allowed("herp_test"); err == nil {
		t.Fatal("re-read within the minute")
	}
	g.wg.Wait()
	if err := g.Allowed("herp_test"); err == nil {
		t.Fatal("re-read within the minute")
	}
	// ...after it, the next order starts a read (it is still answered refused), and the one after goes through.
	b.set(func(b *guardBox) { b.now = b.now.Add(time.Minute) })
	if err := g.Allowed("herp_test"); err == nil {
		t.Fatal("the read is in the background: this order is still refused")
	}
	g.wg.Wait()
	if err := g.Allowed("herp_test"); err != nil {
		t.Fatalf("after the lazy re-read: %v", err)
	}
	if !b.log.loggedInfo("FOLDER ALLOWED: folder herp_test") {
		t.Error("the change was not logged")
	}

	// An administrator opens herp up: refused at the ten-minute re-read, not before.
	b.set(func(b *guardBox) { b.who["herp"] = []dataacl.Writer{usersW}; b.now = b.now.Add(5 * time.Minute) })
	g.tick()
	if err := g.Allowed("herp"); err != nil {
		t.Fatalf("re-read before ten minutes: %v", err)
	}
	b.set(func(b *guardBox) { b.now = b.now.Add(6 * time.Minute) })
	g.tick()
	if err := g.Allowed("herp"); !errors.Is(err, ErrFolderWritable) {
		t.Fatalf("after the ten-minute re-read: %v", err)
	}

	// A read that cannot finish (a domain controller that does not answer) never blocks an order.
	b.set(func(b *guardBox) { b.gate = make(chan struct{}) })
	g2 := b.guard(true)
	done := make(chan struct{})
	go func() { g2.checkAll(); close(done) }()
	start := time.Now()
	if err := g2.Allowed("slow"); !errors.Is(err, ErrFolderWritable) || time.Since(start) > time.Second {
		t.Fatalf("while the first read waits: %v after %s", err, time.Since(start))
	}
	close(b.gate)
	<-done
	if err := g2.Allowed("slow"); err != nil {
		t.Fatalf("once read: %v", err)
	}

	// Off the link nothing is refused, before or after the read.
	off := b.guard(false)
	if err := off.Allowed("herp_test"); err != nil {
		t.Fatalf("off the link before the read: %v", err)
	}
	off.checkAll()
	if err := off.Allowed("herp_test"); err != nil {
		t.Fatalf("off the link: %v", err)
	}
}
