//go:build !windows

package hasavshevet

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// Off Windows nothing can run the receipt import. The stubs must say so: an
// exit 0 there was recorded as import "ok" for a receipt nobody imported.
func TestReceiptImportStubsReportNotRun(t *testing.T) {
	for name, run := range map[string]func() (int, string, error){
		"BAT": func() (int, string, error) { return runReceiptBatFile(context.Background(), `C:\Hash7\kupain.bat`) },
		"has.exe": func() (int, string, error) {
			return runReceiptImporter(context.Background(), `C:\Hash7\has.exe`, `C:\Hash7\k.prm`, `C:\herp`)
		},
	} {
		_, _, err := run()
		if err == nil || !strings.Contains(err.Error(), "only on Windows") {
			t.Errorf("%s stub error = %v; want one saying the import runs only on Windows", name, err)
		}
	}
}

// End to end through the writer's real runners: the receipt is written, its
// import is "failed" with the reason, never "ok".
func TestReceiptWriter_OffWindowsTheImportIsFailedNotOK(t *testing.T) {
	for _, mode := range []string{"BAT", "has.exe"} {
		t.Run(mode, func(t *testing.T) {
			h := newReceiptHarness(t, 1000)
			if mode == "BAT" {
				h.cfg.HasReceiptBatFile = filepath.Join(h.sendDir, "kupain.bat")
			} else {
				h.cfg.HasExePath = filepath.Join(t.TempDir(), "has.exe")
				h.cfg.HasReceiptParamFile = filepath.Join(t.TempDir(), "kupain_perm.bat")
			}
			w := h.writer() // the real runReceiptBatFile / runReceiptImporter
			res := mustWrite(t, w, receiptFor(testKey(1)))
			if !res.Written || res.Import != ImportFailed {
				t.Fatalf("result = %+v; want written with import failed", res)
			}
			e, _, err := w.index.load(testKey(1))
			if err != nil || e == nil || !strings.Contains(e.ImportDetail, "only on Windows") {
				t.Errorf("index entry %+v, %v; want the detail to say why", e, err)
			}
		})
	}
}
