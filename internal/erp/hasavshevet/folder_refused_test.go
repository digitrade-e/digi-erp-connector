package hasavshevet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
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
	h.cfg.FolderRefused = `it can be written by BUILTIN\Users`
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
	h.cfg.FolderRefused = ""
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
