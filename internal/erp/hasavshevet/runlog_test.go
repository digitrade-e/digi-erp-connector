package hasavshevet

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// Run-log fixtures, shaped as FAST_IMP.HDS writes them (synthetic names):
// a block per command, a section per file the command read.

func runBlock(bat, command string, sections ...string) string {
	return "\n[box_1." + bat + "] --->" + command + "\n" + strings.Join(sections, "")
}

func importedSection(file string) string {
	return "[19/11/26 11:00:01] " + file + ":\n" +
		"[19/11/26 11:00:03] קליטת תנועות מלאי הסתיימה בהצלחה\n" +
		"[19/11/26 11:00:03] =============================\n" +
		"[19/11/26 11:00:03] \n"
}

func refusedSection(file string, lines ...string) string {
	s := "[19/11/26 11:00:01] " + file + ":\n"
	for _, l := range lines {
		s += "[19/11/26 11:00:02] " + l + "\n"
	}
	return s
}

func notFoundLine(pattern string) string { return pattern + ":  File Not Found(SetDsn)\n" }

const (
	itemRefusal      = "ITEM-1\tStock.002 פריט לא קיים\t2\t"
	duplicateRefusal = "77\tStock.063B מסמך בספרור זה כבר נקלט בעבר\t1\t" // the code as a live box writes it
	baseQRefusal     = "stockmoves.id = 9 has baseQ = 0.00\tהכמות לסגירה לא יכולה להיות אפס\t1\t"
	systemErrorLine  = "[19/11/26 11:00:04] WizImportError.log  תקלת מערכת, פרטים בקבצי לוג במחיצת התוכנה\n"
)

// writeRunLog writes a Masofon run log as FAST_IMP.HDS names it.
func writeRunLog(t *testing.T, dir, bat, kind string, at time.Time, body string) string {
	t.Helper()
	p := filepath.Join(dir, bat+"."+kind+".DMY-"+at.Format("02.01.06")+".HMS-"+at.Format("15.04.05")+".LOG")
	if err := os.WriteFile(p, win1255(t, body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseMasofonINI_FolderlessFilesUseDataPath(t *testing.T) {
	ini, err := ParseMasofonINI(win1255(t, "[commands]\r\nrel.bat=company_db -6 -imovein.doc -FixedLength -Ansi2Ansi -imovein.prm - -u - -0 -0 - -0 - -;\r\n"+
		"[general]\r\nErrDataPath=E:\\logs\r\nDataPath6=E:\\HASH\\REP\\\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := ini.Profile("rel.bat")
	if len(c) != 1 || c[0].DataFile != `E:\HASH\REP\imovein.doc` || c[0].PrmFile != `E:\HASH\REP\imovein.prm` {
		t.Fatalf("rel.bat = %+v; want the files in DataPath6 ([general] after [commands])", c)
	}
}

// One wildcard run, two commands: each file's result is its own.
func TestParseRunLog_OneSectionPerFile(t *testing.T) {
	a, b, bad := `c:\wms\in\imovein_1.dat`, `c:\wms\in\imovein_2.dat`, `c:\wms\in\imovein_3.dat`
	body := runBlock("wms.bat", `company_db -6 -c:\wms\in\imovein*.dat -FixedLength`,
		importedSection(a), refusedSection(bad, baseQRefusal), importedSection(b)) +
		runBlock("wms.bat", `company_db -118 -c:\wms\int\imovein*.dat -FixedLength`, notFoundLine(`c:\wms\int\imovein*.dat`))
	p := parseRunLog(body)

	if rl := p.forFile("x.LOG", `C:\WMS\IN\IMOVEIN_1.DAT`, false); rl == nil || rl.Section != RunLogImported || len(rl.Errors) != 0 || rl.OtherFiles != 2 {
		t.Fatalf("imported file = %+v; want imported, no errors, the two other files counted", rl)
	}
	rl := p.forFile("x.LOG", bad, false)
	if rl == nil || rl.Section != RunLogRefused || len(rl.Refusals) != 1 {
		t.Fatalf("refused file = %+v", rl)
	}
	if r := rl.Refusals[0]; r.Value != "stockmoves.id = 9 has baseQ = 0.00" || r.Message != "הכמות לסגירה לא יכולה להיות אפס" || r.Row != 1 || r.Code != "" {
		t.Errorf("refusal = %+v", r)
	}
	if rl := p.forFile("x.LOG", `c:\wms\int\imovein_9.dat`, false); rl == nil || rl.Section != RunLogNoInput || !rl.NoInput {
		t.Errorf("file of the command that found nothing = %+v; want no_input", rl)
	}
	if rl := p.forFile("x.LOG", `c:\digi\orders\imovein.doc`, false); rl != nil {
		t.Errorf("a file the run never read = %+v; want nil (another run's log)", rl)
	}
}

func TestParseRunLog_Outcomes(t *testing.T) {
	const f = `c:\digi\orders\imovein.doc`
	cmd := `company_db -6 -c:\digi\orders\imovein.doc`
	cases := []struct {
		name, body, want string
		systemError      bool
	}{
		{"refused", runBlock("digi.bat", cmd, refusedSection(f, itemRefusal, "לא נקלטה כל רשומה")), RunLogRefused, false},
		{"duplicate only", runBlock("digi.bat", cmd, refusedSection(f, duplicateRefusal)), RunLogDuplicate, false},
		{"duplicate and more", runBlock("digi.bat", cmd, refusedSection(f, duplicateRefusal, itemRefusal)), RunLogRefused, false},
		{"no record, no line", runBlock("digi.bat", cmd, refusedSection(f, "לא נקלטה כל רשומה")), RunLogRefused, false},
		{"imported, then a system error", runBlock("digi.bat", cmd, importedSection(f)) + systemErrorLine, RunLogImported, true},
		{"semaphore before the file", runBlock("digi.bat", cmd, "[19/11/26 11:00:00] ERROR תחנה אחרת מעדכנת קובץ הגדרות או מפיקה\n", refusedSection(f, itemRefusal)), RunLogRetry, false},
		{"deadlock in the section", runBlock("digi.bat", cmd, refusedSection(f, "x\tdeadlock victim\t1\t")), RunLogRetry, false},
		{"header written twice", runBlock("digi.bat", cmd, "[19/11/26 11:00:00] "+f+":\n"+importedSection(f)), RunLogImported, false},
	}
	for _, c := range cases {
		rl := parseRunLog(c.body).forFile("x.LOG", f, false)
		if rl == nil || rl.Section != c.want || rl.SystemError != c.systemError {
			t.Errorf("%s: %+v; want section %q systemError %v", c.name, rl, c.want, c.systemError)
		}
	}
	if rl := parseRunLog(runBlock("digi.bat", cmd, refusedSection(f, duplicateRefusal))).forFile("x", f, false); rl.Refusals[0].Code != "Stock.063B" || rl.Refusals[0].Value != "77" {
		t.Errorf("duplicate refusal = %+v", rl.Refusals[0])
	}
	// No section header at all: read whole, as before (Section "").
	whole := parseRunLog("[box_1.digi.bat] --->"+cmd+"\n[19/11/26 11:00:01] קליטת תנועות מלאי הסתיימה בהצלחה\n"+systemErrorLine).forFile("x", f, false)
	if whole == nil || whole.Section != "" || !whole.SystemError || whole.Summary == "" {
		t.Errorf("log without sections = %+v; want whole-log reading", whole)
	}
}

// Of the logs written since the run started, the earliest with our file is
// the run that read it; another run's log (other files only) is skipped.
func TestFindRunLog_TheRunThatReadOurFile(t *testing.T) {
	dir := t.TempDir()
	ours := `c:\digi\orders\imovein.doc`
	cmd := `company_db -6 -c:\digi\orders\imovein*.doc`
	start := time.Now().Add(-time.Minute)
	writeRunLog(t, dir, "digi.bat", "ok", start.Add(-time.Hour), runBlock("digi.bat", cmd, importedSection(ours))) // before the run
	writeRunLog(t, dir, "digi.bat", "err", start.Add(5*time.Second), runBlock("digi.bat", cmd, refusedSection(`c:\digi\orders\imovein_x.doc`, itemRefusal)))
	want := writeRunLog(t, dir, "digi.bat", "err", start.Add(10*time.Second), runBlock("digi.bat", cmd, importedSection(ours), refusedSection(`c:\digi\orders\imovein_y.doc`, itemRefusal)))
	writeRunLog(t, dir, "digi.bat", "err", start.Add(20*time.Second), runBlock("digi.bat", cmd, refusedSection(ours, duplicateRefusal))) // a later re-read
	writeRunLog(t, dir, "wms.bat", "err", start.Add(6*time.Second), runBlock("wms.bat", cmd, refusedSection(ours, itemRefusal)))         // another BAT

	rl, err := FindRunLog(dir, `C:\Hash7\digi.bat`, ours, start)
	if err != nil || rl == nil {
		t.Fatalf("FindRunLog = %v, %v", rl, err)
	}
	if rl.File != want || rl.Section != RunLogImported || rl.OK || rl.OtherFiles != 1 || len(rl.Errors) != 0 {
		t.Fatalf("run log = %+v; want %s, our section imported though the log is .err", rl, want)
	}
	if rl, _ := FindRunLog(dir, "digi.bat", ours, time.Now().Add(time.Hour)); rl != nil {
		t.Errorf("a log older than the run was taken: %+v", rl)
	}
	if rl, _ := FindRunLog("", "digi.bat", ours, start); rl != nil {
		t.Errorf("no folder gave %+v", rl)
	}
}

// A stopped run never stamps its log: the <bat>.err it was writing is read.
func TestFindRunLog_LiveLogOfAStoppedRun(t *testing.T) {
	dir := t.TempDir()
	ours := `c:\digi\orders\imovein.doc`
	if err := os.WriteFile(filepath.Join(dir, "digi.bat.err"), win1255(t, runBlock("digi.bat", "c", refusedSection(ours, itemRefusal))), 0o644); err != nil {
		t.Fatal(err)
	}
	rl, err := FindRunLog(dir, "digi.bat", ours, time.Now().Add(-time.Minute))
	if err != nil || rl == nil || !rl.Live || rl.Section != RunLogRefused {
		t.Fatalf("live log = %+v, %v; want the unfinished refused section", rl, err)
	}
}

// sysLogLine writes a WizImportError line time-stamped as WizIni.dll does
// (hour not padded).
func sysLogLine(at time.Time, rest string) string {
	return fmt.Sprintf("%s %d:%s:%03d  %s", at.Format("2006-01-02"), at.Hour(), at.Format("04:05"), at.Nanosecond()/1e6, rest)
}

func TestSystemErrorLines(t *testing.T) {
	hasDir := t.TempDir()
	logs := filepath.Join(hasDir, "DataImpExpLogs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	body := strings.Join([]string{
		sysLogLine(now.Add(-48*time.Hour), "WizIni.dll #digi.bat#  ERROR קליטה הופסקה עקב הקלקה כפולה"), // long before
		sysLogLine(now, "WizIni.dll #wms.bat#  ERROR קליטה הופסקה עקב הקלקה כפולה"),                     // another profile
		sysLogLine(now, "WizIni.dll #digi.bat#  ERROR קליטה הופסקה עקב הקלקה כפולה"),
		sysLogLine(now, "Prm2Ini.dll  ERROR file does not exists: _access( imovein.doc, 0 )"),
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(logs, "WizImportError_BOX.log"), win1255(t, body), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err := SystemErrorLines(hasDir, `C:\Hash7\digi.bat`, now.Add(-time.Minute), now.Add(time.Second))
	if err != nil || len(lines) != 2 || !strings.Contains(lines[0], "#digi.bat#") || !strings.Contains(lines[1], "Prm2Ini") {
		t.Fatalf("lines = %q, %v; want this profile's and the untagged line, in the run's window", lines, err)
	}
	if rl := attachSystemErrors(nil, lines); rl == nil || !rl.Transient || rl.Section != RunLogRetry {
		t.Errorf("a double-click refusal = %+v; want retry", rl)
	}
}

func TestImportOutcome(t *testing.T) {
	sec := func(s string) *RunLog { return &RunLog{File: "x", Section: s} }
	cases := []struct {
		name string
		c    ImportCheck
		want string
	}{
		{"ImportLog imported", ImportCheck{Result: OrderImportImported, RunLog: &RunLog{File: "x", Errors: []string{"e"}}}, OrderOutcomeImported},
		{"ImportLog no document", ImportCheck{Result: OrderImportLoggedNoDocument}, OrderOutcomeImported},
		{"ImportLog failed", ImportCheck{Result: OrderImportFailed}, OrderOutcomeRefused},
		{"ImportLog failed, section imported", ImportCheck{Result: OrderImportFailed, RunLog: sec(RunLogImported)}, OrderOutcomeRefused},
		{"section imported", ImportCheck{Result: OrderImportNotLogged, RunLog: sec(RunLogImported)}, OrderOutcomeImported},
		{"section refused", ImportCheck{Result: OrderImportFailed, RunLog: sec(RunLogRefused)}, OrderOutcomeRefused},
		{"section duplicate", ImportCheck{Result: OrderImportFailed, RunLog: sec(RunLogDuplicate)}, OrderOutcomeDuplicate},
		{"section retry", ImportCheck{Result: OrderImportFailed, RunLog: sec(RunLogRetry)}, OrderOutcomeRetry},
		{"section no input", ImportCheck{Result: OrderImportNotLogged, RunLog: sec(RunLogNoInput)}, OrderOutcomeNotRun},
		{"whole log transient", ImportCheck{Result: OrderImportFailed, RunLog: &RunLog{File: "x", Transient: true}}, OrderOutcomeRetry},
		{"whole log no input", ImportCheck{Result: OrderImportNotLogged, RunLog: &RunLog{File: "x", NoInput: true}}, OrderOutcomeNotRun},
		{"whole err log with errors", ImportCheck{Result: OrderImportNotLogged, RunLog: &RunLog{File: "x", Errors: []string{"e"}}}, OrderOutcomeRefused},
		{"whole err log, no line", ImportCheck{Result: OrderImportNotLogged, RunLog: &RunLog{File: "x"}}, OrderOutcomeUnknown},
		{"system errors only", ImportCheck{Result: OrderImportNotLogged, RunLog: &RunLog{SystemErrors: []string{"x"}}}, OrderOutcomeUnknown},
		{"nothing", ImportCheck{Result: OrderImportUnchecked}, OrderOutcomeUnknown},
		{"timed out", ImportCheck{Result: OrderImportNotLogged, TimedOut: true}, OrderOutcomeUnknown},
	}
	for _, c := range cases {
		if got := importOutcome(&c.c); got != c.want {
			t.Errorf("%s: outcome %q; want %q", c.name, got, c.want)
		}
	}
}

// batSenderWithINI is batSender whose BAT folder holds a masofon.ini whose
// ErrDataPath is logs; the profile reads <dir>\imovein.doc.
func batSenderWithINI(t *testing.T, moveRefused bool, bat func(ctx context.Context, batPath string, s *Sender, dir string) (int, string, error)) (*Sender, string, string) {
	t.Helper()
	var s *Sender
	var dir string
	logs := t.TempDir()
	s, dir = batSender(t, 0, &fakeImportLog{}, func(ctx context.Context, p string) (int, string, error) { return bat(ctx, p, s, dir) })
	ini := strings.ReplaceAll(masofonFixture, `c:\digi\orders`, dir)
	ini = strings.Replace(ini, `E:\logs\orders`, logs, 1)
	if err := os.WriteFile(filepath.Join(dir, MasofonININame), win1255(t, ini), 0o644); err != nil {
		t.Fatal(err)
	}
	s.cfg.HasMoveRefusedOrders = moveRefused
	return s, dir, logs
}

// runOrder runs order 42 through a sender whose importer writes body as the
// run's log (kind ok / err) and returns the check.
func runOrder(t *testing.T, move bool, kind string, body func(ourFile string) string) (*ImportCheck, string) {
	t.Helper()
	var logs string
	s, dir, l := batSenderWithINI(t, move, func(_ context.Context, _ string, _ *Sender, dir string) (int, string, error) {
		writeRunLog(t, logs, "digi.bat", kind, time.Now(), body(filepath.Join(dir, "imovein.doc")))
		return 0, "", nil
	})
	logs = l
	res, err := s.processOrderWithNumber(context.Background(), drainTestOrder(1), 42)
	if err != nil {
		t.Fatal(err)
	}
	if res.Import == nil {
		t.Fatal("no import check")
	}
	return res.Import, dir
}

// A refusal is not in ImportLog: the order's own section decides, and with
// hasMoveRefusedOrders the refused file leaves sendOrderDir.
func TestSender_RunLogDecidesAndRefusedFileMoves(t *testing.T) {
	for _, move := range []bool{false, true} {
		c, dir := runOrder(t, move, "err", func(f string) string {
			return runBlock("digi.bat", "company_db -6 -"+f, refusedSection(f, itemRefusal, "לא נקלטה כל רשומה"))
		})
		if c.Outcome != OrderOutcomeRefused || c.Result != OrderImportFailed || c.RunLog == nil || c.RunLog.Section != RunLogRefused ||
			len(c.RunLog.Refusals) != 1 || c.RunLog.Refusals[0].Code != "Stock.002" || c.RunLog.Refusals[0].Row != 2 {
			t.Fatalf("move=%v: import check = %+v run log %+v; want refused with the Stock.002 refusal", move, c, c.RunLog)
		}
		_, stillThere := os.Stat(filepath.Join(dir, "IMOVEIN.doc"))
		moved := filepath.Join(dir, "refused", "IMOVEIN_42.doc")
		_, movedErr := os.Stat(moved)
		if move && (stillThere == nil || movedErr != nil || c.MovedTo != moved) {
			t.Errorf("move=true: IMOVEIN.doc still there=%v, moved file err=%v, movedTo=%q", stillThere == nil, movedErr, c.MovedTo)
		}
		if !move && (stillThere != nil || movedErr == nil || c.MovedTo != "") {
			t.Errorf("move=false: the refused file was moved (opt-in only)")
		}
	}
}

// Another file of the same run was refused; ours imported: the .err log is
// not the order's, nothing is moved.
func TestSender_AnotherFilesRefusalIsNotTheOrders(t *testing.T) {
	c, dir := runOrder(t, true, "err", func(f string) string {
		other := filepath.Join(filepath.Dir(f), "imovein_other.doc")
		return runBlock("digi.bat", "company_db -6 -"+f, refusedSection(other, itemRefusal), importedSection(f))
	})
	if c.Outcome != OrderOutcomeImported || c.RunLog == nil || c.RunLog.Section != RunLogImported || len(c.RunLog.Errors) != 0 || c.RunLog.OtherFiles != 1 || c.MovedTo != "" {
		t.Fatalf("import check = %+v run log %+v; want imported, the other file's refusal not reported", c, c.RunLog)
	}
	if _, err := os.Stat(filepath.Join(dir, "IMOVEIN.doc")); err != nil {
		t.Errorf("the file was moved: %v", err)
	}
}

// Already imported (Stock.063): duplicate, never moved.
func TestSender_DuplicateIsNotMoved(t *testing.T) {
	c, _ := runOrder(t, true, "err", func(f string) string {
		return runBlock("digi.bat", "company_db -6 -"+f, refusedSection(f, duplicateRefusal))
	})
	if c.Outcome != OrderOutcomeDuplicate || c.MovedTo != "" {
		t.Fatalf("import check = %+v; want duplicate, not moved", c)
	}
}

// The run found no input file: the profile reads another file (P2's defect).
func TestSender_RunLogNoInput(t *testing.T) {
	c, _ := runOrder(t, true, "err", func(f string) string {
		return runBlock("digi.bat", "company_db -6 -"+f, notFoundLine(f))
	})
	if c.Outcome != OrderOutcomeNotRun || c.Result != OrderImportNotLogged || c.RunLog == nil || !c.RunLog.NoInput || c.MovedTo != "" ||
		!strings.Contains(c.Detail, "masofon_data_file_mismatch") {
		t.Fatalf("import check = %+v; want not_run, noInput, nothing moved", c)
	}
}

// A refusal that passes (another station issuing holds the "Inv" semaphore)
// is retry and never moved aside.
func TestSender_TransientRefusalIsNotMoved(t *testing.T) {
	c, dir := runOrder(t, true, "err", func(f string) string {
		return runBlock("digi.bat", "company_db -6 -"+f, "[19/11/26 11:00:01] ERROR תחנה אחרת מעדכנת קובץ הגדרות או מפיקה\n")
	})
	if c.Outcome != OrderOutcomeRetry || c.RunLog == nil || !c.RunLog.Transient || c.MovedTo != "" {
		t.Fatalf("import check = %+v; want retry, transient, not moved", c)
	}
	if _, err := os.Stat(filepath.Join(dir, "IMOVEIN.doc")); err != nil {
		t.Errorf("the file was moved: %v", err)
	}
}

// A second run of the BAT is refused before any run log is opened; only
// DataImpExpLogs says so.
func TestSender_DoubleClickFromTheImporterErrorLog(t *testing.T) {
	s, _, _ := batSenderWithINI(t, true, func(_ context.Context, _ string, _ *Sender, dir string) (int, string, error) {
		logs := filepath.Join(dir, "DataImpExpLogs")
		if err := os.MkdirAll(logs, 0o755); err != nil {
			return 0, "", err
		}
		line := sysLogLine(time.Now(), "WizIni.dll #digi.bat#  ERROR קליטה הופסקה עקב הקלקה כפולה") + "\r\n"
		return 0, "", os.WriteFile(filepath.Join(logs, "WizImportError_BOX.log"), win1255(t, line), 0o644)
	})
	res, err := s.processOrderWithNumber(context.Background(), drainTestOrder(1), 42)
	if err != nil {
		t.Fatal(err)
	}
	if c := res.Import; c == nil || c.Outcome != OrderOutcomeRetry || c.RunLog == nil || len(c.RunLog.SystemErrors) != 1 || c.MovedTo != "" {
		t.Fatalf("import check = %+v; want retry from the importer error log, not moved", c)
	}
}

// hasRunLogDir is read when masofon.ini is not readable.
func TestSender_HasRunLogDir(t *testing.T) {
	logs := t.TempDir()
	var s *Sender
	s, _ = batSender(t, 0, &fakeImportLog{}, func(context.Context, string) (int, string, error) {
		f := filepath.Join(s.cfg.SendOrderDir, "imovein.doc")
		writeRunLog(t, logs, "digi.bat", "ok", time.Now(), runBlock("digi.bat", "company_db -6 -"+f, importedSection(f)))
		return 0, "", nil
	})
	s.cfg.HasRunLogDir = logs // no masofon.ini anywhere
	res, err := s.processOrderWithNumber(context.Background(), drainTestOrder(1), 42)
	if err != nil {
		t.Fatal(err)
	}
	if c := res.Import; c == nil || c.Outcome != OrderOutcomeImported || c.RunLog == nil || c.RunLog.Section != RunLogImported {
		t.Fatalf("import check = %+v; want imported from hasRunLogDir", c)
	}
}

// A run killed at hasImportTimeoutSeconds leaves <bat>.IsWorking: removed,
// so the next order's run is not refused as a double click.
func TestSender_StoppedRunMarkerIsRemoved(t *testing.T) {
	var marker string
	s, dir := batSender(t, 30, &fakeImportLog{}, func(ctx context.Context, bat string) (int, string, error) {
		marker = bat + ".IsWorking"
		if err := os.WriteFile(marker, []byte("call has.exe"), 0o644); err != nil {
			return 0, "", err
		}
		<-ctx.Done()
		return -1, "", ctx.Err()
	})
	s.cfg.HasImportTimeoutSeconds = 1 // below the validated minimum, for the test's sake
	res, err := s.processOrderWithNumber(context.Background(), drainTestOrder(1), 42)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("the stopped run's marker is still there: %v", err)
	}
	if c := res.Import; c == nil || !c.TimedOut || c.Outcome != OrderOutcomeUnknown || !strings.Contains(c.Detail, "IsWorking") {
		t.Fatalf("import check = %+v; want timedOut, unknown, the removal said", c)
	}

	// A marker older than the run is not ours: left in place.
	old := filepath.Join(dir, "digi.bat.IsWorking")
	if err := os.WriteFile(old, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s.clearStoppedRunMarker(&ImportCheck{}, filepath.Join(dir, "digi.bat"), time.Now().Add(time.Hour), 43)
	if _, err := os.Stat(old); err != nil {
		t.Errorf("a marker older than the run was removed: %v", err)
	}
}

func TestValidateOrders_HasRunLogDir(t *testing.T) {
	dir := t.TempDir()
	ok := config.Config{ERP: config.ERPHasavshevet, SendOrderDir: dir, HasBatFile: `C:\Hash7\digi.bat`, HasRunLogDir: t.TempDir()}
	if err := ok.ValidateOrders(); err != nil {
		t.Errorf("an absolute hasRunLogDir with a BAT refused: %v", err)
	}
	bad := []config.Config{
		{ERP: config.ERPHasavshevet, SendOrderDir: dir, HasBatFile: `C:\Hash7\digi.bat`, HasRunLogDir: `logs`},                                                                 // relative
		{ERP: config.ERPHasavshevet, SendOrderDir: dir, HasRunLogDir: t.TempDir()},                                                                                             // no importer
		{ERP: config.ERPHasavshevet, SendOrderDir: dir, SendOrderPickupDir: t.TempDir(), SendOrderFormat: "legacy", HasExePath: `C:\Hash7\has.exe`, HasRunLogDir: t.TempDir()}, // model (a)
	}
	for i, c := range bad {
		if err := c.ValidateOrders(); err == nil {
			t.Errorf("bad[%d] accepted", i)
		}
	}
}

// A check stored before outcome existed gets one when it is read back.
func TestLoadImportCheck_DerivesTheOutcome(t *testing.T) {
	dir := t.TempDir()
	if err := saveImportCheck(dir, 7, &ImportCheck{Result: OrderImportFailed, RunLog: &RunLog{File: "x", Transient: true}, CheckedAt: "t"}); err != nil {
		t.Fatal(err)
	}
	if c := loadImportCheck(dir, 7); c == nil || c.Outcome != OrderOutcomeRetry {
		t.Fatalf("loaded = %+v; want outcome retry derived", c)
	}
}

// Partial import (Masofon2's "יבוא (חלקי) על אף שגיאות", [general]
// ImportWithErrors=YES): the good lines are imported, the refused dropped.

func TestPartialImport_Profile(t *testing.T) {
	on, err := ParseMasofonINI(win1255(t, strings.Replace(masofonFixture, "[general]\n", "[general]\nImportWithErrors=yes\n", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if !on.PartialImportOn() {
		t.Fatalf("ImportWithErrors=%q not read as on", on.ImportWithErrors)
	}
	got := map[string]bool{}
	for _, f := range CheckMasofonProfile(on, `C:\Hash7\digi.bat`, `c:\digi\orders\IMOVEIN.doc`, `c:\digi\orders\IMOVEIN.prm`) {
		got[f.Code] = f.Defect
	}
	if d, ok := got[MasofonPartialImportOn]; !ok || !d {
		t.Errorf("findings %v; want the defect %s", got, MasofonPartialImportOn)
	}
	off, _ := ParseMasofonINI(win1255(t, masofonFixture))
	for _, f := range CheckMasofonProfile(off, `C:\Hash7\digi.bat`, `c:\digi\orders\IMOVEIN.doc`, `c:\digi\orders\IMOVEIN.prm`) {
		if f.Code == MasofonPartialImportOn {
			t.Errorf("partial import reported with the key absent")
		}
	}
}

func TestPartialImport_RunLogSection(t *testing.T) {
	const f = `c:\digi\orders\imovein.doc`
	body := runBlock("digi.bat", "company_db -6 -"+f, refusedSection(f, itemRefusal, partialImportLine))
	rl := parseRunLog(body).forFile("x", f, false)
	if rl == nil || rl.Section != RunLogPartial || len(rl.Refusals) != 1 || rl.Refusals[0].Code != "Stock.002" {
		t.Fatalf("partial section = %+v; want partial with the dropped line", rl)
	}
	whole := parseRunLog("[box_1.digi.bat] --->c\n[19/11/26 11:00:03] "+partialImportLine+"\n").forFile("x", f, false)
	if c := (ImportCheck{Result: OrderImportNotLogged, RunLog: whole}); importOutcome(&c) != OrderOutcomePartial {
		t.Errorf("a whole log saying partial: outcome %q", importOutcome(&c))
	}
}

// ImportLog logs a partial run "partial": the document is found like a
// success and reported partial.
func TestPartialImport_ImportLog(t *testing.T) {
	ours := `c:\digi\orders\imovein.doc`
	log := &fakeImportLog{markID: 10,
		runs:  []importLogRun{masofonRun(11, "digi.bat", ours, "partial", 700, 700)},
		stock: []fakeStock{{id: 700, documentID: 11, status: 1, docNumber: 42}}}
	c, err := checkImport(context.Background(), log, 10, ours, `C:\Hash7\digi.bat`, 42, 42)
	if err != nil {
		t.Fatal(err)
	}
	if c.Result != OrderImportPartial || len(c.StockIDs) != 1 || c.StockIDs[0] != 700 || importOutcome(&c) != OrderOutcomePartial {
		t.Fatalf("check = %+v; want partial with the document found", c)
	}
	log.stock = nil
	if c, _ := checkImport(context.Background(), log, 10, ours, "", 42, 42); c.Result != OrderImportPartial {
		t.Errorf("partial without the document = %+v; want still partial", c)
	}
}

// End to end: a partial section is reported partial and never moved aside
// (the good lines are in the books; a resend would duplicate them).
func TestSender_PartialImportIsReportedAndNotMoved(t *testing.T) {
	c, dir := runOrder(t, true, "err", func(f string) string {
		return runBlock("digi.bat", "company_db -6 -"+f, refusedSection(f, itemRefusal, partialImportLine))
	})
	if c.Outcome != OrderOutcomePartial || c.Result != OrderImportPartial || c.MovedTo != "" || len(c.RunLog.Refusals) != 1 {
		t.Fatalf("import check = %+v run log %+v; want partial, the dropped line listed, nothing moved", c, c.RunLog)
	}
	if _, err := os.Stat(filepath.Join(dir, "IMOVEIN.doc")); err != nil {
		t.Errorf("the file was moved: %v", err)
	}
}
