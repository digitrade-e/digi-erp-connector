package hasavshevet

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// The .doc is renamed into the herp folder LAST: it is what the importer
// picks up, so nothing it depends on may be missing when it appears. With
// KUPAIN.prm unwritable (a directory squats its name) no .doc is visible.
func TestReceiptWriter_DocIsWrittenLast(t *testing.T) {
	h := newReceiptHarness(t, 1000)
	w := h.writer()
	if err := os.MkdirAll(filepath.Join(h.sendDir, KUPAINPRMName, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(context.Background(), receiptFor(testKey(1))); err == nil {
		t.Fatal("Write succeeded with KUPAIN.prm unwritable")
	}
	if d := h.docs(); len(d) != 0 {
		t.Fatalf("a .doc is visible before KUPAIN.prm: %v", d)
	}
}

// Model (b): when "import started" cannot be recorded, the BAT must NOT run —
// unrecorded, a crash during it would let a repeat run it a second time and
// import the receipt twice. The receipt itself is written, and says so.
func TestReceiptWriter_BatNeverRunsWhenItsStartCannotBeRecorded(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	h := newReceiptHarness(t, 1000)
	h.cfg.HasReceiptBatFile = filepath.Join(h.sendDir, "kupain.bat")
	w := h.writer()
	var calls atomic.Int32
	w.runBat = func(context.Context, string) (int, string, error) { calls.Add(1); return 0, "", nil }
	indexDir := filepath.Join(h.dataDir, "receipts", "index")
	w.crashPoint = func(p string) {
		if p == crashBeforeImportStarted {
			if err := os.Chmod(indexDir, 0o500); err != nil {
				t.Fatal(err)
			}
		}
	}
	defer func() { _ = os.Chmod(indexDir, 0o700) }()

	res := mustWrite(t, w, receiptFor(testKey(1)))
	if c := calls.Load(); c != 0 {
		t.Fatalf("the BAT ran %d times with its start unrecorded; want never", c)
	}
	if !res.Written || res.ReceiptNumber != 1000 || res.Import != ImportNotRun {
		t.Fatalf("result = %+v; want written 1000, import not_run", res)
	}
	if !hasWarningContaining(res.Warnings, "BAT was not run") {
		t.Errorf("warnings %q; want one saying the BAT was not run", res.Warnings)
	}
}

// Model (b): the BAT outlives the HTTP request. A caller hanging up mid-import
// must not kill Hashavshevet half-way through it.
func TestReceiptWriter_BatOutlivesTheCaller(t *testing.T) {
	h := newReceiptHarness(t, 1000)
	h.cfg.HasReceiptBatFile = filepath.Join(h.sendDir, "kupain.bat")
	w := h.writer()
	ctx, hangUp := context.WithCancel(context.Background())
	defer hangUp()
	var batErr atomic.Value
	w.runBat = func(batCtx context.Context, _ string) (int, string, error) {
		hangUp() // the caller goes away while the BAT runs
		batErr.Store(strings.TrimSpace(errString(batCtx.Err())))
		if _, ok := batCtx.Deadline(); !ok {
			t.Error("the BAT ran without its own deadline")
		}
		return 0, "", nil
	}
	res, err := w.Write(ctx, receiptFor(testKey(1)))
	if err != nil {
		t.Fatal(err)
	}
	if got := batErr.Load(); got != "" {
		t.Fatalf("the BAT's context ended with the caller's: %v", got)
	}
	if res.Import != ImportOK {
		t.Fatalf("import = %q; want ok", res.Import)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
