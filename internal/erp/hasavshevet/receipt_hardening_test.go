package hasavshevet

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"
)

// A fresh number whose history folder already exists means the store was moved
// back (a partial restore, a hand edit): that number was issued before. The
// writer refuses rather than overwrite the history of a receipt already given.
func TestReceiptWriter_FreshNumberWithHistoryRefuses(t *testing.T) {
	h := newReceiptHarness(t, 1000)
	if err := os.MkdirAll(filepath.Join(h.dataDir, "history", "receipts", "1000"), 0o755); err != nil {
		t.Fatal(err)
	}
	w := h.writer()

	_, err := w.Write(context.Background(), receiptFor(testKey(1)))
	if !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("Write = %v; want ErrStoreUnavailable", err)
	}
	if n := h.nextNumber(); n != 1000 {
		t.Errorf("next number = %d; want 1000 untouched", n)
	}
	if idx := h.indexFiles(); len(idx) != 0 {
		t.Errorf("index holds %v; want nothing reserved", idx)
	}
	if docs := h.docs(); len(docs) != 0 {
		t.Errorf("herp holds %v; want nothing", docs)
	}
}

// When the files of a fresh number cannot be written, that number stays
// reserved, and the next Write — for any key — finishes it before issuing the
// next one. Without that, n+1 would reach the importer while n is still
// missing.
func TestReceiptWriter_FailedCompleteIsFinishedBeforeTheNextNumber(t *testing.T) {
	h := newReceiptHarness(t, 1000)
	w := h.writer()

	// Make the herp folder unwritable: a file where the folder should be.
	if err := os.RemoveAll(h.sendDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.sendDir, []byte("not a folder"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(context.Background(), receiptFor(testKey(1))); err == nil {
		t.Fatal("Write succeeded with no herp folder")
	}

	// The operator fixes the folder; another key arrives first.
	if err := os.Remove(h.sendDir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(h.sendDir, 0o755); err != nil {
		t.Fatal(err)
	}
	res := mustWrite(t, w, receiptFor(testKey(2)))
	if res.ReceiptNumber != 1001 {
		t.Fatalf("second key got %d; want 1001 (1000 belongs to the first)", res.ReceiptNumber)
	}
	if _, err := os.Stat(h.docPath(1000)); err != nil {
		t.Fatalf("KUPAIN_1000.doc was not finished before 1001 was issued: %v", err)
	}
	e, found, err := w.index.load(testKey(1))
	if err != nil || !found || e.State != receiptWritten {
		t.Fatalf("first key's entry = %+v found=%v err=%v; want written", e, found, err)
	}
}

// Two index entries holding one number would let two keys answer the same
// receipt. Recover and Write refuse to go on.
func TestReceiptIndex_TwoEntriesForOneNumberFailClosed(t *testing.T) {
	h := newReceiptHarness(t, 1000)
	w := h.writer()
	mustWrite(t, w, receiptFor(testKey(1)))

	e, _, err := w.index.load(testKey(1))
	if err != nil {
		t.Fatal(err)
	}
	dup := *e
	dup.Key = testKey(2)
	dup.Request.IdempotencyKey = testKey(2)
	b, _ := json.Marshal(&dup)
	if err := os.WriteFile(w.index.path(testKey(2)), b, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := w.index.all(); !errors.Is(err, ErrReceiptIndexUnreadable) {
		t.Fatalf("all() = %v; want ErrReceiptIndexUnreadable", err)
	}
	if err := h.newWriter().Recover(); err == nil {
		t.Fatal("Recover accepted two entries for one number")
	}
	if _, err := h.newWriter().Write(context.Background(), receiptFor(testKey(3))); err == nil {
		t.Fatal("Write issued a number over an index with a duplicate")
	}
	if n := h.nextNumber(); n != 1001 {
		t.Errorf("next number = %d; want 1001", n)
	}
}

// The receipt BAT's timeout must be real: on Windows killing cmd.exe leaves the
// importer it started holding the output pipe, and without WaitDelay reading
// that pipe outlives the deadline. Here a shell leaves a grandchild holding
// stdout the same way.
func TestRunCommand_WaitDelayBoundsAnOrphanedPipe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh; the mechanism is the same exec.Cmd.WaitDelay")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _, err := runCommand(ctx, 200*time.Millisecond, "", "sh", "-c", "sleep 30 & sleep 30")
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("runCommand took %v; the wait delay did not bound it", took)
	}
	if err == nil {
		t.Error("a killed command reported success")
	}
}

// The BAT cap plus its wait delay fits inside the route's 120 s write deadline.
func TestReceiptBatBudget(t *testing.T) {
	if receiptBatWaitDelay <= 0 {
		t.Fatal("receipt BAT has no wait delay")
	}
	if receiptBatTimeout+receiptBatWaitDelay > 100*time.Second {
		t.Errorf("BAT timeout %v + wait delay %v leaves too little of the 120 s route deadline",
			receiptBatTimeout, receiptBatWaitDelay)
	}
}

// The writer runs the receipt BAT through the wait-delayed runner, not the
// order importer's.
func TestReceiptWriter_UsesTheWaitDelayedBatRunner(t *testing.T) {
	w := newReceiptHarness(t, 1).newWriter()
	if reflect.ValueOf(w.runBat).Pointer() != reflect.ValueOf(runReceiptBatFile).Pointer() {
		t.Error("the receipt writer does not run its BAT with runReceiptBatFile")
	}
}
