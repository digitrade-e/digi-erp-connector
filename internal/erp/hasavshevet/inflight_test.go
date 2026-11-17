package hasavshevet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/formatfloor"
)

var inflightNow = time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)

// reservedOrder saves an order entry left reserved (the connector stopped
// before recording its outcome) and the counter past it.
func (h *orderHarness) reservedOrder(key string, n int64, rule *storedRule) {
	h.t.Helper()
	ix := orderIndex{dir: OrderIndexDir(h.dataDir)}
	if err := os.MkdirAll(ix.dir, 0o700); err != nil {
		h.t.Fatal(err)
	}
	if err := ix.save(&orderIndexEntry{Key: key, Number: n, State: orderReserved, ReservedAt: "2026-11-16T10:00:00Z", Rule: rule}); err != nil {
		h.t.Fatal(err)
	}
	writeStoreFile(h.t, filepath.Join(h.sendDir, "lastOrderNumber.json"), `{"lastOrderNumber": `+strconv.FormatInt(n, 10)+`}`)
}

func (h *orderHarness) history(n int64) string {
	s := strconv.FormatInt(n, 10)
	p := filepath.Join(h.sendDir, "history", s, "IMOVEIN_"+s+".doc")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("doc"), 0o644); err != nil {
		h.t.Fatal(err)
	}
	return p
}

// Review R6: an order left reserved (its outcome unknown after a restart) is
// resolved as failed only once the box proved no file of it exists; then a
// status poll answers failed, a resend replays failed (never written again),
// and its number stays issued — the next order gets the one after.
func TestInflight_OrderFail(t *testing.T) {
	h := newOrderHarness(t)
	cfg := config.Config{SendOrderDir: h.sendDir}
	h.reservedOrder("H-1", 42, nil)

	v, err := InspectOrder(h.dataDir, cfg, "H-1", inflightNow)
	if err != nil || v.State != "reserved" || v.Number != 42 || v.AgeSeconds != 17*3600 || v.Folder != "sendOrderDir" || len(v.Candidates) != 2 {
		t.Fatalf("inspect %+v %v", v, err)
	}
	if _, err := ResolveOrder(h.dataDir, cfg, "H-1", "retryable", ResolveFail, "cs-inflight-1", false, inflightNow); !errors.Is(err, ErrInflightMoved) {
		t.Fatalf("expect retryable on a reserved entry: %v", err)
	}
	if _, err := ResolveOrder(h.dataDir, cfg, "H-1", "reserved", ResolveDone, "cs-inflight-1", false, inflightNow); !errors.Is(err, ErrInflightNotWritten) {
		t.Fatalf("done without a file: %v", err)
	}
	r, err := ResolveOrder(h.dataDir, cfg, "H-1", "reserved", ResolveFail, "cs-inflight-1", false, inflightNow)
	if err != nil || r.State != "failed" || !strings.Contains(r.Note, "cs-inflight-1") || !strings.Contains(r.Note, "2026-11-17T03:00:00Z") {
		t.Fatalf("fail: %+v %v", r, err)
	}
	if _, err := ResolveOrder(h.dataDir, cfg, "H-1", "failed", ResolveFail, "cs-inflight-2", false, inflightNow); !errors.Is(err, ErrInflightFinal) {
		t.Fatalf("a failed entry again: %v", err)
	}

	q := h.queue(false)
	if st, ok := q.Status("42"); !ok || st.Status != JobStatusFailed {
		t.Fatalf("status poll %+v %v", st, ok)
	}
	acc, err := q.Accept(keyedOrder("H-1"))
	if err != nil || !acc.Replay || acc.Status != JobStatusFailed || acc.JobID != "42" {
		t.Fatalf("resend %+v %v: want a replay, failed, never queued again", acc, err)
	}
	acc, err = q.Accept(keyedOrder("H-2"))
	if err != nil || acc.JobID != "43" {
		t.Fatalf("the next order %+v %v: want 43 (42 is never reused)", acc, err)
	}
	if n, _ := ScanOrdersInFlight(h.dataDir); n.Default != 1 { // H-2 only
		t.Errorf("in flight %+v", n)
	}
	// An order's resolution writes only what v1.0.31 reads: no floor raised.
	if floor, _ := formatfloor.Read(h.dataDir); floor != "" {
		t.Errorf("format floor %q after an order's resolution", floor)
	}
}

// writeFile writes p with content, making its folder.
func writeFile(t *testing.T, p, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// Review R6-1: done stands only on proof of DELIVERY — the delivered file,
// or the importer's *.MASOFON2.BAK rename of it, with the history copy's
// bytes. The history copy alone (written before the document is delivered,
// in every path) is history_only; a model (b) order may be resolved on it
// only with acceptHistoryOnly. A planted file with other bytes proves nothing (m-a).
// fail is refused while any file of the order exists.
func TestInflight_OrderDoneNeedsDelivery(t *testing.T) {
	h := newOrderHarness(t)
	herp, pickup := filepath.Join(t.TempDir(), "herp"), filepath.Join(t.TempDir(), "pickup")
	for _, d := range []string{herp, pickup} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Config{SendOrderDir: h.sendDir, SendOrderPickupDir: pickup,
		Documents: config.DocumentsConfig{Folders: map[string]config.FolderConfig{"herp": {Kind: "orders", Path: herp, NamePattern: "IMOVEIN_{n}.doc", Layout: "legacy"}}}}

	// A model (b) order (no pickup folder) with its history copy only.
	cfgB := cfg
	cfgB.SendOrderPickupDir = ""
	h.reservedOrder("H-b", 7, nil)
	h.history(7)
	if _, err := ResolveOrder(h.dataDir, cfgB, "H-b", "reserved", ResolveFail, "cs-inflight-1", false, inflightNow); !errors.Is(err, ErrInflightMayBeWritten) {
		t.Fatalf("fail with the history copy there: %v", err)
	}
	if v, _ := InspectOrder(h.dataDir, cfgB, "H-b", inflightNow); v.Proof != ProofHistoryOnly {
		t.Errorf("proof %q", v.Proof)
	}
	if _, err := ResolveOrder(h.dataDir, cfgB, "H-b", "reserved", ResolveDone, "cs-inflight-1", false, inflightNow); !errors.Is(err, ErrInflightHistoryOnly) {
		t.Fatalf("done on the history copy alone: %v, want history_only", err)
	}
	// Re-review MINOR 2: acceptHistoryOnly is model (b)'s only. The same
	// order under model (a) (a pickup folder), and a typed-folder order:
	// their delivery leaves a file or its rename, so history alone is
	// refused even with the flag.
	if _, err := ResolveOrder(h.dataDir, cfg, "H-b", "reserved", ResolveDone, "cs-inflight-1", true, inflightNow); !errors.Is(err, ErrDeliveryLeavesFile) || !errors.Is(err, ErrInflightHistoryOnly) {
		t.Fatalf("acceptHistoryOnly under model (a): %v, want history_only (delivery leaves file)", err)
	}
	h.reservedOrder("H-t", 11, &storedRule{Counter: "b2b", Folder: "herp", FileName: "IMOVEIN_11.doc"})
	h.history(11)
	if _, err := ResolveOrder(h.dataDir, cfgB, "H-t", "reserved", ResolveDone, "cs-inflight-1", true, inflightNow); !errors.Is(err, ErrDeliveryLeavesFile) {
		t.Fatalf("acceptHistoryOnly for a typed folder: %v, want history_only (delivery leaves file)", err)
	}
	r, err := ResolveOrder(h.dataDir, cfgB, "H-b", "reserved", ResolveDone, "cs-inflight-1", true, inflightNow)
	if err != nil || r.State != "written" || !strings.Contains(r.Note, "acceptHistoryOnly") {
		t.Fatalf("done with acceptHistoryOnly: %+v %v", r, err)
	}

	// A pickup order: a planted file proves nothing; the delivered one does.
	h.reservedOrder("H-a", 8, nil)
	writeFile(t, filepath.Join(h.sendDir, "history", "8", "IMOVEIN_8.doc"), "order 8")
	writeFile(t, filepath.Join(pickup, "IMOVEIN_8.doc"), "planted")
	if _, err := ResolveOrder(h.dataDir, cfg, "H-a", "reserved", ResolveDone, "cs-inflight-1", false, inflightNow); !errors.Is(err, ErrInflightHistoryOnly) {
		t.Fatalf("done on a planted file: %v, want history_only", err)
	}
	if _, err := ResolveOrder(h.dataDir, cfg, "H-a", "reserved", ResolveFail, "cs-inflight-1", false, inflightNow); !errors.Is(err, ErrInflightMayBeWritten) {
		t.Fatalf("fail with a file there: %v", err)
	}
	writeFile(t, filepath.Join(pickup, "IMOVEIN_8.doc"), "order 8")
	r, err = ResolveOrder(h.dataDir, cfg, "H-a", "reserved", ResolveDone, "cs-inflight-1", false, inflightNow)
	if err != nil || r.State != "written" || !slicesContains(r.Files, filepath.Join(pickup, "IMOVEIN_8.doc")) {
		t.Fatalf("done on the delivered file: %+v %v", r, err)
	}

	// An order with a rule: the scheduler's rename of its file proves it.
	rule := &storedRule{Counter: "b2b", Folder: "herp", FileName: "IMOVEIN_9.doc"}
	h.reservedOrder("H-r", 9, rule)
	writeFile(t, filepath.Join(h.sendDir, "history", "9", "IMOVEIN_9.doc"), "order 9")
	bak := writeFile(t, filepath.Join(herp, "IMOVEIN_9.doc.20261117.MASOFON2.BAK"), "order 9")
	v, err := InspectOrder(h.dataDir, cfg, "H-r", inflightNow)
	if err != nil || v.Proof != ProofImported || v.Rule == nil || v.Folder != "herp" {
		t.Fatalf("inspect %+v %v", v, err)
	}
	if r, err = ResolveOrder(h.dataDir, cfg, "H-r", "reserved", ResolveDone, "cs-inflight-1", false, inflightNow); err != nil || !slicesContains(r.Files, bak) {
		t.Fatalf("done on the rename: %+v %v", r, err)
	}
	// Nothing at all: not_written; a folder no longer configured cannot be looked in.
	h.reservedOrder("H-n", 10, &storedRule{Counter: "b2b", Folder: "gone", FileName: "IMOVEIN_10.doc"})
	if _, err := ResolveOrder(h.dataDir, cfg, "H-n", "reserved", ResolveDone, "cs-inflight-1", false, inflightNow); !errors.Is(err, ErrInflightNotWritten) {
		t.Fatalf("done with nothing: %v", err)
	}
	if _, err := ResolveOrder(h.dataDir, cfg, "H-n", "reserved", ResolveFail, "cs-inflight-1", false, inflightNow); !errors.Is(err, ErrInflightMayBeWritten) {
		t.Fatalf("fail with the folder gone (cannot be looked in): %v", err)
	}

	q := h.queue(false)
	acc, err := q.Accept(keyedOrder("H-a"))
	if err != nil || !acc.Replay || acc.Status != JobStatusDone {
		t.Fatalf("resend of the done order %+v %v", acc, err)
	}
	if _, err := InspectOrder(h.dataDir, cfg, "nope", inflightNow); !errors.Is(err, ErrInflightMissing) {
		t.Errorf("an unknown key: %v", err)
	}
}

func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// reservedReceipt saves a receipt left reserved, the store past it.
func (h *receiptHarness) reservedReceipt(key string, n int64) {
	h.t.Helper()
	ix := receiptIndex{dir: filepath.Join(h.dataDir, "receipts", "index")}
	if err := os.MkdirAll(ix.dir, 0o700); err != nil {
		h.t.Fatal(err)
	}
	if err := ix.save(&receiptIndexEntry{Key: key, Number: n, State: receiptReserved, Request: receiptFor(key), Import: ImportNotRun,
		ReservedAt: "2026-11-16T10:00:00Z"}); err != nil {
		h.t.Fatal(err)
	}
	writeStoreFile(h.t, h.storePath(), `{"nextReceiptNumber": `+strconv.FormatInt(n+1, 10)+`}`)
}

// Review R6 for receipts: a reserved receipt (one Recover could not finish:
// a refused folder, another counter's range) resolved as failed — never
// written, its number void: a resend answers RECEIPT_FAILED, the next receipt
// gets the number after it, and nothing holds the receipt folder back.
func TestInflight_ReceiptFail(t *testing.T) {
	h := newReceiptHarness(t, 1000)
	h.reservedReceipt(testKey(1), 1000)
	cfg := config.Config{SendReceiptDir: h.sendDir}

	v, err := InspectReceipt(h.dataDir, cfg, 1000, inflightNow)
	if err != nil || v.Key != testKey(1) || v.State != "reserved" || len(v.Candidates) != 2 || v.Candidates[0].Path != filepath.Join(h.sendDir, "KUPAIN_1000.doc") {
		t.Fatalf("inspect %+v %v", v, err)
	}
	r, err := ResolveReceipt(h.dataDir, cfg, 1000, "reserved", ResolveFail, "cs-inflight-1", inflightNow)
	if err != nil || r.State != "failed" || !strings.Contains(r.Note, "cs-inflight-1") {
		t.Fatalf("fail %+v %v", r, err)
	}
	if n, _, _ := ScanReceiptsInFlight(h.dataDir); n != 0 {
		t.Errorf("%d reserved receipts left", n)
	}
	// An older build cannot read the failed state: no rollback below v1.0.32.
	if floor, err := formatfloor.Read(h.dataDir); err != nil || floor != ReceiptFailedFloor {
		t.Errorf("format floor %q %v, want %s", floor, err, ReceiptFailedFloor)
	}
	w := h.writer()
	if _, err := w.Write(context.Background(), receiptFor(testKey(1))); !errors.Is(err, ErrReceiptFailed) {
		t.Fatalf("resend: %v, want ErrReceiptFailed", err)
	}
	if len(h.docs()) != 0 {
		t.Fatalf("written: %v", h.docs())
	}
	if res := mustWrite(t, w, receiptFor(testKey(2))); res.ReceiptNumber != 1001 {
		t.Fatalf("the next receipt %d, want 1001 (1000 is never reused)", res.ReceiptNumber)
	}
	if _, err := InspectReceipt(h.dataDir, cfg, 999, inflightNow); !errors.Is(err, ErrInflightMissing) {
		t.Errorf("an unknown number: %v", err)
	}
}

// Review R6-1: a reserved receipt with only its history copy is NOT
// resolved as done — it was never delivered, and Recover delivers it
// (complete() is idempotent); a planted file with other bytes proves
// nothing; the delivered receipt (or its rename) does — written, naming the
// files, the importer not run — and a resend replays it; fail is refused.
func TestInflight_ReceiptDone(t *testing.T) {
	h := newReceiptHarness(t, 1000)
	h.reservedReceipt(testKey(3), 1000)
	cfg := config.Config{SendReceiptDir: h.sendDir, HasReceiptBatFile: `C:\Hash7\kupain.bat`}
	writeFile(t, filepath.Join(h.dataDir, "history", "receipts", "1000", "KUPAIN_1000.doc"), "receipt 1000")
	if _, err := ResolveReceipt(h.dataDir, cfg, 1000, "reserved", ResolveFail, "cs-inflight-1", inflightNow); !errors.Is(err, ErrInflightMayBeWritten) {
		t.Fatalf("fail with the history copy there: %v", err)
	}
	_, err := ResolveReceipt(h.dataDir, cfg, 1000, "reserved", ResolveDone, "cs-inflight-1", inflightNow)
	if !errors.Is(err, ErrInflightHistoryOnly) || !errors.Is(err, ErrRecoverDelivers) {
		t.Fatalf("done on the history copy alone: %v, want history_only (Recover delivers it)", err)
	}
	writeFile(t, filepath.Join(h.sendDir, "KUPAIN_1000.doc"), "planted")
	if _, err := ResolveReceipt(h.dataDir, cfg, 1000, "reserved", ResolveDone, "cs-inflight-1", inflightNow); !errors.Is(err, ErrInflightHistoryOnly) {
		t.Fatalf("done on a planted file: %v", err)
	}
	writeFile(t, filepath.Join(h.sendDir, "KUPAIN_1000.doc"), "receipt 1000")
	r, err := ResolveReceipt(h.dataDir, cfg, 1000, "reserved", ResolveDone, "cs-inflight-1", inflightNow)
	if err != nil || r.State != "written" || len(r.Files) != 2 || r.Proof != ProofDelivered || !strings.Contains(r.Note, "did not run the receipt importer") {
		t.Fatalf("done %+v %v", r, err)
	}
	h.cfg.HasReceiptBatFile = ""
	res, err := h.writer().Write(context.Background(), receiptFor(testKey(3)))
	if err != nil || !res.Replayed || res.ReceiptNumber != 1000 || !strings.Contains(strings.Join(res.Warnings, " "), "cs-inflight-1") {
		t.Fatalf("resend %+v %v", res, err)
	}
}

// A failed receipt entry reads back; any other new state still refuses.
func TestReceiptIndex_FailedState(t *testing.T) {
	key := testKey(5)
	if _, err := decodeReceiptIndexEntry([]byte(`{"key":"`+key+`","number":5,"state":"failed","request":{},"files":null,"import":"not_run","reservedAt":"x"}`), key); err != nil {
		t.Errorf("failed: %v", err)
	}
	if _, err := decodeReceiptIndexEntry([]byte(`{"key":"`+key+`","number":5,"state":"void","request":{},"files":null,"import":"not_run","reservedAt":"x"}`), key); err == nil {
		t.Error("an unknown state read back")
	}
}

// Review m-b: history pruned and the delivered file renamed by the importer
// (*.MASOFON2.BAK) may be an order the books have — fail is refused; done
// needs the history copy to show it is the connector's; the same for a
// receipt.
func TestInflight_FailSeesTheImportersRename(t *testing.T) {
	h := newOrderHarness(t)
	pickup := filepath.Join(t.TempDir(), "pickup")
	cfg := config.Config{SendOrderDir: h.sendDir, SendOrderPickupDir: pickup}
	h.reservedOrder("H-p", 12, nil)
	writeFile(t, filepath.Join(pickup, "imovein_12.doc.MASOFON2.BAK"), "order 12") // no history copy: pruned
	if _, err := ResolveOrder(h.dataDir, cfg, "H-p", "reserved", ResolveFail, "cs-inflight-1", false, inflightNow); !errors.Is(err, ErrInflightMayBeWritten) {
		t.Fatalf("fail with the scheduler's rename there: %v, want may_be_written", err)
	}
	// …and done does not stand on it either: without its history copy
	// nothing shows the file is the connector's (re-review MINOR 1).
	if _, err := ResolveOrder(h.dataDir, cfg, "H-p", "reserved", ResolveDone, "cs-inflight-1", false, inflightNow); !errors.Is(err, ErrInflightHistoryMissing) {
		t.Fatalf("done on the rename with no history copy: %v, want history_missing", err)
	}

	rh := newReceiptHarness(t, 1000)
	rh.reservedReceipt(testKey(4), 1000)
	writeFile(t, filepath.Join(rh.sendDir, "KUPAIN_1000.doc.1.MASOFON2.BAK"), "receipt")
	if _, err := ResolveReceipt(rh.dataDir, config.Config{SendReceiptDir: rh.sendDir}, 1000, "reserved", ResolveFail, "cs-inflight-1", inflightNow); !errors.Is(err, ErrInflightMayBeWritten) {
		t.Fatalf("receipt fail with the rename there: %v", err)
	}
}

// Re-review MINOR 1: a delivered file with no readable history copy proves
// nothing — the connector writes history first, so it may be planted, or
// the legacy app's file under the same number in a shared folder:
// history_missing. A history copy over 8 MiB is compared by streaming.
func TestInflight_DeliveredWithoutHistoryProvesNothing(t *testing.T) {
	h := newOrderHarness(t)
	pickup := filepath.Join(t.TempDir(), "pickup")
	cfg := config.Config{SendOrderDir: h.sendDir, SendOrderPickupDir: pickup}
	h.reservedOrder("H-x", 20, nil)
	writeFile(t, filepath.Join(pickup, "IMOVEIN_20.doc"), "the legacy app's order 20")
	if _, err := ResolveOrder(h.dataDir, cfg, "H-x", "reserved", ResolveDone, "cs-inflight-1", false, inflightNow); !errors.Is(err, ErrInflightHistoryMissing) {
		t.Fatalf("done on a delivered file without history: %v, want history_missing", err)
	}

	big := strings.Repeat("0123456789abcdef", 600_000) // 9.6 MB, over the old 8 MiB read
	h.reservedOrder("H-y", 21, nil)
	writeFile(t, filepath.Join(h.sendDir, "history", "21", "IMOVEIN_21.doc"), big)
	writeFile(t, filepath.Join(pickup, "IMOVEIN_21.doc"), big[:len(big)-1]+"X")
	if _, err := ResolveOrder(h.dataDir, cfg, "H-y", "reserved", ResolveDone, "cs-inflight-1", false, inflightNow); err == nil {
		t.Fatal("done on a large file that differs from its history copy in the last byte")
	}
	writeFile(t, filepath.Join(pickup, "IMOVEIN_21.doc"), big)
	if r, err := ResolveOrder(h.dataDir, cfg, "H-y", "reserved", ResolveDone, "cs-inflight-1", false, inflightNow); err != nil || r.Proof != ProofDelivered {
		t.Fatalf("done on a large delivered file: %+v %v", r, err)
	}
}
