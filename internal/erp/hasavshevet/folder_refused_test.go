package hasavshevet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// Plan 087 review M1: behind the link the daemon writes nothing into a typed
// folder accounts besides the administrators can write to (RefuseFolders,
// FolderRefused). Nothing is reserved for a new document; a re-queue or the
// finishing of a reserved receipt waits; a repeat of a written one is still
// answered.

func plantRuledEntry(t *testing.T, dataDir, key string, n int64, state orderState, folder string) {
	t.Helper()
	ix := orderIndex{dir: OrderIndexDir(dataDir)}
	if err := os.MkdirAll(ix.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	e := &orderIndexEntry{Key: key, Number: n, State: state, ReservedAt: "2026-11-16T10:00:00Z",
		Rule: &storedRule{Counter: "shopify", Folder: folder, FileName: fmt.Sprintf("IMOVEIN_%d.doc", n)}}
	if err := ix.save(e); err != nil {
		t.Fatal(err)
	}
}

func TestQueue_RefusedFolderReservesNothing(t *testing.T) {
	h := newCountersHarness(t)
	plantRuledEntry(t, h.dataDir, "retry-1", 5_000_000, orderRetryable, "herp")
	plantRuledEntry(t, h.dataDir, "done-1", 5_000_001, orderWritten, "herp")
	q := h.queue(false)
	q.RefuseFolders(map[string]string{"herp": `it can be written by BUILTIN\Users`})

	if _, err := q.Accept(ruled("w1", "shopify", "herp", false)); !errors.Is(err, ErrFolderWritable) {
		t.Fatalf("a new order into a refused folder: %v, want ErrFolderWritable", err)
	}
	if _, found, _ := (orderIndex{dir: OrderIndexDir(h.dataDir)}).load("w1"); found {
		t.Error("an entry was reserved")
	}
	if h.last("shopify") != 5_000_001 {
		t.Errorf("the counter moved to %d", h.last("shopify"))
	}
	if _, err := q.Accept(ruled("retry-1", "shopify", "herp", false)); !errors.Is(err, ErrFolderWritable) {
		t.Fatalf("a re-queue into a refused folder: %v", err)
	}
	if h.entry("retry-1").State != orderRetryable {
		t.Errorf("the refused re-queue changed the entry: %s", h.entry("retry-1").State)
	}
	if acc, err := q.Accept(ruled("done-1", "shopify", "herp", false)); err != nil || !acc.Replay || acc.Status != JobStatusDone {
		t.Fatalf("a written order's repeat: %+v %v", acc, err)
	}
	// Another folder, and an order without a rule, go on.
	if _, err := q.Accept(ruled("t1", "b2b_test", "herp_test", false)); err != nil {
		t.Fatalf("another folder: %v", err)
	}
	if _, err := q.Accept(keyedOrder("plain")); err != nil {
		t.Fatalf("an order without a rule: %v", err)
	}
}

func TestReceiptWriter_RefusedFolderWritesNothing(t *testing.T) {
	h := newReceiptHarness(t, 1000)
	ix := receiptIndex{dir: filepath.Join(h.dataDir, "receipts", "index")}
	if err := os.MkdirAll(ix.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A receipt reserved before a crash: the store already moved past it.
	if err := ix.save(&receiptIndexEntry{Key: testKey(2), Number: 1000, State: receiptReserved, Request: receiptFor(testKey(2)),
		Import: ImportNotRun, ReservedAt: "2026-11-16T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	writeStoreFile(t, h.storePath(), `{"nextReceiptNumber": 1001}`)
	h.cfg.FolderCheck = func() error { return staticRefusals{"herp": `it can be written by BUILTIN\Users`}.Allowed("herp") }
	w := h.writer() // Recover finishes nothing into the folder
	if len(h.docs()) != 0 {
		t.Fatalf("Recover wrote into a refused folder: %v", h.docs())
	}
	if _, err := w.Write(context.Background(), receiptFor(testKey(1))); !errors.Is(err, ErrFolderWritable) {
		t.Fatalf("a new receipt: %v, want ErrFolderWritable", err)
	}
	if _, err := w.Write(context.Background(), receiptFor(testKey(2))); !errors.Is(err, ErrFolderWritable) {
		t.Fatalf("finishing a reserved receipt: %v, want ErrFolderWritable", err)
	}
	if n := h.nextNumber(); n != 1001 || len(h.docs()) != 0 || len(h.indexFiles()) != 1 {
		t.Fatalf("next %d, docs %v, index %v: something was reserved or written", n, h.docs(), h.indexFiles())
	}

	// Locked down and restarted: the reserved receipt is finished, then new ones go on.
	h.cfg.FolderCheck = nil
	w = h.writer()
	if len(h.docs()) != 1 {
		t.Fatalf("Recover did not finish the reserved receipt: %v", h.docs())
	}
	if res := mustWrite(t, w, receiptFor(testKey(1))); res.ReceiptNumber != 1001 {
		t.Fatalf("next receipt %d, want 1001", res.ReceiptNumber)
	}
}

// Review R5: GET /api/counters' cache keys on the generation, so it must move
// only once the store has moved. Bumped at the index save, a report read
// between the save and the store's advance — under the new generation — kept
// the old last number for the cache's 3 s.
func TestGeneration_MovesAfterTheStore(t *testing.T) {
	h := newCountersHarness(t)
	q := h.queue(false)
	g0, atIndex := q.Generation(), uint64(0)
	q.crashPoint = func(p string) {
		if p == crashOrderAfterIndexReserved {
			atIndex = q.Generation()
		}
	}
	if _, err := q.Accept(keyedOrder("g1")); err != nil {
		t.Fatal(err)
	}
	if atIndex != g0 || q.Generation() == g0 {
		t.Fatalf("order: generation %d at the index save (before %d), %d after; want it to move only after the store", atIndex, g0, q.Generation())
	}

	r := newReceiptHarness(t, 1000)
	w := r.writer()
	g0, atIndex = w.Generation(), 0
	w.crashPoint = func(p string) {
		if p == crashAfterIndexReserved {
			atIndex = w.Generation()
		}
	}
	mustWrite(t, w, receiptFor(testKey(1)))
	if atIndex != g0 || w.Generation() == g0 {
		t.Fatalf("receipt: generation %d at the index save (before %d), %d after", atIndex, g0, w.Generation())
	}
}

// importerRefusal is a guard that refuses the importers (review T1).
type importerRefusal struct{}

func (importerRefusal) Allowed(key string) error {
	if strings.HasPrefix(key, "importer:") {
		return fmt.Errorf("%w: %s can be changed by BFL\\wizsched", ErrImporterWritable, key)
	}
	return nil
}

// Review T1: behind the link an importer others can change is not run. The
// worker reads it before any file of a model (b) order is written (the order
// stays retryable); Accept refuses a rule-less order before reserving; a
// receipt that would run the receipt importer is refused before reserving.
func TestImporterRefused_NothingRunsNothingIsReserved(t *testing.T) {
	h := newOrderHarness(t)
	cfg := config.Config{ERP: config.ERPHasavshevet, SendOrderDir: h.sendDir, HasExePath: `C:\Hash7\has.exe`}
	s := NewSender(nil, cfg, NewOrderNumberStore(filepath.Join(h.sendDir, "lastOrderNumber.json")), h.log)
	ran := 0
	s.runHasExe = func(context.Context, string, string, string) (int, string, error) { ran++; return 0, "", nil }
	s.importerCheck = func() error { return importerRefusal{}.Allowed(KeyOrderImporter) }
	_, err := s.processOrder(context.Background(), keyedOrder("k"), 5, nil)
	if !errors.Is(err, ErrImporterWritable) || !errors.Is(err, ErrOrderNotWritten) || ran != 0 {
		t.Fatalf("worker: %v (ran %d): want IMPORTER_WRITABLE, not written, not run", err, ran)
	}
	if _, err := os.Stat(filepath.Join(h.sendDir, "IMOVEIN.doc")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("IMOVEIN.doc was written")
	}

	q := NewOrderQueue(s, h.log)
	q.UseKeyIndex(h.dataDir)
	q.UseImporterGuard(importerRefusal{})
	if err := q.Recover(); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Accept(keyedOrder("k2")); !errors.Is(err, ErrImporterWritable) {
		t.Fatalf("Accept: %v, want IMPORTER_WRITABLE", err)
	}
	if _, found, _ := (orderIndex{dir: OrderIndexDir(h.dataDir)}).load("k2"); found {
		t.Error("an entry was reserved")
	}

	r := newReceiptHarness(t, 1000)
	r.cfg.HasReceiptBatFile = `C:\Hash7\kupain.bat`
	r.cfg.ImporterCheck = func() error { return importerRefusal{}.Allowed(KeyReceiptImporter) }
	w := r.writer()
	if _, err := w.Write(context.Background(), receiptFor(testKey(1))); !errors.Is(err, ErrImporterWritable) {
		t.Fatalf("receipt: %v, want IMPORTER_WRITABLE", err)
	}
	if r.nextNumber() != 1000 || len(r.indexFiles()) != 0 {
		t.Error("a receipt number was reserved")
	}
}

// Review m2: while the receipt folder is refused (a reserved receipt left in
// it), a receipt is refused before the recovery's full index scan — not
// after re-reading every entry on every request: an index entry unreadable
// since the start changes nothing — and a repeat of a written key is still
// answered from its own entry. Once allowed, recovery runs and finishes it.
func TestReceiptWriter_RefusedBeforeTheRecoveryScan(t *testing.T) {
	h := newReceiptHarness(t, 1000)
	refused := false
	h.cfg.FolderCheck = func() error {
		if refused {
			return staticRefusals{"herp": `it can be written by BUILTIN\Users`}.Allowed("herp")
		}
		return nil
	}
	w := h.writer()
	first := mustWrite(t, w, receiptFor(testKey(1)))
	ix := receiptIndex{dir: filepath.Join(h.dataDir, "receipts", "index")}
	if err := ix.save(&receiptIndexEntry{Key: testKey(2), Number: first.ReceiptNumber + 1, State: receiptReserved, Request: receiptFor(testKey(2)),
		Import: ImportNotRun, ReservedAt: "2026-11-16T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	writeStoreFile(t, h.storePath(), fmt.Sprintf(`{"nextReceiptNumber": %d}`, first.ReceiptNumber+2))
	refused = true
	w = h.writer() // the start's Recover leaves the reserved receipt: refused folder
	// An entry that does not decode: the full scan would fail on it.
	if err := os.WriteFile(filepath.Join(ix.dir, testKey(9)+".json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := w.Write(context.Background(), receiptFor(testKey(3))); !errors.Is(err, ErrFolderWritable) {
			t.Fatalf("a new receipt while refused: %v, want ErrFolderWritable (refused before the scan)", err)
		}
	}
	if _, err := w.Write(context.Background(), receiptFor(testKey(2))); !errors.Is(err, ErrFolderWritable) {
		t.Fatalf("the reserved receipt while refused: %v", err)
	}
	res, err := w.Write(context.Background(), receiptFor(testKey(1)))
	if err != nil || res.ReceiptNumber != first.ReceiptNumber || !res.Replayed {
		t.Fatalf("a written repeat while refused: %+v, %v", res, err)
	}
	// Allowed again: the next receipt recovers first (the unreadable entry now stops it, as before).
	refused = false
	if _, err := w.Write(context.Background(), receiptFor(testKey(3))); !errors.Is(err, ErrReceiptIndexUnreadable) {
		t.Fatalf("allowed: %v, want the scan's ErrReceiptIndexUnreadable", err)
	}
	if err := os.Remove(filepath.Join(ix.dir, testKey(9)+".json")); err != nil {
		t.Fatal(err)
	}
	if res := mustWrite(t, w, receiptFor(testKey(3))); res.ReceiptNumber != first.ReceiptNumber+2 || len(h.docs()) != 3 {
		t.Fatalf("after recovery: %+v, docs %v", res, h.docs())
	}
}
