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

func (b *guardBox) fileWriters(path string) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, w := range b.who[filepath.Base(path)] {
		out = append(out, w.Name)
	}
	return out, b.errs[filepath.Base(path)]
}

func (b *guardBox) guard(linked bool) *FolderGuard {
	g := NewFolderGuard(b.cfg, linked, b.writers, b.fileWriters, b.log)
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
	g.wg.Wait() // review m1: the request started the unread folder's read
	if err := g.Allowed("herp"); err != nil {
		t.Fatalf("after the read the request started: %v", err)
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

// Review T1: a trusted writer counts where an importer runs (here the bound
// receipt folder, has.exe's working directory in model b), and the importer
// itself is read before each run with nobody exempt: behind the link a
// writable one is not run.
func TestFolderGuard_ImporterAndItsFolders(t *testing.T) {
	b := newGuardBox(t)
	b.cfg.HasExePath = filepath.Join(filepath.Dir(b.cfg.SendOrderDir), "Hash7", "has.exe")
	b.cfg.HasReceiptParamFile = filepath.Join(filepath.Dir(b.cfg.SendOrderDir), "Hash7", "kupain.prm")
	b.cfg.SendOrderPickupDir, b.cfg.SendOrderFormat = "", ""
	b.who = map[string][]dataacl.Writer{"kupain": {schedulerW}, "has.exe": {schedulerW}}
	g := b.guard(true)
	g.checkAll()
	if err := g.Allowed("herp_receipts"); !errors.Is(err, ErrFolderWritable) {
		t.Errorf("the bound receipt folder where has.exe runs, written by a trusted account: %v, want refused", err)
	}
	for _, k := range []string{KeyOrderImporter, KeyReceiptImporter} {
		if err := g.Allowed(k); !errors.Is(err, ErrImporterWritable) {
			t.Errorf("%s written by the trusted account: %v, want IMPORTER_WRITABLE", k, err)
		}
	}
	if !b.log.errored("IMPORTER REFUSED") {
		t.Errorf("errors %v", b.log.errors)
	}
	// Off the link: run, with a warning.
	off := b.guard(false)
	off.checkAll()
	if err := off.Allowed(KeyOrderImporter); err != nil {
		t.Errorf("off the link: %v", err)
	}
}

// Review m1: the folders are read in parallel, each waited on at most the
// read timeout. A read hung on a domain controller leaves only its folder
// unknown (refused behind the link) — the others are decided — and starts no
// second read of it while it hangs; once it ends its result counts.
func TestFolderGuard_AHungReadHoldsUpNoOtherFolder(t *testing.T) {
	b := newGuardBox(t)
	b.who = map[string][]dataacl.Writer{"herp_test": {usersW}}
	reads := 0
	b.gate = make(chan struct{})
	g := NewFolderGuard(b.cfg, true, func(dir string) ([]dataacl.Writer, error) {
		if filepath.Base(dir) == "slow" {
			b.mu.Lock()
			reads++
			b.mu.Unlock()
		}
		return b.writers(dir)
	}, b.fileWriters, b.log)
	g.readTimeout = 50 * time.Millisecond
	start := time.Now()
	g.checkAll()
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("checkAll waited %s on the hung read", took)
	}
	if err := g.Allowed("herp"); err != nil {
		t.Errorf("herp, read while slow hung: %v", err)
	}
	if err := g.Allowed("herp_test"); !errors.Is(err, ErrFolderWritable) {
		t.Errorf("herp_test: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := g.Allowed("slow"); !errors.Is(err, ErrFolderWritable) || !strings.Contains(err.Error(), "did not finish") {
			t.Fatalf("slow while its read hangs: %v", err)
		}
	}
	g.tick()
	if !b.log.warned("did not finish in 50ms") {
		t.Errorf("warnings %v", b.log.warns)
	}
	close(b.gate)
	g.wg.Wait()
	if err := g.Allowed("slow"); err != nil {
		t.Errorf("slow once its read ended: %v", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if reads != 1 {
		t.Errorf("%d reads of the hung folder, want 1 (no pile-up)", reads)
	}
}

// Review R4-3: the ten-minute re-check of an allowed folder that does not finish in time keeps it allowed until the
// read gives an answer — a slow domain controller causes no 503 — and the answer, when it comes, decides. A refused
// folder whose re-read hangs stays refused; one never read is still unknown.
func TestFolderGuard_ASlowRecheckKeepsTheLastAnswer(t *testing.T) {
	b := newGuardBox(t)
	g := b.guard(true)
	g.readTimeout = 50 * time.Millisecond
	g.checkAll()
	if err := g.Allowed("slow"); err != nil {
		t.Fatalf("slow, read once: %v", err)
	}
	// ten minutes on, its re-read hangs
	b.set(func(b *guardBox) { b.now = b.now.Add(FolderRecheckAllowed); b.gate = make(chan struct{}) })
	g.tick()
	for i := 0; i < 3; i++ {
		if err := g.Allowed("slow"); err != nil {
			t.Fatalf("slow while its re-read hangs: %v (a slow domain controller made it refused)", err)
		}
	}
	if !b.log.warned("it stays allowed, as its last read found it") {
		t.Errorf("warnings %v", b.log.warns)
	}
	// the answer, when it comes, decides: someone opened it up meanwhile
	b.set(func(b *guardBox) { b.who["slow"] = []dataacl.Writer{usersW} })
	b.mu.Lock()
	gate := b.gate
	b.mu.Unlock()
	close(gate)
	g.wg.Wait()
	if err := g.Allowed("slow"); !errors.Is(err, ErrFolderWritable) || !strings.Contains(err.Error(), `BUILTIN\Users`) {
		t.Fatalf("slow once its re-read answered: %v", err)
	}
	// refused, its lazy re-read hangs: it stays refused
	b.set(func(b *guardBox) { b.now = b.now.Add(FolderRecheckRefused); b.gate = make(chan struct{}) })
	if err := g.Allowed("slow"); !errors.Is(err, ErrFolderWritable) {
		t.Fatalf("refused, re-read starting: %v", err)
	}
	time.Sleep(150 * time.Millisecond) // the re-read's wait ends
	if err := g.Allowed("slow"); !errors.Is(err, ErrFolderWritable) || !strings.Contains(err.Error(), `BUILTIN\Users`) {
		t.Fatalf("refused while its re-read hangs: %v", err)
	}
	b.mu.Lock()
	gate = b.gate
	b.mu.Unlock()
	close(gate)
	g.wg.Wait()
	// never read, its first read hangs: unknown, refused behind the link
	fresh := newGuardBox(t)
	fresh.gate = make(chan struct{})
	h := fresh.guard(true)
	h.readTimeout = 50 * time.Millisecond
	h.checkAll()
	if err := h.Allowed("slow"); !errors.Is(err, ErrFolderWritable) || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("never read: %v", err)
	}
	close(fresh.gate)
	h.wg.Wait()
}
