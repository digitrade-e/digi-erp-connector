package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/charmap"

	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
)

func win1255File(t *testing.T, path, text string) {
	t.Helper()
	b, err := charmap.Windows1255.NewEncoder().Bytes([]byte(strings.ReplaceAll(text, "\n", "\r\n")))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// masofonBox is a box whose orders run digi.bat beside a masofon.ini whose ErrDataPath is logs; hash is has.exe's folder.
func masofonBox(t *testing.T) (sys *fakeSys, hash, logs string) {
	t.Helper()
	sys, _, orders := handInstallBox(t)
	hash, logs = t.TempDir(), t.TempDir()
	bat := filepath.Join(hash, "digi.bat")
	if err := os.WriteFile(bat, []byte("call has.exe\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	win1255File(t, filepath.Join(hash, "masofon.ini"), "[commands]\ndigi.bat=company_db -6 -"+orders+`\imovein.doc -FixedLength -Ansi2Ansi -x.prm - -u - -0 -0 - -0 -YES -;`+
		"\n[general]\nErrDataPath="+logs+"\n")
	cfg := handInstallConfig(orders)
	cfg.HasBatFile = bat
	writeConfig(t, sys.dir, cfg)
	return sys, hash, logs
}

func runLogName(bat, kind string, at time.Time) string {
	return bat + "." + kind + ".DMY-" + at.Format("02.01.06") + ".HMS-" + at.Format("15.04.05") + ".LOG"
}

// logs masofon: the BAT's run logs, newest last, each line with its file; another BAT's logs are not read; grep and
// maxLines apply; files are their full paths.
func TestLogs_MasofonRunLogs(t *testing.T) {
	sys, _, logs := masofonBox(t)
	t0 := time.Date(2026, 11, 19, 14, 0, 0, 0, time.Local)
	win1255File(t, filepath.Join(logs, runLogName("digi.bat", "ok", t0)), "[box_1.digi.bat] --->company_db -6\n[19/11/26 14:00:01] c:/o/imovein.doc:\n[19/11/26 14:00:02] קליטת תנועות מלאי הסתיימה בהצלחה\n")
	win1255File(t, filepath.Join(logs, runLogName("digi.bat", "err", t0.Add(5*time.Minute))), "[box_1.digi.bat] --->company_db -6\n[19/11/26 14:05:01] c:/o/imovein.doc:\n[19/11/26 14:05:02] 77\tStock.063B מסמך בספרור זה כבר נקלט בעבר\t1\t\n")
	win1255File(t, filepath.Join(logs, runLogName("wms.bat", "err", t0.Add(time.Minute))), "[box_1.wms.bat] --->other\nSECRET-OF-ANOTHER-INTEGRATION\n")

	e, res, raw := runLogs(t, sys, `{"source":"masofon"}`)
	if !e.OK {
		t.Fatalf("refused: %+v", e.Refusal)
	}
	if strings.Contains(string(raw), "ANOTHER-INTEGRATION") {
		t.Fatalf("another BAT's log was read: %s", raw)
	}
	if len(res.Files) != 2 || !filepath.IsAbs(res.Files[0]) || !strings.Contains(res.Files[1], ".err.") || !res.Untrusted {
		t.Fatalf("files %v untrusted %v", res.Files, res.Untrusted)
	}
	last := res.Lines[len(res.Lines)-1]
	if !strings.HasPrefix(last, runLogName("digi.bat", "err", t0.Add(5*time.Minute))+"| ") || !strings.Contains(last, "Stock.063B") ||
		!strings.Contains(strings.Join(res.Lines, "\n"), "הסתיימה בהצלחה") {
		t.Fatalf("lines %q", res.Lines)
	}
	_, g, _ := runLogs(t, sys, `{"source":"masofon","grep":"stock.063"}`)
	if len(g.Lines) != 1 {
		t.Fatalf("grep: %q", g.Lines)
	}
	_, s, _ := runLogs(t, sys, fmt.Sprintf(`{"source":"masofon","since":%q}`, t0.Add(time.Minute).Format(time.RFC3339)))
	if len(s.Files) != 1 {
		t.Fatalf("since: files %v", s.Files)
	}
}

// logs masofon-errors: the importer's error logs beside has.exe; lines tagged with another profile left out.
func TestLogs_MasofonErrors(t *testing.T) {
	sys, hash, _ := masofonBox(t)
	win1255File(t, filepath.Join(hash, "DataImpExpLogs", "WizImportError_BOX.log"), strings.Join([]string{
		"2026-11-19 8:01:03:799  WizIni.dll #digi.bat#  ERROR קליטה הופסקה עקב הקלקה כפולה",
		"2026-11-19 8:02:03:000  WizIni.dll #wms.bat#  ERROR another profile's",
		"2026-11-19 10:16:30  Prm2Ini.dll  ERROR file does not exists: _access( imovein.doc, 0 )",
	}, "\n"))
	e, res, _ := runLogs(t, sys, `{"source":"masofon-errors"}`)
	if !e.OK {
		t.Fatalf("refused: %+v", e.Refusal)
	}
	if len(res.Lines) != 2 || !strings.Contains(res.Lines[0], "#digi.bat#") || !strings.Contains(res.Lines[1], "Prm2Ini") || len(res.Files) != 1 {
		t.Fatalf("lines %q files %v", res.Lines, res.Files)
	}
}

// Without a Masofon BAT there is nothing to read: log_unreadable, not an empty answer.
func TestLogs_MasofonWithoutABat(t *testing.T) {
	sys := logsBox(t)
	e, _, _ := runLogs(t, sys, `{"source":"masofon"}`)
	if e.OK || e.Refusal == nil || e.Refusal.Code != CodeLogUnreadable {
		t.Fatalf("envelope %+v", e)
	}
}

func writeImportCheck(t *testing.T, dataDir string, n int64, c hasavshevet.ImportCheck) {
	t.Helper()
	b, _ := json.Marshal(c)
	p := filepath.Join(hasavshevet.OrderImportsDir(dataDir), fmt.Sprintf("%d.json", n))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// state's imports: counts by outcome, the newest not imported with their codes, and one order's whole check.
func TestState_Imports(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	env := testEnv(sys, fdb)
	if res, err := State(context.Background(), env, StateInput{}); err != nil || res.Imports != nil {
		t.Fatalf("a box without checks: imports %+v, %v", res.Imports, err)
	}
	writeImportCheck(t, sys.dir, 41, hasavshevet.ImportCheck{Result: "imported", Outcome: "imported", CheckedAt: "t1"})
	writeImportCheck(t, sys.dir, 42, hasavshevet.ImportCheck{Result: "failed", Outcome: "refused", CheckedAt: "t2",
		RunLog: &hasavshevet.RunLog{File: `E:\l\digi.bat.err.LOG`, Section: "refused", Refusals: []hasavshevet.RunLogRefusal{{Value: "1001", Code: "Stock.002", Message: "פריט לא קיים", Row: 2}}}})
	writeImportCheck(t, sys.dir, 43, hasavshevet.ImportCheck{Result: "failed", RunLog: &hasavshevet.RunLog{File: "x", Transient: true}, CheckedAt: "t3"}) // stored before outcome

	res, err := State(context.Background(), env, StateInput{OrderNumber: 42})
	if err != nil {
		t.Fatal(err)
	}
	im := res.Imports
	if im == nil || !im.Untrusted || im.Stored != 3 || im.Scanned != 3 || im.ByOutcome["imported"] != 1 || im.ByOutcome["refused"] != 1 || im.ByOutcome["retry"] != 1 {
		t.Fatalf("imports %+v", im)
	}
	if len(im.Recent) != 2 || im.Recent[0].OrderNumber != 43 || im.Recent[1].OrderNumber != 42 || len(im.Recent[1].Codes) != 1 || im.Recent[1].Codes[0] != "Stock.002" {
		t.Fatalf("recent %+v", im.Recent)
	}
	if im.Order == nil || im.Order.RunLog == nil || im.Order.RunLog.Refusals[0].Value != "1001" {
		t.Fatalf("order 42 %+v", im.Order)
	}
	res, _ = State(context.Background(), env, StateInput{OrderNumber: 7})
	if res.Imports == nil || res.Imports.Order != nil || res.Imports.OrderMissing == "" {
		t.Fatalf("order 7: %+v", res.Imports)
	}
	if _, err := State(context.Background(), env, StateInput{OrderNumber: -1}); err == nil {
		t.Fatalf("a negative order number accepted")
	}
}
