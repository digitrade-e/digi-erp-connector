package hasavshevet

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Receipt import check (plan 096 phase 3).
//
// A receipt's import is not run by the connector on every box: on a split
// install (BFL) the box that writes KUPAIN_<n>.doc has no database, and
// Hashavshevet's own scheduled task imports the file minutes later. So the
// check is a lookup by receipt number, made where the database is: the
// import runs that read the receipt's file (dbo.ImportLog — the file name
// carries the number), the document the newest run created (dbo.Stock: its
// type, Hashavshevet's own number and whether it is still a draft), and that
// run's section of the Masofon run log when the log folder is known. Nothing
// is stored and nothing is acted on: it is read for whoever asks (state
// {receiptNumber}, digi-mcp).
//
// Hashavshevet logs a receipt run with an empty &Status= and its Stock range
// as "FirstStockID=…​.LastStockID=…" (a dot, where order runs have '&'):
// stockRangeRe takes both.

// Receipt import results.
const (
	// ReceiptImportImported: a run read the receipt's file and created a
	// document (StockIDs, StockStatus 0 draft / 1 issued).
	ReceiptImportImported = "imported"
	// ReceiptImportNoDocument: a run read the file, logged no document and
	// its run log does not say it was refused.
	ReceiptImportNoDocument = "no_document"
	// ReceiptImportFailed: a run read the file and failed — logged with
	// another status than Success, or refused in its run log.
	ReceiptImportFailed = "failed"
	// ReceiptImportNotLogged: no run read the file in the import-log rows
	// searched: the importer has not run since it was written, or it reads
	// another folder or file name.
	ReceiptImportNotLogged = "not_logged"
	// ReceiptImportUnchecked: the database could not be read.
	ReceiptImportUnchecked = "unchecked"
)

// ReceiptImport is what Hashavshevet did with one receipt's file.
type ReceiptImport struct {
	ReceiptNumber int64  `json:"receiptNumber"`
	FileName      string `json:"fileName"` // the base name searched for
	Result        string `json:"result"`
	Detail        string `json:"detail,omitempty"`
	// Runs counts the runs that read the file (newest wins); more than one
	// means the file was imported again.
	Runs        int    `json:"runs,omitempty"`
	ImportLogID int64  `json:"importLogId,omitempty"`
	ImportedAt  string `json:"importedAt,omitempty"` // ImportLog.ImportDate, the server's local time
	File        string `json:"file,omitempty"`       // the path the run logged
	Profile     string `json:"profile,omitempty"`    // the Masofon profile (BAT) of the run
	LogStatus   string `json:"logStatus,omitempty"`  // as logged; empty for receipts
	// StockIDs are the documents the run created; DocumentID the company's
	// document type, DocNumber Hashavshevet's own number (not the receipt
	// number), StockStatus 0 draft (temporary) or 1 issued.
	StockIDs    []int64 `json:"stockIds,omitempty"`
	DocumentID  int64   `json:"documentId,omitempty"`
	DocNumber   int64   `json:"docNumber,omitempty"`
	StockStatus *int64  `json:"stockStatus,omitempty"`
	RunLog      *RunLog `json:"runLog,omitempty"`
	// RunLogNote says why no run log is shown, when none is.
	RunLogNote string `json:"runLogNote,omitempty"`
	CheckedAt  string `json:"checkedAt"`
}

// receiptRun is one dbo.ImportLog row that read a receipt's file.
type receiptRun struct {
	importLogRun
	ImportDate time.Time
}

// stockDoc is one dbo.Stock row of a run's range.
type stockDoc struct {
	ID, DocumentID, DocNumber, Status int64
}

// receiptImportReader reads Hashavshevet's import log; a seam for tests.
type receiptImportReader interface {
	// receiptRuns returns the newest runs (at most maxReceiptRuns) among the
	// last window ImportLog rows whose file name ends in fileName.
	receiptRuns(ctx context.Context, fileName string, window int64) ([]receiptRun, error)
	// stockRange returns the documents with ID in [first, last].
	stockRange(ctx context.Context, first, last int64) ([]stockDoc, error)
}

const (
	maxReceiptRuns = 5
	// ReceiptImportWindow is how many of the newest ImportLog rows are
	// searched: ImportLog is indexed on ID only, so the search is a range
	// on it (BFL logs about 2,500 runs a day: some four months).
	ReceiptImportWindow int64 = 300_000
	// receiptRunLogLead is how long before ImportLog's time the run log may
	// have been started.
	receiptRunLogLead = 5 * time.Minute
)

// SQLReceiptImports reads the company database dbName with conn's login.
// dbName must have passed IsSafeDBName.
func SQLReceiptImports(conn *sql.DB, dbName string) receiptImportReader {
	return sqlReceiptImports{db: conn, dbName: dbName}
}

type sqlReceiptImports struct {
	db     *sql.DB
	dbName string
}

func (r sqlReceiptImports) receiptRuns(ctx context.Context, fileName string, window int64) ([]receiptRun, error) {
	// '!' escapes LIKE's wildcards in the name ('_' is one); the name must
	// follow a path separator, and whatever follows it is checked below.
	q := fmt.Sprintf(`SELECT TOP (%d) ID, ISNULL(FileName, ''), ISNULL(IsMasofonCommand, ''), ISNULL(ExtraData, ''), ISNULL(ImportDate, 0)
FROM [%s].[dbo].[ImportLog]
WHERE ID > (SELECT ISNULL(MAX(ID), 0) FROM [%s].[dbo].[ImportLog]) - @window
  AND FileName LIKE @pattern ESCAPE '!'
ORDER BY ID DESC`, maxReceiptRuns, r.dbName, r.dbName)
	rows, err := r.db.QueryContext(ctx, q, sql.Named("window", window),
		sql.Named("pattern", `%[\/]`+escapeLike(fileName)+`%`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []receiptRun
	for rows.Next() {
		var run receiptRun
		if err := rows.Scan(&run.ID, &run.FileName, &run.Command, &run.ExtraData, &run.ImportDate); err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func (r sqlReceiptImports) stockRange(ctx context.Context, first, last int64) ([]stockDoc, error) {
	q := fmt.Sprintf(`SELECT TOP (%d) ID, ISNULL(DocumentID, 0), ISNULL(CAST(DocNumber AS bigint), 0), ISNULL(Status, -1)
FROM [%s].[dbo].[Stock] WHERE ID BETWEEN @first AND @last ORDER BY ID`, maxStockHits, r.dbName)
	rows, err := r.db.QueryContext(ctx, q, sql.Named("first", first), sql.Named("last", last))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []stockDoc
	for rows.Next() {
		var d stockDoc
		if err := rows.Scan(&d.ID, &d.DocumentID, &d.DocNumber, &d.Status); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// escapeLike escapes s for LIKE … ESCAPE '!'.
func escapeLike(s string) string {
	r := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_", "[", "![")
	return r.Replace(s)
}

// ReceiptFileName is receipt number's file name under namePattern ("" =
// KUPAIN_{n}.doc), as the writer names it.
func ReceiptFileName(namePattern string, number int64) string {
	p := strings.TrimSpace(namePattern)
	if p == "" {
		p = "KUPAIN_{n}.doc"
	}
	return strings.ReplaceAll(p, "{n}", strconv.FormatInt(number, 10))
}

// baseName is a logged path's file name: Windows or slash separators,
// quotes and surrounding spaces ignored.
func baseName(p string) string {
	p = strings.TrimSpace(strings.Trim(strings.TrimSpace(p), `"`))
	if i := strings.LastIndexAny(p, `\/`); i >= 0 {
		p = p[i+1:]
	}
	return p
}

// CheckReceiptImport looks up what Hashavshevet did with receipt number's
// file (fileName, its base name). logDir is the Masofon run-log folder
// (ErrDataPath), "" when unknown. The error, if any, is the raw database
// error behind an unchecked result, for the log only.
func CheckReceiptImport(ctx context.Context, r receiptImportReader, number int64, fileName, logDir string, window int64) (ReceiptImport, error) {
	out := ReceiptImport{ReceiptNumber: number, FileName: fileName, CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	if window <= 0 {
		window = ReceiptImportWindow
	}
	found, err := r.receiptRuns(ctx, fileName, window)
	if err != nil {
		out.Result, out.Detail = ReceiptImportUnchecked, "import log not readable: "+sqlErrorClass(err)
		return out, err
	}
	var runs []receiptRun
	for _, run := range found {
		if strings.EqualFold(baseName(run.FileName), fileName) {
			runs = append(runs, run)
		}
	}
	if len(runs) == 0 {
		out.Result = ReceiptImportNotLogged
		out.Detail = fmt.Sprintf("no import run read %s in the last %d import-log rows: the importer has not run since the file was written, or it reads another folder or name", fileName, window)
		return out, nil
	}
	run := runs[0] // newest first
	out.Runs = len(runs)
	profile, status, first, last, hasRange := parseImportLogRun(run.importLogRun)
	out.ImportLogID, out.File, out.Profile, out.LogStatus = run.ID, run.FileName, profile, status
	at := asLocal(run.ImportDate)
	if !at.IsZero() && at.Year() > 1900 {
		out.ImportedAt = at.Format("2006-01-02T15:04:05")
	}
	if out.Runs > 1 {
		out.Detail = fmt.Sprintf("the file was imported %d times; the newest run is shown", out.Runs)
	}

	switch {
	case logDir == "":
		out.RunLogNote = "no run-log folder is known on this box (hasRunLogDir, or masofon.ini's ErrDataPath beside the importer)"
	case profile == "":
		out.RunLogNote = "the run names no Masofon profile (a manual import): no run log to read"
	default:
		rl, err := FindRunLog(logDir, profile, run.FileName, at.Add(-receiptRunLogLead))
		switch {
		case err != nil:
			out.RunLogNote = "the run log could not be read: " + err.Error()
		case rl == nil:
			out.RunLogNote = "no run log of " + profile + " for this file in " + logDir
		default:
			out.RunLog = rl
		}
	}
	refused := out.RunLog != nil && (out.RunLog.Section == RunLogRefused || out.RunLog.Section == RunLogDuplicate || out.RunLog.Section == RunLogRetry)

	if status != "" && !strings.EqualFold(status, "Success") && !strings.EqualFold(status, "partial") {
		out.Result = ReceiptImportFailed
		return out, nil
	}
	if hasRange {
		docs, err := r.stockRange(ctx, first, last)
		if err != nil {
			out.Result = ReceiptImportUnchecked
			out.Detail = joinDetail(out.Detail, "documents not readable: "+sqlErrorClass(err))
			return out, err
		}
		if len(docs) > 0 {
			out.Result = ReceiptImportImported
			for _, d := range docs {
				out.StockIDs = append(out.StockIDs, d.ID)
			}
			out.DocumentID, out.DocNumber = docs[0].DocumentID, docs[0].DocNumber
			st := docs[0].Status
			out.StockStatus = &st
			return out, nil
		}
		out.Detail = joinDetail(out.Detail, fmt.Sprintf("no document in Stock %d-%d (deleted since?)", first, last))
	}
	if refused {
		out.Result = ReceiptImportFailed
		return out, nil
	}
	out.Result = ReceiptImportNoDocument
	return out, nil
}

// asLocal reads a datetime the driver returned as the server's wall clock
// (SQL Server's datetime has no zone) in this machine's zone: the run-log
// files' times are local.
func asLocal(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.Local)
}
