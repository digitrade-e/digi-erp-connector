package hasavshevet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/atomicfile"
)

// Import confirmation (research/reverse-engineering/hasavshevet-importer.md, P1).
//
// The importer's exit code carries nothing (the Masofon BAT ends with a bare
// EXIT), but Hashavshevet writes one row per import run into the company
// database's dbo.ImportLog: the file it read, the profile, Success / Failed,
// and the range of dbo.Stock IDs the run created. After a model (b) run the
// connector reads, with its own read-only login, the rows logged since it
// started the importer, and looks in that Stock range for the document that
// carries the order number. The outcome is reported, never acted on: an
// order's done / failed meaning and its index entry are unchanged.

// Import check results.
const (
	// OrderImportImported: a run for this order's file ended Success and created
	// a document carrying the order number (DocNumber, or Reference when the
	// document type numbers itself).
	OrderImportImported = "imported"
	// OrderImportLoggedNoDocument: the run ended Success, but no document in its
	// range carries the order number.
	OrderImportLoggedNoDocument = "logged_no_document"
	// OrderImportFailed: a run for this order's file was logged with another
	// status (Failed; partial for a run a user started by hand).
	OrderImportFailed = "failed"
	// OrderImportNotLogged: no run for this order's file was logged. A refused
	// Masofon run is not always logged: its .err log under the profile's
	// ErrDataPath says why.
	OrderImportNotLogged = "not_logged"
	// OrderImportUnchecked: the check could not run (no database, no ImportLog
	// table, a read error).
	OrderImportUnchecked = "unchecked"
)

// ImportCheck is what the connector learned from Hashavshevet's import log
// after running the importer for one order. It is stored beside the order
// index (DataDir/orders/imports/<n>.json), never inside it.
type ImportCheck struct {
	Result string `json:"result"`
	// Detail is a short reason for a result other than imported.
	Detail string `json:"detail,omitempty"`
	// Profile is the Masofon profile (BAT name) the logged run used.
	Profile string `json:"profile,omitempty"`
	// LogStatus is the run's status as logged (Success, Failed, partial).
	LogStatus string `json:"logStatus,omitempty"`
	// StockIDs are the dbo.Stock IDs of the document(s) carrying the order
	// number; DocumentID is the company's own document type (the code sent
	// in IMOVEIN line 4 is translated per company), StockStatus the
	// document's status (0 not issued, 1 issued).
	StockIDs    []int64 `json:"stockIds,omitempty"`
	DocumentID  int64   `json:"documentId,omitempty"`
	StockStatus *int64  `json:"stockStatus,omitempty"`
	// MatchedBy is the column the order number was found in: docNumber, or
	// reference when the document type replaced DocNumber on issue.
	MatchedBy string `json:"matchedBy,omitempty"`
	// RunLog is the Masofon run's own log (P3): <bat>.ok|err.…LOG in the
	// profile's ErrDataPath, with its error lines and summary.
	RunLog *RunLog `json:"runLog,omitempty"`
	// MovedTo is where a refused IMOVEIN.doc was moved (hasMoveRefusedOrders).
	MovedTo string `json:"movedTo,omitempty"`
	// TimedOut: the importer was stopped at hasImportTimeoutSeconds.
	TimedOut  bool   `json:"timedOut,omitempty"`
	CheckedAt string `json:"checkedAt"`
}

// importLogRun is one dbo.ImportLog row.
type importLogRun struct {
	ID        int64
	FileName  string
	Command   string // IsMasofonCommand
	ExtraData string
}

// stockHit is one dbo.Stock row carrying the order number.
type stockHit struct {
	ID         int64
	DocumentID int64
	Status     int64
	MatchedBy  string
}

// importLogReader reads Hashavshevet's import log; a seam for tests.
type importLogReader interface {
	// mark is the highest ImportLog ID now (0 for an empty log).
	mark(ctx context.Context) (int64, error)
	// runsSince returns the rows logged after mark, oldest first.
	runsSince(ctx context.Context, mark int64) ([]importLogRun, error)
	// findStock returns the documents with ID in [first, last] whose
	// DocNumber is number or whose Reference is reference.
	findStock(ctx context.Context, first, last, number, reference int64) ([]stockHit, error)
}

// sqlImportLog reads the company database with the connector's login.
type sqlImportLog struct {
	db     *sql.DB
	dbName string // checked by IsSafeDBName before it is bracket-quoted
}

const (
	maxImportLogRows = 50
	maxStockHits     = 10
)

func (r sqlImportLog) mark(ctx context.Context) (int64, error) {
	var id int64
	q := fmt.Sprintf(`SELECT ISNULL(MAX(ID), 0) FROM [%s].[dbo].[ImportLog]`, r.dbName)
	err := r.db.QueryRowContext(ctx, q).Scan(&id)
	return id, err
}

func (r sqlImportLog) runsSince(ctx context.Context, mark int64) ([]importLogRun, error) {
	q := fmt.Sprintf(`SELECT TOP (%d) ID, ISNULL(FileName, ''), ISNULL(IsMasofonCommand, ''), ISNULL(ExtraData, '')
FROM [%s].[dbo].[ImportLog] WHERE ID > @mark ORDER BY ID`, maxImportLogRows, r.dbName)
	rows, err := r.db.QueryContext(ctx, q, sql.Named("mark", mark))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []importLogRun
	for rows.Next() {
		var run importLogRun
		if err := rows.Scan(&run.ID, &run.FileName, &run.Command, &run.ExtraData); err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func (r sqlImportLog) findStock(ctx context.Context, first, last, number, reference int64) ([]stockHit, error) {
	q := fmt.Sprintf(`SELECT TOP (%d) ID, ISNULL(DocumentID, 0), ISNULL(Status, -1),
  CASE WHEN DocNumber = @number THEN 'docNumber' ELSE 'reference' END
FROM [%s].[dbo].[Stock]
WHERE ID BETWEEN @first AND @last AND (DocNumber = @number OR Reference = @reference)
ORDER BY ID`, maxStockHits, r.dbName)
	rows, err := r.db.QueryContext(ctx, q,
		sql.Named("first", first), sql.Named("last", last),
		sql.Named("number", number), sql.Named("reference", reference))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []stockHit
	for rows.Next() {
		var h stockHit
		if err := rows.Scan(&h.ID, &h.DocumentID, &h.Status, &h.MatchedBy); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

var (
	logStatusRe  = regexp.MustCompile(`&Status=([A-Za-z]+)`)
	stockRangeRe = regexp.MustCompile(`FirstStockID=(\d+)&LastStockID=(\d+)`)
)

// parseImportLogRun reads a row's profile (the BAT name before the first
// '='), its status and its Stock ID range. A manual import's command is "No"
// (no profile).
func parseImportLogRun(run importLogRun) (profile, status string, first, last int64, hasRange bool) {
	// "<bat>=<command>&Company=…" for a Masofon run; "No&Company=…" for a
	// manual one, whose text before the first '=' holds a '&': no profile.
	if i := strings.Index(run.Command, "="); i > 0 && !strings.Contains(run.Command[:i], "&") {
		profile = strings.TrimSpace(run.Command[:i])
	}
	for _, m := range logStatusRe.FindAllStringSubmatch(run.Command, -1) {
		if m[1] != "" {
			status = m[1]
			break
		}
	}
	if m := stockRangeRe.FindStringSubmatch(run.ExtraData); m != nil {
		f, err1 := strconv.ParseInt(m[1], 10, 64)
		l, err2 := strconv.ParseInt(m[2], 10, 64)
		if err1 == nil && err2 == nil && f > 0 && l >= f {
			first, last, hasRange = f, l, true
		}
	}
	return profile, status, first, last, hasRange
}

// sameImportFile reports whether the file a run logged is ours: Windows
// paths, compared case-insensitively, quotes and surrounding spaces ignored.
func sameImportFile(logged, ours string) bool {
	clean := func(p string) string {
		p = strings.TrimSpace(strings.Trim(strings.TrimSpace(p), `"`))
		if p == "" {
			return ""
		}
		return strings.ToLower(filepath.Clean(strings.ReplaceAll(p, "/", `\`)))
	}
	a, b := clean(logged), clean(ours)
	return a != "" && a == b
}

// checkImport decides an ImportCheck from the rows logged after mark.
// ourFile is the file the importer read; number the order number (line 3,
// DocNumber); reference line 8 (asmahta2, else the order number); bat the
// configured BAT, if any, whose name is the expected profile. The error, if
// any, is the raw database error behind an unchecked result, for the log only.
func checkImport(ctx context.Context, r importLogReader, mark int64, ourFile, bat string, number, reference int64) (ImportCheck, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	runs, err := r.runsSince(ctx, mark)
	if err != nil {
		return ImportCheck{Result: OrderImportUnchecked, Detail: "import log not readable: " + sqlErrorClass(err), CheckedAt: now}, err
	}
	var mine *importLogRun
	for i := range runs {
		if sameImportFile(runs[i].FileName, ourFile) {
			mine = &runs[i] // the last run of our file wins
		}
	}
	if mine == nil {
		return ImportCheck{Result: OrderImportNotLogged,
			Detail:    fmt.Sprintf("%d run(s) logged since the importer started, none for this order's file; a refused Masofon run may be unlogged: see the profile's ErrDataPath", len(runs)),
			CheckedAt: now}, nil
	}
	profile, status, first, last, hasRange := parseImportLogRun(*mine)
	check := ImportCheck{Profile: profile, LogStatus: status, CheckedAt: now}
	if b := strings.TrimSpace(bat); b != "" && profile != "" && !strings.EqualFold(profile, filepath.Base(b)) {
		check.Detail = fmt.Sprintf("run logged under profile %q, not this connector's %q: another integration reads this file", profile, filepath.Base(b))
	}
	if !strings.EqualFold(status, "Success") {
		check.Result = OrderImportFailed
		return check, nil
	}
	if !hasRange {
		check.Result = OrderImportLoggedNoDocument
		check.Detail = joinDetail(check.Detail, "the run logged no Stock range")
		return check, nil
	}
	hits, err := r.findStock(ctx, first, last, number, reference)
	if err != nil {
		check.Result = OrderImportUnchecked
		check.Detail = joinDetail(check.Detail, "documents not readable: "+sqlErrorClass(err))
		return check, err
	}
	if len(hits) == 0 {
		check.Result = OrderImportLoggedNoDocument
		check.Detail = joinDetail(check.Detail, fmt.Sprintf("no document in Stock %d-%d carries the order number", first, last))
		return check, nil
	}
	check.Result = OrderImportImported
	for _, h := range hits {
		check.StockIDs = append(check.StockIDs, h.ID)
	}
	check.DocumentID = hits[0].DocumentID
	st := hits[0].Status
	check.StockStatus = &st
	check.MatchedBy = hits[0].MatchedBy
	return check, nil
}

// OrderImportsDir is where import checks are kept: one file per order number,
// beside the order index and never inside its entries — a build that does
// not know them never reads this folder, so a downgrade is unaffected.
func OrderImportsDir(dataDir string) string {
	return filepath.Join(dataDir, "orders", "imports")
}

func importCheckPath(dir string, number int64) string {
	return filepath.Join(dir, strconv.FormatInt(number, 10)+".json")
}

func saveImportCheck(dir string, number int64, c *ImportCheck) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(importCheckPath(dir, number), append(b, '\n'), 0o600)
}

// loadImportCheck reads an order's import check; nil without one (no importer
// ran, a build before checks, or the file cannot be read: a check is a
// report, never needed to answer the order's status).
func loadImportCheck(dir string, number int64) *ImportCheck {
	b, err := os.ReadFile(importCheckPath(dir, number))
	if err != nil {
		return nil
	}
	var c ImportCheck
	if json.Unmarshal(b, &c) != nil || c.Result == "" {
		return nil
	}
	return &c
}

func joinDetail(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// sqlErrorClass names a database error without echoing driver text (it can
// carry server and object names): never return raw driver errors.
func sqlErrorClass(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, sql.ErrNoRows):
		return "no rows"
	default:
		return "query failed"
	}
}
