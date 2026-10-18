package hasavshevet

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// Model (b) has two runners, the mirror of sendOrder's: hasReceiptBatFile, or
// has.exe (the orders' hasExePath) with hasReceiptParamFile, run from
// sendReceiptDir. These tests pin the has.exe one to the same guarantees as the
// BAT: after the file, with a deadline, outcome recorded, never run twice.

func hasExeHarness(t *testing.T, seed int64) *receiptHarness {
	t.Helper()
	h := newReceiptHarness(t, seed)
	h.cfg.HasExePath = filepath.Join(t.TempDir(), "has.exe")
	h.cfg.HasReceiptParamFile = filepath.Join(t.TempDir(), "kupain_perm.bat")
	return h
}

func failBat(t *testing.T) func(context.Context, string) (int, string, error) {
	return func(context.Context, string) (int, string, error) {
		t.Error("the receipt BAT ran in has.exe mode")
		return 0, "", nil
	}
}

func TestReceiptWriter_HasExeFailureIsWrittenAndNeverReRun(t *testing.T) {
	h := hasExeHarness(t, 1000)
	w := h.writer()
	w.runBat = failBat(t)
	var calls atomic.Int32
	w.runHasExe = func(ctx context.Context, exe, param, workDir string) (int, string, error) {
		calls.Add(1)
		if exe != h.cfg.HasExePath || param != h.cfg.HasReceiptParamFile {
			t.Errorf("ran %q %q; want hasExePath with hasReceiptParamFile", exe, param)
		}
		if workDir != h.sendDir {
			t.Errorf("has.exe ran in %q; want sendReceiptDir %q", workDir, h.sendDir)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("has.exe ran without a deadline")
		}
		if _, err := os.Stat(h.docPath(1000)); err != nil {
			t.Errorf("has.exe ran before the receipt file existed: %v", err)
		}
		return 3, "import refused", nil
	}
	req := receiptFor(testKey(1))

	res := mustWrite(t, w, req)
	if !res.Written || res.ReceiptNumber != 1000 || res.Import != ImportFailed {
		t.Fatalf("result = %+v; want written 1000 with import failed", res)
	}
	for i := 0; i < 2; i++ {
		if again := mustWrite(t, w, req); again.Import != ImportFailed || again.ReceiptNumber != 1000 {
			t.Fatalf("repeat = %+v; want 1000 / failed", again)
		}
	}
	w2 := h.writer()
	w2.runBat = failBat(t)
	w2.runHasExe = func(context.Context, string, string, string) (int, string, error) {
		calls.Add(1)
		return 0, "", nil
	}
	if again := mustWrite(t, w2, req); again.Import != ImportFailed {
		t.Fatalf("repeat after restart import = %q; want failed", again.Import)
	}
	if c := calls.Load(); c != 1 {
		t.Errorf("has.exe ran %d times; want exactly once", c)
	}
	e, _, err := w2.index.load(testKey(1))
	if err != nil || e == nil || e.ImportDetail == "" {
		t.Errorf("index entry %+v, %v; want the failure detail recorded", e, err)
	}
}

func TestReceiptWriter_HasExeSuccess(t *testing.T) {
	h := hasExeHarness(t, 1)
	w := h.writer()
	w.runBat = failBat(t)
	w.runHasExe = func(context.Context, string, string, string) (int, string, error) { return 0, "ok", nil }
	if res := mustWrite(t, w, receiptFor(testKey(1))); res.Import != ImportOK {
		t.Fatalf("import = %q; want ok", res.Import)
	}
}

// An exec error (has.exe missing, the deadline) with exit 0 is still a failure.
func TestReceiptWriter_HasExeExecErrorIsAFailure(t *testing.T) {
	h := hasExeHarness(t, 1)
	w := h.writer()
	w.runHasExe = func(context.Context, string, string, string) (int, string, error) {
		return 0, "", fmt.Errorf("exec: file does not exist")
	}
	if res := mustWrite(t, w, receiptFor(testKey(1))); res.Import != ImportFailed {
		t.Fatalf("import = %q; want failed", res.Import)
	}
}

func TestReceiptWriter_HasExeImportStartedIsNeverReRun(t *testing.T) {
	h := hasExeHarness(t, 1)
	req := receiptFor(testKey(1))

	w := h.writer()
	w.runHasExe = func(context.Context, string, string, string) (int, string, error) { panic("killed during import") }
	func() {
		defer func() { _ = recover() }()
		_, _ = w.Write(context.Background(), req)
	}()

	w2 := h.writer()
	var calls atomic.Int32
	w2.runHasExe = func(context.Context, string, string, string) (int, string, error) { calls.Add(1); return 0, "", nil }
	res := mustWrite(t, w2, req)
	if res.Import != ImportStarted || res.ReceiptNumber != 1 {
		t.Fatalf("result = %+v; want 1 / started", res)
	}
	if calls.Load() != 0 {
		t.Error("has.exe was re-run for a key whose import had started")
	}
}

// Recover never runs has.exe, and the recovered receipt says so on every replay.
func TestReceiptWriter_RecoveredReceiptWarnsHasExeWasNotRun(t *testing.T) {
	h := hasExeHarness(t, 1000)
	req := receiptFor(testKey(1))
	w := h.writer()
	w.crashPoint = func(p string) {
		if p == crashAfterStoreAdvanced {
			panic("killed")
		}
	}
	func() {
		defer func() { _ = recover() }()
		_, _ = w.Write(context.Background(), req)
	}()

	var calls atomic.Int32
	for i := 0; i < 2; i++ {
		ww := h.writer()
		ww.runHasExe = func(context.Context, string, string, string) (int, string, error) { calls.Add(1); return 0, "", nil }
		res := mustWrite(t, ww, req)
		if res.ReceiptNumber != 1000 || res.Import != ImportNotRun || !res.Replayed {
			t.Fatalf("replay = %+v; want replayed 1000 / not_run", res)
		}
		if !hasWarningContaining(res.Warnings, "has.exe was not run") {
			t.Errorf("warnings %q: want the has.exe recovery note", res.Warnings)
		}
	}
	if c := calls.Load(); c != 0 {
		t.Errorf("has.exe ran %d times for a recovered receipt; want never", c)
	}
}

// The BAT mode never calls has.exe, and files-only calls neither.
func TestReceiptWriter_ImportRunnerChoice(t *testing.T) {
	for _, tc := range []struct {
		name          string
		bat, exe, prm bool
		wantBat       int32
		wantExe       int32
		wantImport    ImportState
	}{
		{"files only", false, false, false, 0, 0, ImportNotRun},
		{"hasExePath alone imports nothing", false, true, false, 0, 0, ImportNotRun},
		{"BAT", true, false, false, 1, 0, ImportOK},
		{"BAT, orders' hasExePath also set", true, true, false, 1, 0, ImportOK},
		{"has.exe", false, true, true, 0, 1, ImportOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReceiptHarness(t, 1)
			if tc.bat {
				h.cfg.HasReceiptBatFile = filepath.Join(h.sendDir, "kupain.bat")
			}
			if tc.exe {
				h.cfg.HasExePath = filepath.Join(t.TempDir(), "has.exe")
			}
			if tc.prm {
				h.cfg.HasReceiptParamFile = filepath.Join(t.TempDir(), "kupain_perm.bat")
			}
			w := h.writer()
			var bats, exes atomic.Int32
			w.runBat = func(context.Context, string) (int, string, error) { bats.Add(1); return 0, "", nil }
			w.runHasExe = func(context.Context, string, string, string) (int, string, error) { exes.Add(1); return 0, "", nil }
			res := mustWrite(t, w, receiptFor(testKey(1)))
			if bats.Load() != tc.wantBat || exes.Load() != tc.wantExe || res.Import != tc.wantImport {
				t.Errorf("BAT ran %d, has.exe ran %d, import %q; want %d, %d, %q",
					bats.Load(), exes.Load(), res.Import, tc.wantBat, tc.wantExe, tc.wantImport)
			}
		})
	}
}

// The writer refuses an ambiguous or incomplete import config even if a caller
// skipped ValidateReceiptConfig.
func TestNewReceiptWriter_RefusesAnAmbiguousImport(t *testing.T) {
	base := ReceiptWriterConfig{SendReceiptDir: t.TempDir(), DataDir: t.TempDir()}
	both := base
	both.HasReceiptBatFile, both.HasExePath, both.HasReceiptParamFile = "/x/k.bat", "/x/has.exe", "/x/k.prm"
	if _, err := NewReceiptWriter(both, noopLogger{}); err == nil {
		t.Error("accepted both a receipt BAT and a receipt param file")
	}
	noExe := base
	noExe.HasReceiptParamFile = "/x/k.prm"
	if _, err := NewReceiptWriter(noExe, noopLogger{}); err == nil {
		t.Error("accepted a receipt param file with no hasExePath")
	}
}

// has.exe runs through the wait-delayed runner, so the 90 s cap holds.
func TestReceiptWriter_UsesTheWaitDelayedHasExeRunner(t *testing.T) {
	w := newReceiptHarness(t, 1).newWriter()
	if reflect.ValueOf(w.runHasExe).Pointer() != reflect.ValueOf(runReceiptImporter).Pointer() {
		t.Error("the receipt writer does not run has.exe with runReceiptImporter")
	}
}

func TestReceiptWriterConfigFrom_HasExe(t *testing.T) {
	got := ReceiptWriterConfigFrom(config.Config{
		SendReceiptDir: "/srv/herp", HasExePath: " /h/has.exe ", HasReceiptParamFile: "\t/h/kupain_perm.bat ",
	}, "/data")
	if got.HasExePath != "/h/has.exe" || got.HasReceiptParamFile != "/h/kupain_perm.bat" {
		t.Errorf("writer config = %q / %q; want trimmed", got.HasExePath, got.HasReceiptParamFile)
	}
}
