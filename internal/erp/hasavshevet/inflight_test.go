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
	if _, err := ResolveOrder(h.dataDir, cfg, "H-1", "retryable", ResolveFail, "cs-inflight-1", inflightNow); !errors.Is(err, ErrInflightMoved) {
		t.Fatalf("expect retryable on a reserved entry: %v", err)
	}
	if _, err := ResolveOrder(h.dataDir, cfg, "H-1", "reserved", ResolveDone, "cs-inflight-1", inflightNow); !errors.Is(err, ErrInflightNotWritten) {
		t.Fatalf("done without a file: %v", err)
	}
	r, err := ResolveOrder(h.dataDir, cfg, "H-1", "reserved", ResolveFail, "cs-inflight-1", inflightNow)
	if err != nil || r.State != "failed" || !strings.Contains(r.Note, "cs-inflight-1") || !strings.Contains(r.Note, "2026-11-17T03:00:00Z") {
		t.Fatalf("fail: %+v %v", r, err)
	}
	if _, err := ResolveOrder(h.dataDir, cfg, "H-1", "failed", ResolveFail, "cs-inflight-2", inflightNow); !errors.Is(err, ErrInflightFinal) {
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

// Review R6: fail is refused while any file of the order exists (its
// history copy, the pickup file, its frozen name in its typed folder), and
// done is allowed then — the entry names the files found; a resend replays
// done, nothing written again.
func TestInflight_OrderDoneAndMayBeWritten(t *testing.T) {
	h := newOrderHarness(t)
	herp, pickup := filepath.Join(t.TempDir(), "herp"), filepath.Join(t.TempDir(), "pickup")
	for _, d := range []string{herp, pickup} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Config{SendOrderDir: h.sendDir, SendOrderPickupDir: pickup,
		Documents: config.DocumentsConfig{Folders: map[string]config.FolderConfig{"herp": {Kind: "orders", Path: herp, NamePattern: "IMOVEIN_{n}.doc", Layout: "legacy"}}}}

	// A model (b) order: its history copy.
	h.reservedOrder("H-b", 7, nil)
	hist := h.history(7)
	if _, err := ResolveOrder(h.dataDir, cfg, "H-b", "reserved", ResolveFail, "cs-inflight-1", inflightNow); !errors.Is(err, ErrInflightMayBeWritten) {
		t.Fatalf("fail with the history copy there: %v", err)
	}
	r, err := ResolveOrder(h.dataDir, cfg, "H-b", "reserved", ResolveDone, "cs-inflight-1", inflightNow)
	if err != nil || r.State != "written" || len(r.Files) != 1 || r.Files[0] != hist {
		t.Fatalf("done %+v %v", r, err)
	}

	// A pickup order: the scheduler's file.
	h.reservedOrder("H-a", 8, nil)
	if err := os.WriteFile(filepath.Join(pickup, "IMOVEIN_8.doc"), []byte("doc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveOrder(h.dataDir, cfg, "H-a", "reserved", ResolveFail, "cs-inflight-1", inflightNow); !errors.Is(err, ErrInflightMayBeWritten) {
		t.Fatalf("fail with the pickup file there: %v", err)
	}

	// An order with a rule: its frozen name in its typed folder; and a
	// folder no longer configured cannot be looked in.
	rule := &storedRule{Counter: "b2b", Folder: "herp", FileName: "IMOVEIN_9.doc"}
	h.reservedOrder("H-r", 9, rule)
	v, err := InspectOrder(h.dataDir, cfg, "H-r", inflightNow)
	if err != nil || v.Rule == nil || v.Rule.FileName != "IMOVEIN_9.doc" || v.Folder != "herp" || v.Counter != "b2b" || v.Candidates[0].Path != filepath.Join(herp, "IMOVEIN_9.doc") {
		t.Fatalf("inspect %+v %v", v, err)
	}
	if err := os.WriteFile(filepath.Join(herp, "IMOVEIN_9.doc"), []byte("doc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveOrder(h.dataDir, cfg, "H-r", "reserved", ResolveFail, "cs-inflight-1", inflightNow); !errors.Is(err, ErrInflightMayBeWritten) {
		t.Fatalf("fail with the folder's file there: %v", err)
	}
	delete(cfg.Documents.Folders, "herp")
	if _, err := ResolveOrder(h.dataDir, cfg, "H-r", "reserved", ResolveFail, "cs-inflight-1", inflightNow); !errors.Is(err, ErrInflightMayBeWritten) {
		t.Fatalf("fail with the folder gone (cannot be looked in): %v", err)
	}

	q := h.queue(false)
	acc, err := q.Accept(keyedOrder("H-b"))
	if err != nil || !acc.Replay || acc.Status != JobStatusDone {
		t.Fatalf("resend of the done order %+v %v", acc, err)
	}
	if st, ok := q.Status("7"); !ok || st.Status != JobStatusDone || len(st.WrittenFiles) != 1 {
		t.Fatalf("status poll %+v", st)
	}
	if _, err := InspectOrder(h.dataDir, cfg, "nope", inflightNow); !errors.Is(err, ErrInflightMissing) {
		t.Errorf("an unknown key: %v", err)
	}
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

// Review R6: a reserved receipt whose history copy is there is resolved as
// done — written, naming the files found, the importer not run — and a
// resend replays it; fail is refused then.
func TestInflight_ReceiptDone(t *testing.T) {
	h := newReceiptHarness(t, 1000)
	h.reservedReceipt(testKey(3), 1000)
	cfg := config.Config{SendReceiptDir: h.sendDir, HasReceiptBatFile: `C:\Hash7\kupain.bat`}
	hist := filepath.Join(h.dataDir, "history", "receipts", "1000", "KUPAIN_1000.doc")
	if err := os.MkdirAll(filepath.Dir(hist), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hist, []byte("doc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveReceipt(h.dataDir, cfg, 1000, "reserved", ResolveFail, "cs-inflight-1", inflightNow); !errors.Is(err, ErrInflightMayBeWritten) {
		t.Fatalf("fail with the history copy there: %v", err)
	}
	r, err := ResolveReceipt(h.dataDir, cfg, 1000, "reserved", ResolveDone, "cs-inflight-1", inflightNow)
	if err != nil || r.State != "written" || len(r.Files) != 1 || !strings.Contains(r.Note, "did not run the receipt importer") {
		t.Fatalf("done %+v %v", r, err)
	}
	h.cfg.HasReceiptBatFile = ""
	res, err := h.writer().Write(context.Background(), receiptFor(testKey(3)))
	if err != nil || !res.Replayed || res.ReceiptNumber != 1000 || len(h.docs()) != 0 || !strings.Contains(strings.Join(res.Warnings, " "), "cs-inflight-1") {
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
