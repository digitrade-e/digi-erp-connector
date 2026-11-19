package hasavshevet

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// fakeImportLog is an import log in memory: the rows logged after the mark,
// and the Stock documents (by ID) with their DocNumber / Reference.
type fakeImportLog struct {
	markID  int64
	markErr error
	runs    []importLogRun
	runsErr error
	stock   []fakeStock
	findErr error

	gotMark int64 // the mark runsSince was asked about
}

type fakeStock struct {
	id, documentID, status, docNumber, reference int64
}

func (f *fakeImportLog) mark(context.Context) (int64, error) { return f.markID, f.markErr }

func (f *fakeImportLog) runsSince(_ context.Context, mark int64) ([]importLogRun, error) {
	f.gotMark = mark
	if f.runsErr != nil {
		return nil, f.runsErr
	}
	var out []importLogRun
	for _, r := range f.runs {
		if r.ID > mark {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeImportLog) findStock(_ context.Context, first, last, number, reference int64) ([]stockHit, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	var out []stockHit
	for _, s := range f.stock {
		if s.id < first || s.id > last || (s.docNumber != number && s.reference != reference) {
			continue
		}
		by := "reference"
		if s.docNumber == number {
			by = "docNumber"
		}
		out = append(out, stockHit{ID: s.id, DocumentID: s.documentID, Status: s.status, MatchedBy: by})
	}
	return out, nil
}

// A row shaped as Hashavshevet 2026a SP2.1 writes it for a Masofon run.
func masofonRun(id int64, bat, file, status string, first, last int64) importLogRun {
	return importLogRun{
		ID:       id,
		FileName: file,
		Command:  bat + "=company -6 -" + file + " -FixedLength -Ansi2Ansi -x.prm - -user - -0 -0 - -0 -YES -&Company=Co&Status=" + status,
		ExtraData: "StationID=box_1&FirstStockID=" + strconv.FormatInt(first, 10) +
			"&LastStockID=" + strconv.FormatInt(last, 10) + "&Ver=2026a SP2.1 &finish=11:07:51",
	}
}

func TestParseImportLogRun(t *testing.T) {
	cases := []struct {
		name            string
		run             importLogRun
		profile, status string
		first, last     int64
		hasRange        bool
	}{
		{"masofon success", masofonRun(1, "digi.bat", `C:\o\IMOVEIN.doc`, "Success", 10, 12), "digi.bat", "Success", 10, 12, true},
		{"masofon failed", masofonRun(1, "digi.bat", `C:\o\IMOVEIN.doc`, "Failed", 10, 10), "digi.bat", "Failed", 10, 10, true},
		// A company name holding "&Status=" left an empty status first: the
		// first non-empty one counts.
		{"empty status first", importLogRun{Command: "a.bat=x&Company=Co&Status=&Status=Success", ExtraData: "FirstStockID=5&LastStockID=5&Ver=1"}, "a.bat", "Success", 5, 5, true},
		// A manual import: no profile, may be partial.
		{"manual partial", importLogRun{Command: "No&Company=Co&Status=partial", ExtraData: "StationID=o_1&FirstStockID=7&LastStockID=9&Ver=1"}, "", "partial", 7, 9, true},
		{"no range", importLogRun{Command: "a.bat=x&Status=Success", ExtraData: ".finish time:10:17:58"}, "a.bat", "Success", 0, 0, false},
		{"inverted range", importLogRun{Command: "a.bat=x&Status=Success", ExtraData: "FirstStockID=9&LastStockID=7&Ver=1"}, "a.bat", "Success", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, s, f, l, ok := parseImportLogRun(tc.run)
			if p != tc.profile || s != tc.status || f != tc.first || l != tc.last || ok != tc.hasRange {
				t.Fatalf("got (%q, %q, %d, %d, %v); want (%q, %q, %d, %d, %v)", p, s, f, l, ok, tc.profile, tc.status, tc.first, tc.last, tc.hasRange)
			}
		})
	}
}

func TestSameImportFile(t *testing.T) {
	ours := `C:\digi\orders\IMOVEIN.doc`
	for _, logged := range []string{`C:\digi\orders\IMOVEIN.doc`, `c:\DIGI\Orders\imovein.DOC`, `"C:\digi\orders\IMOVEIN.doc"`, ` C:\digi\orders\IMOVEIN.doc `, `C:/digi/orders/IMOVEIN.doc`} {
		if !sameImportFile(logged, ours) {
			t.Errorf("sameImportFile(%q) = false; want true", logged)
		}
	}
	for _, logged := range []string{"", `C:\digi\orders\IMOVEIN_1.doc`, `E:\other\orders\imovein.doc`, `"@E:\x.xlsx"`} {
		if sameImportFile(logged, ours) {
			t.Errorf("sameImportFile(%q) = true; want false", logged)
		}
	}
}

func TestCheckImport(t *testing.T) {
	const ours, bat = `C:\o\IMOVEIN.doc`, `C:\Hash7\digi.bat`
	const number, reference = 1000123, 555
	ctx := context.Background()
	cases := []struct {
		name      string
		log       *fakeImportLog
		want      string
		matchedBy string
		stockIDs  []int64
		detail    bool // a detail is expected
	}{
		{"imported by DocNumber", &fakeImportLog{
			runs:  []importLogRun{masofonRun(11, "digi.bat", ours, "Success", 100, 101)},
			stock: []fakeStock{{id: 100, documentID: 11, status: 1, docNumber: number}},
		}, OrderImportImported, "docNumber", []int64{100}, false},
		// An auto-numbered type replaced DocNumber on issue; line 8 kept it.
		{"imported by Reference", &fakeImportLog{
			runs:  []importLogRun{masofonRun(11, "digi.bat", ours, "Success", 100, 101)},
			stock: []fakeStock{{id: 101, documentID: 6, status: 1, docNumber: 35498, reference: reference}},
		}, OrderImportImported, "reference", []int64{101}, false},
		{"success, no document carries the number", &fakeImportLog{
			runs:  []importLogRun{masofonRun(11, "digi.bat", ours, "Success", 100, 101)},
			stock: []fakeStock{{id: 100, docNumber: 1, reference: 2}},
		}, OrderImportLoggedNoDocument, "", nil, true},
		{"failed", &fakeImportLog{
			runs: []importLogRun{masofonRun(11, "digi.bat", ours, "Failed", 0, 0)},
		}, OrderImportFailed, "", nil, false},
		// Other writers' runs in the same window are not ours.
		{"not logged", &fakeImportLog{
			runs: []importLogRun{masofonRun(11, "pep.bat", `E:\p\imovein.doc`, "Success", 100, 100)},
		}, OrderImportNotLogged, "", nil, true},
		{"a run before the mark is not ours", &fakeImportLog{
			markID: 10,
			runs:   []importLogRun{masofonRun(9, "digi.bat", ours, "Success", 100, 100)},
			stock:  []fakeStock{{id: 100, docNumber: number}},
		}, OrderImportNotLogged, "", nil, true},
		// Another profile read our file: still reported, with a warning.
		{"another profile", &fakeImportLog{
			runs:  []importLogRun{masofonRun(11, "pep_ordr.bat", ours, "Success", 100, 100)},
			stock: []fakeStock{{id: 100, docNumber: number}},
		}, OrderImportImported, "docNumber", []int64{100}, true},
		{"log not readable", &fakeImportLog{runsErr: errors.New("driver: login failed for user x on server y")}, OrderImportUnchecked, "", nil, true},
		{"stock not readable", &fakeImportLog{
			runs:    []importLogRun{masofonRun(11, "digi.bat", ours, "Success", 100, 100)},
			findErr: errors.New("driver text"),
		}, OrderImportUnchecked, "", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := checkImport(ctx, tc.log, tc.log.markID, ours, bat, number, reference)
			if got.Result != tc.want || got.MatchedBy != tc.matchedBy || !reflect.DeepEqual(got.StockIDs, tc.stockIDs) {
				t.Fatalf("checkImport = %+v; want result %s matchedBy %q stockIds %v", got, tc.want, tc.matchedBy, tc.stockIDs)
			}
			if (got.Detail != "") != tc.detail {
				t.Errorf("detail = %q; want one: %v", got.Detail, tc.detail)
			}
			if got.CheckedAt == "" {
				t.Error("checkedAt is empty")
			}
			// Driver text never reaches the report (it is for the log).
			if tc.log.runsErr != nil || tc.log.findErr != nil {
				if got.Detail == "" || strings.Contains(got.Detail, "driver") || strings.Contains(got.Detail, "login") || strings.Contains(got.Detail, "server") {
					t.Errorf("detail %q leaks the driver error", got.Detail)
				}
			}
		})
	}
}

// batSender is a model (b) sender whose BAT importer and import log are fakes.
func batSender(t *testing.T, timeoutSeconds int, log importLogReader, bat func(context.Context, string) (int, string, error)) (*Sender, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{ERP: config.ERPHasavshevet, SendOrderDir: dir, HasBatFile: filepath.Join(dir, "digi.bat"), HasImportTimeoutSeconds: timeoutSeconds}
	if err := cfg.ValidateOrders(); err != nil {
		t.Fatalf("the test config is refused: %v", err)
	}
	s := NewSender(nil, cfg, NewOrderNumberStore(filepath.Join(dir, "lastOrderNumber.json")), noopLogger{})
	s.runBat = bat
	s.importLog = log
	return s, dir
}

// The order's run is confirmed from the rows logged after the mark taken
// before the importer started; the order is done either way.
func TestSender_ConfirmsTheImportFromTheLog(t *testing.T) {
	log := &fakeImportLog{markID: 50}
	var s *Sender
	var dir string
	s, dir = batSender(t, 0, log, func(context.Context, string) (int, string, error) {
		// The importer logs its run and creates the document.
		log.runs = append(log.runs, masofonRun(51, "digi.bat", filepath.Join(dir, "IMOVEIN.doc"), "Success", 700, 700))
		log.stock = append(log.stock, fakeStock{id: 700, documentID: 11, status: 1, docNumber: 42})
		return 0, "", nil
	})
	res, err := s.processOrderWithNumber(context.Background(), drainTestOrder(1), 42)
	if err != nil {
		t.Fatalf("processOrderWithNumber: %v", err)
	}
	if log.gotMark != 50 {
		t.Errorf("rows read after mark %d; want 50 (taken before the importer ran)", log.gotMark)
	}
	c := res.Import
	if c == nil || c.Result != OrderImportImported || !reflect.DeepEqual(c.StockIDs, []int64{700}) || c.DocumentID != 11 || c.StockStatus == nil || *c.StockStatus != 1 || c.Profile != "digi.bat" || c.TimedOut {
		t.Fatalf("import check = %+v; want imported, stock 700, type 11, status 1, profile digi.bat", c)
	}
}

// No database: the order is written and imported as before; the check says
// it could not look.
func TestSender_ImportUncheckedWithoutADatabase(t *testing.T) {
	s, _ := batSender(t, 0, nil, func(context.Context, string) (int, string, error) { return 0, "", nil })
	res, err := s.processOrderWithNumber(context.Background(), drainTestOrder(1), 42)
	if err != nil {
		t.Fatalf("processOrderWithNumber: %v", err)
	}
	if res.Import == nil || res.Import.Result != OrderImportUnchecked {
		t.Fatalf("import check = %+v; want unchecked", res.Import)
	}
}

// The timeout is opt-in: unset, the importer runs without a deadline (today's
// behaviour); set, it runs under one and a run cut short is reported.
func TestSender_ImportTimeout(t *testing.T) {
	t.Run("unset: no deadline", func(t *testing.T) {
		s, _ := batSender(t, 0, nil, func(ctx context.Context, _ string) (int, string, error) {
			if _, ok := ctx.Deadline(); ok {
				t.Error("the importer ran under a deadline with hasImportTimeoutSeconds unset")
			}
			return 0, "", nil
		})
		if _, err := s.processOrderWithNumber(context.Background(), drainTestOrder(1), 42); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("set: the run has the deadline", func(t *testing.T) {
		s, _ := batSender(t, 30, &fakeImportLog{}, func(ctx context.Context, _ string) (int, string, error) {
			dl, ok := ctx.Deadline()
			if !ok || time.Until(dl) > 30*time.Second || time.Until(dl) < 25*time.Second {
				t.Errorf("deadline in %v (set %v); want about 30 s", time.Until(dl), ok)
			}
			return 0, "", nil
		})
		res, err := s.processOrderWithNumber(context.Background(), drainTestOrder(1), 42)
		if err != nil {
			t.Fatal(err)
		}
		if res.Import == nil || res.Import.TimedOut {
			t.Fatalf("import check = %+v; want one, not timed out", res.Import)
		}
	})
	t.Run("a run outliving the cap is cut and reported", func(t *testing.T) {
		s, _ := batSender(t, 30, &fakeImportLog{}, func(ctx context.Context, _ string) (int, string, error) {
			<-ctx.Done() // a run that never ends on its own (a message box)
			return -1, "", ctx.Err()
		})
		s.cfg.HasImportTimeoutSeconds = 1 // below the validated minimum, for the test's sake
		res, err := s.processOrderWithNumber(context.Background(), drainTestOrder(1), 42)
		if err != nil {
			t.Fatalf("a timed-out import must not fail the written order: %v", err)
		}
		if res.Import == nil || !res.Import.TimedOut || res.Import.Result != OrderImportNotLogged {
			t.Fatalf("import check = %+v; want timedOut, not_logged", res.Import)
		}
	})
}

func TestValidateOrders_ImportTimeout(t *testing.T) {
	base := config.Config{ERP: config.ERPHasavshevet, SendOrderDir: t.TempDir()}
	for _, v := range []int{0, 30, 600, 3600} {
		c := base
		c.HasImportTimeoutSeconds = v
		if err := c.ValidateOrders(); err != nil {
			t.Errorf("hasImportTimeoutSeconds %d refused: %v", v, err)
		}
	}
	for _, v := range []int{-1, 1, 29, 3601} {
		c := base
		c.HasImportTimeoutSeconds = v
		if err := c.ValidateOrders(); err == nil {
			t.Errorf("hasImportTimeoutSeconds %d accepted", v)
		}
	}
}

// A written order's check is kept beside the index: a status answered after a
// restart (from the index entry) still carries it; a damaged file costs only
// the report.
func TestOrderQueue_ImportCheckSurvivesARestart(t *testing.T) {
	h := newOrderHarness(t)
	q := h.queue(true)
	log := &fakeImportLog{}
	q.sender.cfg.HasBatFile = filepath.Join(h.sendDir, "digi.bat")
	q.sender.importLog = log
	q.sender.runBat = func(context.Context, string) (int, string, error) {
		log.runs = append(log.runs, masofonRun(1, "digi.bat", filepath.Join(h.sendDir, "IMOVEIN.doc"), "Failed", 0, 0))
		return 0, "", nil
	}
	acc := mustAccept(t, q, keyedOrder("K-import"))
	res := waitForStatus(t, q, acc.JobID, JobStatusDone)
	if res.Import == nil || res.Import.Result != OrderImportFailed {
		t.Fatalf("in-memory status import = %+v; want failed", res.Import)
	}

	q2 := h.newQueue() // a new process over the same folders
	if err := q2.Recover(); err != nil {
		t.Fatal(err)
	}
	got, ok := q2.Status(acc.JobID)
	if !ok || got.Status != JobStatusDone || got.Import == nil || got.Import.Result != OrderImportFailed {
		t.Fatalf("status after restart = %+v (found %v); want done with the failed import check", got, ok)
	}
}

func TestValidateOrders_RefusedAndPrecheckSwitches(t *testing.T) {
	dir := t.TempDir()
	ok := []config.Config{
		{ERP: config.ERPHasavshevet, SendOrderDir: dir, HasBatFile: `C:\Hash7\digi.bat`, HasMoveRefusedOrders: true},
		{ERP: config.ERPHasavshevet, SendOrderDir: dir, HasOrderPrecheck: true, DB: config.DBConfig{Database: "company_db"}},
	}
	for i, c := range ok {
		if err := c.ValidateOrders(); err != nil {
			t.Errorf("ok[%d] refused: %v", i, err)
		}
	}
	bad := []config.Config{
		{ERP: config.ERPHasavshevet, SendOrderDir: dir, HasMoveRefusedOrders: true},                                                                                             // no importer
		{ERP: config.ERPHasavshevet, SendOrderDir: dir, HasOrderPrecheck: true},                                                                                                 // no database
		{ERP: config.ERPHasavshevet, SendOrderDir: dir, SendOrderPickupDir: t.TempDir(), SendOrderFormat: "legacy", HasExePath: `C:\Hash7\has.exe`, HasMoveRefusedOrders: true}, // model (a)
	}
	for i, c := range bad {
		if err := c.ValidateOrders(); err == nil {
			t.Errorf("bad[%d] accepted", i)
		}
	}
}
