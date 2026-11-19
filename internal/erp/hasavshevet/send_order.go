package hasavshevet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/logger"
)

// ErrOrderNotWritten marks a processing error raised before the first order
// file was opened for writing: nothing of the order is on disk and no importer
// ran, so it may be tried again under the same number (the key index's
// retryable state). Any error after that point may leave a file behind and is
// final for its key. errors.Is finds it; the error's text is unchanged.
var ErrOrderNotWritten = errors.New("order not written")

type orderNotWrittenError struct{ err error }

func (e *orderNotWrittenError) Error() string        { return e.err.Error() }
func (e *orderNotWrittenError) Unwrap() error        { return e.err }
func (e *orderNotWrittenError) Is(target error) bool { return target == ErrOrderNotWritten }

// notWritten marks err as raised before any order file was written.
func notWritten(err error) error { return &orderNotWrittenError{err: err} }

// OrderRequest is the internal representation of a send-order request,
// translated from the API DTO before enqueueing.
// DBName is not part of the request; the Sender resolves it from config.
type OrderRequest struct {
	DocumentType  string
	UserExtID     string
	DueDate       string
	CreatedDate   string
	Comment       string
	Discount      float64
	HistoryID     string
	Total         float64
	Currency      string
	CustomerEmail string // optional; used for PDF email delivery
	Details       []OrderLineItem
	// Account optionally carries the customer details normally read from
	// [dbo].[Accounts]. When set it is used as-is and no lookup happens, which
	// is what lets a connector with no database build orders.
	Account *AccountDetails

	// Document fields (DIG-1295, document_fields.go): the tenant's
	// erp-manager config for the IMOVEIN header. Zero / "" = today's value.
	// omitempty keeps a request without them hashing as it did before them
	// (orderRequestHash, the DIG-1291 index).
	DocumentID int    `json:",omitempty"` // line4
	Warehouse  int    `json:",omitempty"` // line12
	VatPercent string `json:",omitempty"` // line18 text, e.g. "17.00"
	Asmahta2   string `json:",omitempty"` // line8, digits; MyID (line3) stays the order number

	// Header (DIG-1315, header_fields.go): the order's own header details,
	// mapped by the tenant's erp-manager config. nil = none, today's bytes;
	// omitempty keeps a request without it hashing as before.
	Header *OrderHeader `json:",omitempty"`

	// Rule (plan 087, POST /api/v2/sendOrder): the documentRule — which
	// counter numbers the order and which typed folder it goes to. nil =
	// today's counter and destination; omitempty keeps a request without it
	// hashing as before.
	Rule *DocumentRule `json:",omitempty"`
}

// AccountDetails is the caller-supplied form of the account record: the columns
// the connector would otherwise read from [dbo].[Accounts].
type AccountDetails struct {
	AccountKey string
	FullName   string
	Address    string
	City       string
	Phone      string
	Agent      string
	HProtect   string
}

// OrderLineItem is one line item in an order.
type OrderLineItem struct {
	Title         string
	SKU           string
	Quantity      float64
	Packs         float64 // number of packages → IMOVEIN line33 (אריזות)
	OriginalPrice float64
	SinglePrice   float64
	TotalPrice    float64
	Discount      float64
}

// OrderResult is returned by ProcessOrder on success.
type OrderResult struct {
	OrderNumber  int64
	WrittenFiles []string
	Account      AccountInfo
	// Import is what Hashavshevet's import log says about this order's run
	// (import_check.go); nil when no importer ran (model (a), typed folders,
	// no importer configured).
	Import *ImportCheck
}

// AccountInfo holds customer data needed for PDF generation.
// Exported mirror of the internal accountInfo queried from the DB.
type AccountInfo struct {
	AccountKey string
	FullName   string
	Address    string
	City       string
	Phone      string
}

// accountInfo holds the DB columns needed from the Accounts table.
type accountInfo struct {
	AccountKey string
	FullName   string
	Address    string
	City       string
	Phone      string
	Agent      string
	HProtect   string
}

// Sender orchestrates the Hasavshevet send-order pipeline:
// account lookup → currency rate → file generation → has.exe execution.
//
// ProcessOrder must only be called from a single goroutine at a time
// (guaranteed by OrderQueue's single-worker model) because IMOVEIN.doc/.prm
// are shared files in SendOrderDir that cannot be written concurrently.
type Sender struct {
	db          *sql.DB
	cfg         config.Config
	numberStore *OrderNumberStore
	log         logger.LoggerService

	// Test seams for the importers; nil = runBatFile / runImporter.
	runBat    func(ctx context.Context, batPath string) (int, string, error)
	runHasExe func(ctx context.Context, hasExePath, paramFile, workDir string) (int, string, error)
	// importerCheck, when set, is asked before a model (b) order's files are
	// written: the importer must not be one others can change (plan 087
	// review T1; the daemon's guard). A refusal leaves the order retryable.
	importerCheck func() error
	// importLog is a test seam for Hashavshevet's import log; nil = read the
	// configured database (importLogReader()).
	importLog importLogReader
	// precheck is a test seam for the order pre-check (precheck.go); nil =
	// the configured database.
	precheck precheckReader
}

// importLogReader is the import log the confirmation reads: the test seam,
// else the configured database; nil when there is none (no connection, no
// or an unsafe database name).
func (s *Sender) importLogReader() importLogReader {
	if s.importLog != nil {
		return s.importLog
	}
	dbName := strings.TrimSpace(s.cfg.DB.Database)
	if s.db == nil || dbName == "" || !IsSafeDBName(dbName) {
		return nil
	}
	return sqlImportLog{db: s.db, dbName: dbName}
}

// importLogTimeout bounds each read of the import log: the confirmation must
// never hold the worker (and the import lock) for long.
const importLogTimeout = 15 * time.Second

// NewSender creates a Sender. db and numberStore must be non-nil.
func NewSender(db *sql.DB, cfg config.Config, numberStore *OrderNumberStore, log logger.LoggerService) *Sender {
	return &Sender{db: db, cfg: cfg, numberStore: numberStore, log: log}
}

// ProcessOrder executes the full send-order pipeline for one order.
// It is NOT safe for concurrent IMOVEIN file access; always call via OrderQueue.
func (s *Sender) ProcessOrder(ctx context.Context, req OrderRequest) (*OrderResult, error) {
	orderNum, err := s.nextOrderNumber()
	if err != nil {
		return nil, err
	}
	return s.processOrderWithNumber(ctx, req, orderNum)
}

// nextOrderNumber increments and persists the order sequence.
func (s *Sender) nextOrderNumber() (int64, error) {
	if s.numberStore == nil {
		return 0, errors.New("order number store is not configured")
	}
	orderNum, err := s.numberStore.Next()
	if err != nil {
		return 0, fmt.Errorf("get order number: %w", err)
	}
	return orderNum, nil
}

// processOrderWithNumber executes the full send-order flow using a pre-reserved
// order number. If orderNum is zero, it reserves one before continuing.
//
// Every error until the first order file is opened for writing is marked
// ErrOrderNotWritten (see there); from then on a file may exist.
func (s *Sender) processOrderWithNumber(ctx context.Context, req OrderRequest, orderNum int64) (*OrderResult, error) {
	return s.processOrder(ctx, req, orderNum, nil)
}

// processOrder is processOrderWithNumber with the destination of an order
// with a documentRule (plan 087): its typed folder by key — path and layout
// as configured now — and the file name frozen at reservation. nil is
// today's destination.
func (s *Sender) processOrder(ctx context.Context, req OrderRequest, orderNum int64, dest *orderDestination) (*OrderResult, error) {
	if strings.TrimSpace(s.cfg.SendOrderDir) == "" {
		return nil, notWritten(errors.New("sendOrderDir is not configured"))
	}
	var folder *config.FolderConfig
	if dest != nil {
		f, ok := s.cfg.Documents.Folders[dest.Folder]
		if !ok || f.Kind != config.DocumentKindOrders {
			return nil, notWritten(fmt.Errorf("order %d goes to folder %q, which is no longer an orders folder on this connector (documents.folders)", orderNum, dest.Folder))
		}
		// Review R2: never a test order into a folder that is production now,
		// nor the reverse; retryable until the flag is back.
		if why := testFlagChanged(f, dest.CounterTest, dest.FolderTest); why != "" {
			return nil, notWritten(fmt.Errorf("%w: order %d into folder %s: %s", ErrFolderTestChanged, orderNum, dest.Folder, why))
		}
		folder = &f
	}

	// Review T1: the importer a model (b) order runs is read first — before a
	// number is taken or any file written, so a refusal leaves the order
	// retryable.
	if folder == nil && s.importerCheck != nil && s.cfg.OrderImporter() != "" {
		if err := s.importerCheck(); err != nil {
			s.log.Warn(fmt.Sprintf("order not written (number %d kept): %v", orderNum, err))
			return nil, notWritten(err)
		}
	}

	// 1. Pre-flight validation (business rules + Hasavshevet mandatory field
	// spec + what the destination's layout can hold)
	if err := validateOrderRequest(req); err != nil {
		return nil, notWritten(err)
	}
	if err := validateLayout(s.layoutFor(folder), req); err != nil {
		return nil, notWritten(err)
	}

	// 2. Concurrency-safe order number (reserved at queue submit when possible).
	if orderNum == 0 {
		var err error
		orderNum, err = s.nextOrderNumber()
		if err != nil {
			return nil, notWritten(err)
		}
	}

	dbName := strings.TrimSpace(s.cfg.DB.Database)
	s.log.Info(fmt.Sprintf("processing order orderNumber=%d historyId=%s userExtId=%s dbName=%q",
		orderNum, req.HistoryID, req.UserExtID, dbName))

	// 3. Account details: supplied by the caller, or looked up in the ERP database
	account, err := s.resolveAccount(ctx, req)
	if err != nil {
		return nil, notWritten(err)
	}

	// 4. Currency rate (non-fatal; default 1.0 matches legacy behaviour)
	rate := 1.0
	if s.db != nil && dbName != "" {
		if r, err := s.queryRate(ctx, dbName, req.Currency); err != nil {
			s.log.Warn(fmt.Sprintf("rate lookup failed currency=%s: %v; using 1.0", req.Currency, err))
		} else {
			rate = r
		}
	}

	// 5. Build DOC and PRM content, in the destination's layout
	hdr, moves := buildIMOVEIN(orderNum, account, req, rate)
	docBytes, prmBytes, err := render(s.layoutFor(folder), hdr, moves)
	if err != nil {
		return nil, notWritten(fmt.Errorf("generate DOC: %w", err))
	}

	// A typed folder (plan 087): one file under the frozen name, for the
	// ERP's importer reading that folder; no .prm there, no importer run.
	if folder != nil {
		writtenFiles, err := s.writeToFolder(folder, dest.FileName, orderNum, docBytes, prmBytes)
		if err != nil {
			return nil, err
		}
		s.log.Success(fmt.Sprintf("order complete orderNumber=%d folder=%s files=%v (left for the folder's importer; no importer run)", orderNum, dest.Folder, writtenFiles))
		return orderResult(orderNum, writtenFiles, account), nil
	}

	// Model (a): one file for the ERP's scheduler, no importer.
	if s.cfg.OrdersPickup() {
		writtenFiles, err := s.writePickup(req, orderNum, docBytes, prmBytes)
		if err != nil {
			return nil, err
		}
		s.log.Success(fmt.Sprintf("order complete orderNumber=%d files=%v (model a: left for the scheduler; no importer run)", orderNum, writtenFiles))
		return orderResult(orderNum, writtenFiles, account), nil
	}

	// 6. Ensure output directories exist
	dir := s.cfg.SendOrderDir
	historyDir := filepath.Join(dir, "history", fmt.Sprintf("%d", orderNum))
	if err := os.MkdirAll(historyDir, 0o755); err != nil {
		return nil, notWritten(fmt.Errorf("create history dir: %w", err))
	}

	// 7. Write files
	// Active import files (read by has.exe): single-worker queue prevents collision.
	// History copies: permanent audit trail per order.
	toWrite := []struct {
		path string
		data []byte
	}{
		{filepath.Join(dir, "IMOVEIN.doc"), docBytes},
		{filepath.Join(dir, "IMOVEIN.prm"), prmBytes},
		{filepath.Join(historyDir, fmt.Sprintf("IMOVEIN_%d.doc", orderNum)), docBytes},
		{filepath.Join(historyDir, fmt.Sprintf("IMOVEIN_%d.prm", orderNum)), prmBytes},
	}

	var writtenFiles []string
	for _, f := range toWrite {
		opened, err := writeOrderFile(f.path, f.data)
		if err != nil {
			err = fmt.Errorf("write %s: %w", f.path, err)
			if !opened && len(writtenFiles) == 0 {
				// IMOVEIN.doc could not even be opened (e.g. a sharing
				// violation while the importer holds it): nothing is written.
				return nil, notWritten(err)
			}
			return nil, err
		}
		writtenFiles = append(writtenFiles, f.path)
	}

	// 8. Execute Hasavshevet importer (Windows only; no-op on other platforms).
	// HasBatFile (Masofon-generated BAT launcher) takes precedence over HasExePath.
	// The single-worker queue guarantees the previous import is finished before
	// the next order's files are written and the importer is invoked again.
	//
	// The exit code carries nothing (Masofon's BAT ends with a bare EXIT): the
	// run is confirmed from Hashavshevet's import log instead (import_check.go),
	// reported on the result and never changing the order's outcome.
	// hasImportTimeoutSeconds, when set, caps the run and kills its process
	// tree when it passes.
	bat, hasExe := strings.TrimSpace(s.cfg.HasBatFile), strings.TrimSpace(s.cfg.HasExePath)
	var check *ImportCheck
	if bat != "" || hasExe != "" {
		logReader := s.importLogReader()
		var mark int64
		markErr := errors.New("no database to read the import log from")
		if logReader != nil {
			mctx, cancel := context.WithTimeout(ctx, importLogTimeout)
			mark, markErr = logReader.mark(mctx)
			cancel()
			if markErr != nil {
				s.log.Warn(fmt.Sprintf("import log not readable before the import orderNumber=%d: %v; the import will not be confirmed", orderNum, markErr))
			}
		}

		runCtx, cancelRun := ctx, context.CancelFunc(func() {})
		if t := s.cfg.OrderImportTimeout(); t > 0 {
			runCtx, cancelRun = context.WithTimeout(ctx, t)
		}
		start := time.Now()
		if bat != "" {
			exitCode, output, execErr := s.batRunner()(runCtx, bat)
			s.log.Info(fmt.Sprintf("digi.bat exit=%d durationMs=%d output=%q orderNumber=%d",
				exitCode, time.Since(start).Milliseconds(), output, orderNum))
			if execErr != nil || exitCode != 0 {
				s.log.Error(fmt.Sprintf("digi.bat failed orderNumber=%d exit=%d", orderNum, exitCode), execErr)
			}
		} else {
			exitCode, output, execErr := s.hasExeRunner()(runCtx, hasExe, s.cfg.HasParamFile, dir)
			s.log.Info(fmt.Sprintf("has.exe exit=%d durationMs=%d output=%q orderNumber=%d",
				exitCode, time.Since(start).Milliseconds(), output, orderNum))
			if execErr != nil {
				// Log but do not fail: files are written; import can be retried.
				s.log.Error(fmt.Sprintf("has.exe failed orderNumber=%d", orderNum), execErr)
			}
		}
		timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
		cancelRun()
		if timedOut {
			s.log.Error(fmt.Sprintf("order importer stopped at hasImportTimeoutSeconds=%d orderNumber=%d: its process tree was killed; confirm the order in Hashavshevet before resending it",
				s.cfg.HasImportTimeoutSeconds, orderNum), nil)
		}

		ourFile := filepath.Join(dir, "IMOVEIN.doc")
		check = s.confirmImport(ctx, logReader, mark, markErr, ourFile, bat, orderNum, req.Asmahta2)
		check.TimedOut = timedOut
		if bat != "" {
			if timedOut {
				s.clearStoppedRunMarker(check, bat, start, orderNum)
			}
			s.attachRunLog(check, bat, ourFile, start, orderNum)
		}
		check.Outcome = importOutcome(check)
		s.log.Info(fmt.Sprintf("import check orderNumber=%d outcome=%s result=%s logStatus=%q profile=%q stockIds=%v matchedBy=%q detail=%q",
			orderNum, check.Outcome, check.Result, check.LogStatus, check.Profile, check.StockIDs, check.MatchedBy, check.Detail))
		// Only a real refusal of the order's own file is moved aside: never
		// one that passes (retry), a duplicate (already imported) or an
		// outcome nothing confirms (P14).
		if s.cfg.HasMoveRefusedOrders && check.Outcome == OrderOutcomeRefused {
			s.moveRefused(check, dir, orderNum)
		}
	}

	s.log.Success(fmt.Sprintf("order complete orderNumber=%d files=%v", orderNum, writtenFiles))
	res := orderResult(orderNum, writtenFiles, account)
	res.Import = check
	return res, nil
}

// attachRunLog adds what the Masofon run logged about the order's own file
// (P3, P9–P12): the section for ourFile in the run that read it, in
// hasRunLogDir or masofon.ini's ErrDataPath, plus the system-error lines
// DataImpExpLogs got during the run. It lets the log decide what ImportLog
// could not: a refusal is almost never in ImportLog.
func (s *Sender) attachRunLog(check *ImportCheck, bat, ourFile string, start time.Time, orderNum int64) {
	dirs := ResolveRunLogDirs(s.cfg)
	if dirs.LogDir == "" {
		why := "masofon.ini has no ErrDataPath"
		if dirs.INIErr != nil {
			why = fmt.Sprintf("%s not readable (%v)", dirs.INI, dirs.INIErr)
		}
		s.log.Warn(fmt.Sprintf("import check orderNumber=%d: %s and no hasRunLogDir is set; no run log", orderNum, why))
	}
	rl, err := FindRunLog(dirs.LogDir, bat, ourFile, start)
	if err != nil {
		s.log.Warn(fmt.Sprintf("import check orderNumber=%d: run log in %q not readable: %v", orderNum, dirs.LogDir, err))
		rl = nil
	}
	if check.Result != OrderImportImported && (rl == nil || rl.Section != RunLogImported) {
		lines, err := SystemErrorLines(dirs.HasDir, bat, start, time.Now())
		if err != nil {
			s.log.Warn(fmt.Sprintf("import check orderNumber=%d: importer error log not readable: %v", orderNum, err))
		}
		rl = attachSystemErrors(rl, lines)
	}
	if rl == nil {
		return
	}
	check.RunLog = rl
	if len(rl.Errors) > 0 || len(rl.SystemErrors) > 0 {
		s.log.Warn(fmt.Sprintf("import run log orderNumber=%d %s section=%q: %s", orderNum, rl.File, rl.Section,
			strings.Join(append(append([]string{}, rl.Errors...), rl.SystemErrors...), " || ")))
	}
	refusedSection := rl.Section == RunLogRefused || rl.Section == RunLogDuplicate || rl.Section == RunLogRetry
	switch {
	case check.Result == OrderImportImported || check.Result == OrderImportFailed || check.Result == OrderImportPartial:
		// ImportLog already decided; the log is detail.
	case rl.Section == RunLogPartial:
		check.Result = OrderImportPartial
		check.Detail = joinDetail(check.Detail, "the Masofon run imported this order's file partially: the refused lines (runLog.refusals) were dropped")
	case rl.Section == RunLogNoInput || (rl.Section == "" && rl.NoInput):
		check.Result = OrderImportNotLogged
		check.Detail = joinDetail(check.Detail, "the Masofon run found no input file: the profile's data file is not this connector's file (discover: masofon_data_file_mismatch)")
	case refusedSection || (rl.Section == "" && rl.File != "" && !rl.OK && len(rl.Errors) > 0):
		check.Result = OrderImportFailed
		check.Detail = joinDetail(check.Detail, "the Masofon run refused this order's file: see runLog")
	case rl.Section == RunLogImported:
		check.Detail = joinDetail(check.Detail, "the Masofon run log shows this order's file imported")
	}
	if rl.Live {
		check.Detail = joinDetail(check.Detail, "the run was stopped before it finished its log: runLog is the unfinished "+filepath.Base(rl.File))
	}
	if rl.Transient && check.Result != OrderImportImported {
		check.Detail = joinDetail(check.Detail, "refused for a passing reason (another station issuing, a second run, a deadlock): this run did not import it")
	}
}

// clearStoppedRunMarker removes the <bat>.IsWorking a run killed at
// hasImportTimeoutSeconds leaves in the BAT's folder (FAST_IMP.HDS creates
// it on start and deletes it on exit): left there, the profile's next run —
// the next order's — would be refused as a "double click". Only a marker
// created since this run started is removed: an older one is not ours.
func (s *Sender) clearStoppedRunMarker(check *ImportCheck, bat string, start time.Time, orderNum int64) {
	marker := filepath.Join(filepath.Dir(bat), strings.ReplaceAll(filepath.Base(bat), " ", "_")+".IsWorking")
	st, err := os.Stat(marker)
	if err != nil {
		return
	}
	if fileCreatedAt(st).Before(start.Add(-runLogSlack)) {
		s.log.Warn(fmt.Sprintf("order %d: %s is older than this run; left in place", orderNum, marker))
		return
	}
	if err := os.Remove(marker); err != nil {
		s.log.Warn(fmt.Sprintf("order %d: the stopped run's %s could not be removed: %v; the profile's next run will be refused once", orderNum, marker, err))
		return
	}
	check.Detail = joinDetail(check.Detail, "the stopped run's "+filepath.Base(marker)+" was removed so the next run is not refused")
	s.log.Warn(fmt.Sprintf("order %d: removed the stopped run's %s", orderNum, marker))
}

// moveRefused moves a refused IMOVEIN.doc out of sendOrderDir into
// sendOrderDir\refused\IMOVEIN_<n>.doc (hasMoveRefusedOrders): left there, a
// later run of the profile would import it again behind the connector's
// back. Only a file still in place is moved (Masofon renames an imported one
// to ….MASOFON2.BAK); the history copy stays where it is.
func (s *Sender) moveRefused(check *ImportCheck, dir string, orderNum int64) {
	src := filepath.Join(dir, "IMOVEIN.doc")
	if _, err := os.Stat(src); err != nil {
		return
	}
	refused := filepath.Join(dir, "refused")
	if err := os.MkdirAll(refused, 0o755); err != nil {
		s.log.Warn(fmt.Sprintf("order %d refused by the importer; not moved aside: %v", orderNum, err))
		return
	}
	dst := filepath.Join(refused, fmt.Sprintf("IMOVEIN_%d.doc", orderNum))
	if _, err := os.Stat(dst); err == nil {
		s.log.Warn(fmt.Sprintf("order %d refused by the importer; not moved aside: %s already exists", orderNum, dst))
		return
	}
	if err := os.Rename(src, dst); err != nil {
		s.log.Warn(fmt.Sprintf("order %d refused by the importer; not moved aside: %v", orderNum, err))
		return
	}
	check.MovedTo = dst
	s.log.Warn(fmt.Sprintf("order %d refused by the importer: %s moved to %s so no later run imports it", orderNum, src, dst))
}

// headerDocumentCode is the code written in IMOVEIN line 4: the request's
// documentId (DIG-1295), else today's rule — ORDER 30 for the literal shekel
// currency, else 32; QUOATE 40; RETURN 74. Hashavshevet reads it as a
// Hashavshevet-6 code and translates it per company (DocumentsDefV6).
func headerDocumentCode(req OrderRequest) int {
	if req.DocumentID != 0 {
		return req.DocumentID
	}
	switch req.DocumentType {
	case "ORDER":
		if req.Currency == `ש"ח` {
			return 30
		}
		return 32
	case "QUOATE":
		return 40
	case "RETURN":
		return 74
	}
	return 0
}

// confirmImport reads the import log rows written since mark (taken before the
// importer started) and looks for the order's document. markErr != nil means
// the log could not be read before the run: the check is unchecked.
func (s *Sender) confirmImport(ctx context.Context, r importLogReader, mark int64, markErr error, ourFile, bat string, orderNum int64, asmahta2 string) *ImportCheck {
	if r == nil || markErr != nil {
		detail := "no database to read the import log from"
		if r != nil {
			detail = "import log not readable before the import: " + sqlErrorClass(markErr)
		}
		return &ImportCheck{Result: OrderImportUnchecked, Detail: detail, CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	}
	reference := orderNum // line 8 is the order number unless asmahta2 is sent
	if a := strings.TrimSpace(asmahta2); a != "" {
		if n, err := strconv.ParseInt(a, 10, 64); err == nil {
			reference = n
		}
	}
	cctx, cancel := context.WithTimeout(ctx, importLogTimeout)
	defer cancel()
	check, err := checkImport(cctx, r, mark, ourFile, bat, orderNum, reference)
	if err != nil {
		s.log.Warn(fmt.Sprintf("import check orderNumber=%d: %v", orderNum, err))
	}
	return &check
}

func orderResult(orderNum int64, writtenFiles []string, account accountInfo) *OrderResult {
	return &OrderResult{
		OrderNumber:  orderNum,
		WrittenFiles: writtenFiles,
		Account: AccountInfo{
			AccountKey: account.AccountKey,
			FullName:   account.FullName,
			Address:    account.Address,
			City:       account.City,
			Phone:      account.Phone,
		},
	}
}

func (s *Sender) batRunner() func(context.Context, string) (int, string, error) {
	if s.runBat != nil {
		return s.runBat
	}
	if s.cfg.OrderImportTimeout() > 0 {
		return runBatFileTree // a timeout must kill has.exe too
	}
	return runBatFile
}

func (s *Sender) hasExeRunner() func(context.Context, string, string, string) (int, string, error) {
	if s.runHasExe != nil {
		return s.runHasExe
	}
	if s.cfg.OrderImportTimeout() > 0 {
		return runImporterTree
	}
	return runImporter
}

// layoutFor is the IMOVEIN layout of a destination: a typed folder's own, or
// the configured sendOrderFormat.
func (s *Sender) layoutFor(folder *config.FolderConfig) string {
	if folder != nil {
		return folder.Layout
	}
	return s.cfg.OrderFormat()
}

// render builds the order's DOC and the PRM that describes it, in layout.
func render(layout string, hdr stockHeader, moves []stockMove) (doc, prm []byte, err error) {
	if layout == config.OrderFormatLegacy {
		return generateLegacyDOC(hdr, moves), generateLegacyPRM(), nil
	}
	doc, err = generateDOC(hdr, moves)
	if err != nil {
		return nil, nil, err
	}
	return doc, generatePRM(), nil
}

// ValidateLayout refuses a request the configured layout cannot hold as
// written (ValidateLegacyFit); the official layout adds no check here. The
// route calls it before a number is reserved, the writer again for every
// other caller. It returns a *FieldError or nil.
//
// An order with a documentRule is checked against its folder's layout (plan
// 087); a folder the config does not have is the route's to refuse.
func (s *Sender) ValidateLayout(req OrderRequest) error {
	var folder *config.FolderConfig
	if req.Rule != nil {
		if f, ok := s.cfg.Documents.Folders[req.Rule.Folder]; ok {
			folder = &f
		}
	}
	return validateLayout(s.layoutFor(folder), req)
}

func validateLayout(layout string, req OrderRequest) error {
	if layout == config.OrderFormatLegacy {
		return ValidateLegacyFit(req)
	}
	return nil
}

// writeOrderFile is os.WriteFile that says whether the file was opened: once
// it is (and truncated), a failed write may leave part of the order in it.
func writeOrderFile(path string, data []byte) (opened bool, err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return false, err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return true, err
}

// buildIMOVEIN maps a validated OrderRequest to the stockHeader + []stockMove
// needed by generateDOC.
func buildIMOVEIN(orderNum int64, account accountInfo, req OrderRequest, rate float64) (stockHeader, []stockMove) {
	now := time.Now()
	createdDate := parseTimeOrNow(req.CreatedDate, now)
	dueDate := parseTimeOrNow(req.DueDate, now)

	shortDate := fmt.Sprintf("%02d/%02d/%04d",
		createdDate.Day(), int(createdDate.Month()), createdDate.Year())

	discount := req.Discount
	if discount >= 100 {
		discount = 99.99
	}

	// Document type → Hasavshevet header DocumentID code (headerDocumentCode).
	headerDocID, wareHouse := headerDocumentCode(req), 0
	switch req.DocumentType {
	case "ORDER", "QUOATE", "RETURN":
		wareHouse = defaultWarehouse
	}
	if req.Warehouse != 0 {
		wareHouse = req.Warehouse
	}
	asmahta2 := fmt.Sprintf("%d", orderNum)
	if req.Asmahta2 != "" {
		asmahta2 = req.Asmahta2
	}
	vat := defaultVatPercent
	if req.VatPercent != "" {
		vat = req.VatPercent
	}

	hdr := stockHeader{
		AccountKey:   account.AccountKey,
		MyID:         orderNum,
		DocumentID:   headerDocID,
		AccountName:  sanitizeField(account.FullName),
		Address:      sanitizeField(account.Address),
		City:         sanitizeField(account.City),
		Phone:        sanitizeField(account.Phone),
		Asmahta2:     asmahta2,
		ShortDate:    shortDate,
		Agent:        account.Agent,
		WareHouse:    wareHouse,
		DiscountPrcR: fmt.Sprintf("%.2f", discount),
		VatPrc:       vat,
		Copies:       "1",
		Currency:     req.Currency,
		Rate:         fmt.Sprintf("%.4f", rate),
		Remarks:      sanitizeComment(req.Comment, 250),
		HProtect:     account.HProtect,
	}
	// The request's header fields (DIG-1315) win over the account's.
	applyHeader(&hdr, req.Header)

	_ = dueDate // available for future per-line DueDate fields

	moves := make([]stockMove, 0, len(req.Details))
	for _, d := range req.Details {
		// Packs (line33/אריזות): only emit when known (>0) so legacy callers that
		// omit packs keep writing a blank field, identical to prior behaviour.
		packs := ""
		if d.Packs > 0 {
			packs = fmt.Sprintf("%.2f", d.Packs)
		}
		moves = append(moves, stockMove{
			ItemKey:     d.SKU,
			ItemName:    sanitizeField(d.Title),
			Quantity:    fmt.Sprintf("%.2f", d.Quantity),
			Packs:       packs,
			Price:       fmt.Sprintf("%.2f", d.OriginalPrice),
			DiscountPrc: fmt.Sprintf("%.2f", d.Discount),
			Unit:        "יח'",
		})
	}

	return hdr, moves
}

// queryAccount fetches account columns required for the DOC header.
// resolveAccount returns the customer details for the order.
//
// Details supplied with the request win and skip the database entirely — that is
// what allows a connector with no database (an order-writing node sitting beside
// the ERP importer) to build IMOVEIN files at all. Otherwise they are read from
// [dbo].[Accounts] as before.
func (s *Sender) resolveAccount(ctx context.Context, req OrderRequest) (accountInfo, error) {
	if a := req.Account; a != nil {
		account := accountInfo{
			AccountKey: strings.TrimSpace(a.AccountKey),
			FullName:   a.FullName,
			Address:    a.Address,
			City:       a.City,
			Phone:      a.Phone,
			Agent:      a.Agent,
			HProtect:   a.HProtect,
		}
		// The account key identifies the customer in the IMOVEIN record; fall
		// back to the order's userExtId, which is the same identifier.
		if account.AccountKey == "" {
			account.AccountKey = strings.TrimSpace(req.UserExtID)
		}
		if account.AccountKey == "" {
			return accountInfo{}, errors.New("account.accountKey or userExtId is required")
		}
		return account, nil
	}

	if s.db == nil {
		return accountInfo{}, errors.New(
			"no database is configured and the order carries no \"account\" details: " +
				"either send account details with the order, or configure db.host/db.user/db.port/db.database")
	}

	dbName := strings.TrimSpace(s.cfg.DB.Database)
	if dbName == "" {
		return accountInfo{}, errors.New("db.database is not set, so the customer account cannot be looked up")
	}

	account, err := s.queryAccount(ctx, dbName, req.UserExtID)
	if err != nil {
		return accountInfo{}, fmt.Errorf("query account %q: %w", req.UserExtID, err)
	}
	return account, nil
}

func (s *Sender) queryAccount(ctx context.Context, dbName, userExtID string) (accountInfo, error) {
	if !IsSafeDBName(dbName) {
		return accountInfo{}, fmt.Errorf("invalid dbName %q", dbName)
	}
	query := fmt.Sprintf(
		`SELECT TOP 1 AccountKey, FullName, Address, City, Phone, Agent, HProtect`+
			` FROM [%s].[dbo].[Accounts] WHERE AccountKey = @userExtId`,
		dbName,
	)
	row := s.db.QueryRowContext(ctx, query, sql.Named("userExtId", userExtID))

	var a accountInfo
	var fullName, address, city, phone, agent, hprotect sql.NullString
	if err := row.Scan(&a.AccountKey, &fullName, &address, &city, &phone, &agent, &hprotect); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return accountInfo{}, fmt.Errorf("account not found: %q", userExtID)
		}
		return accountInfo{}, err
	}
	a.FullName = fullName.String
	a.Address = address.String
	a.City = city.String
	a.Phone = phone.String
	a.Agent = agent.String
	a.HProtect = hprotect.String
	return a, nil
}

// queryRate fetches the latest exchange rate for currencyCode.
// Returns 1.0 when no rate is found (matches legacy fallback).
func (s *Sender) queryRate(ctx context.Context, dbName, currencyCode string) (float64, error) {
	if !IsSafeDBName(dbName) {
		return 1.0, fmt.Errorf("invalid dbName %q", dbName)
	}
	query := fmt.Sprintf(
		`SELECT TOP 1 Rate FROM [%s].[dbo].[Rates]`+
			` WHERE CurrencyCode = @currencyCode ORDER BY DatF DESC`,
		dbName,
	)
	row := s.db.QueryRowContext(ctx, query, sql.Named("currencyCode", currencyCode))
	var rate sql.NullFloat64
	if err := row.Scan(&rate); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 1.0, nil
		}
		return 1.0, err
	}
	if !rate.Valid || rate.Float64 == 0 {
		return 1.0, nil
	}
	return rate.Float64, nil
}

// validateOrderRequest enforces required fields per the legacy controller and
// Hasavshevet mandatory field spec (line4, line8/asmachta2, line22/SKU, line23/qty≠0).
func validateOrderRequest(req OrderRequest) error {
	var missing []string
	if req.DocumentType == "" {
		missing = append(missing, "documentType")
	}
	if req.UserExtID == "" {
		missing = append(missing, "userExtId")
	}
	if req.DueDate == "" {
		missing = append(missing, "dueDate")
	}
	if req.CreatedDate == "" {
		missing = append(missing, "createdDate")
	}
	if req.HistoryID == "" {
		missing = append(missing, "historyId")
	}
	if len(req.Details) == 0 {
		missing = append(missing, "details (must be non-empty array)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required fields: %s", strings.Join(missing, ", "))
	}

	switch req.DocumentType {
	case "ORDER", "QUOATE", "RETURN":
	default:
		return fmt.Errorf("invalid documentType %q; allowed: ORDER, QUOATE, RETURN", req.DocumentType)
	}

	if err := ValidateDocumentFields(req); err != nil {
		return err
	}
	if err := ValidateHeader(req.Header); err != nil {
		return err
	}

	for i, d := range req.Details {
		if d.SKU == "" {
			return fmt.Errorf("details[%d]: sku is required (line22)", i)
		}
		if d.Quantity == 0 {
			return fmt.Errorf("details[%d]: quantity cannot be zero (line23 Hasavshevet spec)", i)
		}
		if d.Title == "" {
			return fmt.Errorf("details[%d]: title is required", i)
		}
	}
	return nil
}

// IsSafeDBName reports whether a database name is safe to bracket-quote into SQL: true when name contains only alphanumeric and underscore chars.
// Mirrors the legacy Node ensureDbNameSafe pattern to prevent SQL injection via dbName.
func IsSafeDBName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	return true
}

// parseTimeOrNow parses an ISO-8601 date string; returns fallback on any parse failure.
func parseTimeOrNow(s string, fallback time.Time) time.Time {
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return fallback
}

// sanitizeField removes single quotes and converts newlines to ". "
// matching the legacy Node sanitisation applied to free-text DB fields.
func sanitizeField(s string) string {
	s = strings.ReplaceAll(s, "'", "")
	s = strings.ReplaceAll(s, "\n", ". ")
	return s
}

// sanitizeComment applies sanitizeField and truncates to maxLen runes.
func sanitizeComment(s string, maxLen int) string {
	s = sanitizeField(s)
	runes := []rune(s)
	if len(runes) > maxLen {
		return string(runes[:maxLen])
	}
	return s
}
