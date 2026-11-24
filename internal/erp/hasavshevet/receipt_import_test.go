package hasavshevet

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The shapes below are BFL's (2026-11-24): a receipt run is logged with an
// empty &Status= and "FirstStockID=…​.LastStockID=…", and its run-log section
// ends "יבוא תנועות לקובץ הקופה הסתיים בהצלחה". Numbers and paths are synthetic.

const receiptSuccessLine = "יבוא תנועות לקובץ הקופה הסתיים בהצלחה"

type fakeReceiptImports struct {
	runs    []receiptRun
	docs    []stockDoc
	runsErr error
	docsErr error
	gotName string
}

func (f *fakeReceiptImports) receiptRuns(_ context.Context, name string, _ int64) ([]receiptRun, error) {
	f.gotName = name
	return f.runs, f.runsErr
}

func (f *fakeReceiptImports) stockRange(_ context.Context, first, last int64) ([]stockDoc, error) {
	if f.docsErr != nil {
		return nil, f.docsErr
	}
	var out []stockDoc
	for _, d := range f.docs {
		if d.ID >= first && d.ID <= last {
			out = append(out, d)
		}
	}
	return out, nil
}

// driverTime is how the SQL driver hands back a zone-less datetime: the
// server's wall clock labelled UTC.
func driverTime(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
}

func receiptRunRow(id int64, file, extra string, at time.Time) receiptRun {
	return receiptRun{
		importLogRun: importLogRun{ID: id, FileName: file,
			Command:   "digitrade_reciept.bat=CO -126 -" + file + " -FixedLength&Company=x&Status=",
			ExtraData: extra},
		ImportDate: driverTime(at),
	}
}

const receiptFile = `c:\shop\receipts\kupain_10200.doc`

func receiptLogBody(sections ...string) string {
	return runBlock("digitrade_reciept.bat", `CO -126 -c:\shop\receipts\kupain*.doc -FixedLength`,
		append([]string{
			"[24/11/26 15:34:23] c:\\shop\\receipts\\kupain.doc:\n" +
				"[24/11/26 15:34:23] קובץ קלט ריק\n" +
				"[24/11/26 15:34:24] לא נקלטה כל רשומה\n",
		}, sections...)...)
}

func TestReceiptImportImportedAsDraftWithItsRunLog(t *testing.T) {
	dir := t.TempDir()
	at := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	writeRunLog(t, dir, "digitrade_reciept.bat", "ok", at.Add(14*time.Second), receiptLogBody(
		"[24/11/26 15:34:24] "+receiptFile+":\n"+
			"[24/11/26 15:34:37] "+receiptSuccessLine+"\n"))
	r := &fakeReceiptImports{
		runs: []receiptRun{receiptRunRow(1267157, receiptFile, "FirstStockID=2611232.LastStockID=2611232&Ver=x&finish=15:34:37", at)},
		docs: []stockDoc{{ID: 2611232, DocumentID: 31, DocNumber: 25767, Status: 0}},
	}
	got, err := CheckReceiptImport(context.Background(), r, 10200, "KUPAIN_10200.doc", dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Result != ReceiptImportImported || got.DocNumber != 25767 || got.DocumentID != 31 ||
		got.StockStatus == nil || *got.StockStatus != 0 || len(got.StockIDs) != 1 || got.StockIDs[0] != 2611232 {
		t.Fatalf("got %+v", got)
	}
	if got.Profile != "digitrade_reciept.bat" || got.ImportLogID != 1267157 || got.Runs != 1 {
		t.Fatalf("run fields: %+v", got)
	}
	if got.RunLog == nil || got.RunLog.Section != RunLogImported || !got.RunLog.OK || got.RunLog.OtherFiles != 1 {
		t.Fatalf("run log: %+v (note %q)", got.RunLog, got.RunLogNote)
	}
	if got.ImportedAt != at.Format("2006-01-02T15:04:05") {
		t.Fatalf("importedAt %q, want the server's wall clock %q", got.ImportedAt, at.Format("2006-01-02T15:04:05"))
	}
}

func TestReceiptImportNotLoggedAndLookalikeFiles(t *testing.T) {
	at := time.Now()
	r := &fakeReceiptImports{runs: []receiptRun{
		// LIKE '%[\/]kupain!_10200.doc%' also returns these; neither is ours.
		receiptRunRow(9, `c:\shop\receipts\kupain_10200.doc.bak`, "", at),
		receiptRunRow(8, `c:\shop\receipts\xkupain_10200.doc`, "", at),
	}}
	got, err := CheckReceiptImport(context.Background(), r, 10200, "KUPAIN_10200.doc", "", 0)
	if err != nil || got.Result != ReceiptImportNotLogged || got.Detail == "" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if r.gotName != "KUPAIN_10200.doc" {
		t.Fatalf("searched %q", r.gotName)
	}
}

func TestReceiptImportNewestOfSeveralRuns(t *testing.T) {
	at := time.Now()
	r := &fakeReceiptImports{
		runs: []receiptRun{ // newest first, as the query orders them
			receiptRunRow(20, `"C:\Shop\Receipts\KUPAIN_10200.DOC"`, "FirstStockID=200.LastStockID=200", at),
			receiptRunRow(10, receiptFile, "FirstStockID=100.LastStockID=100", at.Add(-time.Hour)),
		},
		docs: []stockDoc{{ID: 100, DocumentID: 31, DocNumber: 1, Status: 1}, {ID: 200, DocumentID: 31, DocNumber: 2, Status: 1}},
	}
	got, _ := CheckReceiptImport(context.Background(), r, 10200, "KUPAIN_10200.doc", "", 0)
	if got.Result != ReceiptImportImported || got.Runs != 2 || got.ImportLogID != 20 || got.DocNumber != 2 || got.Detail == "" {
		t.Fatalf("got %+v", got)
	}
	if got.RunLogNote == "" {
		t.Fatal("no run-log folder must be said, not silent")
	}
}

func TestReceiptImportRefusedInItsRunLogIsFailed(t *testing.T) {
	dir := t.TempDir()
	at := time.Now().Add(-time.Minute).Truncate(time.Second)
	writeRunLog(t, dir, "digitrade_reciept.bat", "err", at.Add(5*time.Second), receiptLogBody(
		refusedSection(receiptFile, "90\tKupa.017 חשבון לא קיים\t1\t")))
	r := &fakeReceiptImports{runs: []receiptRun{receiptRunRow(5, receiptFile, "", at)}}
	got, err := CheckReceiptImport(context.Background(), r, 10200, "KUPAIN_10200.doc", dir, 0)
	if err != nil || got.Result != ReceiptImportFailed {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got.RunLog == nil || got.RunLog.Section != RunLogRefused || len(got.RunLog.Refusals) != 1 || got.RunLog.Refusals[0].Code != "Kupa.017" {
		t.Fatalf("run log %+v", got.RunLog)
	}
}

func TestReceiptImportNoRangeAndNoVerdictIsNoDocument(t *testing.T) {
	r := &fakeReceiptImports{runs: []receiptRun{receiptRunRow(5, receiptFile, "", time.Now())}}
	got, _ := CheckReceiptImport(context.Background(), r, 10200, "KUPAIN_10200.doc", t.TempDir(), 0)
	if got.Result != ReceiptImportNoDocument || got.RunLogNote == "" {
		t.Fatalf("got %+v", got)
	}
}

func TestReceiptImportLoggedFailedAndDeletedDocument(t *testing.T) {
	failedRun := receiptRunRow(5, receiptFile, "FirstStockID=7.LastStockID=7", time.Now())
	failedRun.Command = "digitrade_reciept.bat=CO&Status=Failed"
	got, _ := CheckReceiptImport(context.Background(), &fakeReceiptImports{runs: []receiptRun{failedRun}}, 10200, "KUPAIN_10200.doc", "", 0)
	if got.Result != ReceiptImportFailed || got.LogStatus != "Failed" {
		t.Fatalf("failed run: %+v", got)
	}
	// The draft was deleted in Hashavshevet after the import: the range is empty.
	r := &fakeReceiptImports{runs: []receiptRun{receiptRunRow(5, receiptFile, "FirstStockID=7.LastStockID=7", time.Now())}}
	got, _ = CheckReceiptImport(context.Background(), r, 10200, "KUPAIN_10200.doc", "", 0)
	if got.Result != ReceiptImportNoDocument || got.Detail == "" {
		t.Fatalf("deleted document: %+v", got)
	}
}

func TestReceiptImportUncheckedNamesNoDriverText(t *testing.T) {
	secret := errors.New("login failed for user 'x' on server SQLSRV\\HIDDEN")
	got, err := CheckReceiptImport(context.Background(), &fakeReceiptImports{runsErr: secret}, 1, "KUPAIN_1.doc", "", 0)
	if !errors.Is(err, secret) || got.Result != ReceiptImportUnchecked || got.Detail != "import log not readable: query failed" {
		t.Fatalf("got %+v, %v", got, err)
	}
	r := &fakeReceiptImports{runs: []receiptRun{receiptRunRow(5, `c:\r\KUPAIN_1.doc`, "FirstStockID=7.LastStockID=7", time.Now())}, docsErr: secret}
	got, _ = CheckReceiptImport(context.Background(), r, 1, "KUPAIN_1.doc", "", 0)
	if got.Result != ReceiptImportUnchecked || got.Detail != "documents not readable: query failed" {
		t.Fatalf("got %+v", got)
	}
}

func TestReceiptRangeTakesBothSeparators(t *testing.T) {
	for _, extra := range []string{
		"FirstStockID=2611232.LastStockID=2611233&Ver=2026a&finish=15:34:37", // a receipt run
		"StationID=S_1&FirstStockID=2611232&LastStockID=2611233&Ver=2026a",   // an order run
	} {
		_, _, first, last, ok := parseImportLogRun(importLogRun{ExtraData: extra})
		if !ok || first != 2611232 || last != 2611233 {
			t.Fatalf("%q: %d-%d %v", extra, first, last, ok)
		}
	}
}

func TestReceiptFileNameAndLikeEscape(t *testing.T) {
	if got := ReceiptFileName("", 10200); got != "KUPAIN_10200.doc" {
		t.Fatal(got)
	}
	if got := ReceiptFileName("R{n}_kupain.doc", 7); got != "R7_kupain.doc" {
		t.Fatal(got)
	}
	if got := escapeLike("a_b%c[d!e"); got != "a!_b!%c![d!!e" {
		t.Fatal(got)
	}
}

func TestRunLogReceiptSuccessLineIsImported(t *testing.T) {
	p := parseRunLog(receiptLogBody("[24/11/26 15:34:24] " + receiptFile + ":\n[24/11/26 15:34:37] " + receiptSuccessLine + "\n"))
	rl := p.forFile("x.LOG", receiptFile, true)
	if rl == nil || rl.Section != RunLogImported || rl.Summary != receiptSuccessLine {
		t.Fatalf("got %+v", rl)
	}
	// The empty bare kupain.doc read before it is its own section, refused, not ours.
	if empty := p.forFile("x.LOG", `c:\shop\receipts\kupain.doc`, true); empty == nil || empty.Section != RunLogRefused {
		t.Fatalf("empty section %+v", empty)
	}
}
