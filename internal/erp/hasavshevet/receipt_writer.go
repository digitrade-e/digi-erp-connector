package hasavshevet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/logger"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/atomicfile"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/filelock"
)

// KUPAINPRMName is the parameter file the receipt importer reads beside the
// KUPAIN_{n}.doc files.
const KUPAINPRMName = "KUPAIN.prm"

// receiptBatTimeout bounds one run of the receipt BAT (model (b)). It sits
// inside the route's 120 s write deadline.
const receiptBatTimeout = 90 * time.Second

// maxImportDetail caps the BAT output kept in the index entry.
const maxImportDetail = 1000

// Crash points for tests: where a process kill is simulated.
const (
	crashAfterIndexReserved  = "index-reserved"        // the index owns the number; the store has not moved on
	crashAfterStoreAdvanced  = "store-advanced"        // the store has moved on; no file is in place yet
	crashBeforeImportStarted = "before-import-started" // written; "import started" not yet recorded
)

// recoveredWithoutBatWarning is stored on a model-(b) receipt that Recover
// finished: its BAT was never run and never will be.
const recoveredWithoutBatWarning = "this receipt was completed by recovery after an interrupted write; the receipt BAT was not run for it — import it by hand"

// ErrStoredReceiptUnbuildable means a receipt request already stored in the
// index — being finished by Recover or by a repeat — no longer builds. It is
// the connector's fault (an index written by another version, a hand edit),
// never the current caller's: the FieldError that caused it is kept out of the
// error chain so the route cannot answer it as a 400.
var ErrStoredReceiptUnbuildable = errors.New("stored receipt request cannot be built")

// ErrReceiptDataInUse means another writer — in practice another connector
// process on the same data directory — holds the receipt data. The import lock
// only serialises one process; two processes each reading the store would give
// one number to two keys. This writer issues nothing until the other lets go
// (exits), and then takes over on its next request.
var ErrReceiptDataInUse = errors.New("the receipt data directory is in use by another connector process")

// receiptLockName is the OS lock file in DataDir/receipts, held for as long as
// a writer has recovered (from the first Recover or Write to Close or exit).
const receiptLockName = "writer.lock"

// ReceiptNumberStorePath is where the receipt number store lives inside the
// connector's data directory. cutover-seed writes the seed here.
func ReceiptNumberStorePath(dataDir string) string {
	return filepath.Join(dataDir, "receipts", "receiptNumber.json")
}

// ReceiptWriterConfig is what the writer needs from the connector's config.
type ReceiptWriterConfig struct {
	// SendReceiptDir is the folder the receipt importer reads (herp). Only
	// KUPAIN_{n}.doc and KUPAIN.prm are ever written to it.
	SendReceiptDir string
	// DataDir is the connector's data directory (paths.DataDir()). The number
	// store, the idempotency index and the history copies live under it — never
	// in SendReceiptDir, which on a real install sits under a web root.
	DataDir string
	// HasReceiptBatFile switches on model (b): when set, the BAT is run once
	// after each receipt is written. Empty is model (a): files only.
	HasReceiptBatFile string
	// CardNames overrides the built-in card-name table for F17.
	CardNames ReceiptCardNames
}

// ReceiptResult is what a Write produced or, on a repeat, what the key's first
// Write produced. The route answers it as
// {receiptNumber, written:true, idempotencyKey, import, warnings}.
type ReceiptResult struct {
	ReceiptNumber  int64
	IdempotencyKey string // canonical (lower-case) form
	Written        bool   // always true on success: the file is in place
	Import         ImportState
	// Warnings are the builder's (e.g. F17 left blank) plus anything this call
	// noticed (a repeat whose body differs from the stored one, an import
	// outcome that could not be recorded).
	Warnings []string
	Files    []string
	// Replayed is true when the key had been seen before; the stored request,
	// not this call's, is what the receipt holds.
	Replayed bool
}

// ReceiptWriter turns a validated receipt request into exactly one KUPAIN
// receipt per idempotency key, with gapless numbers and files that appear
// whole.
//
// One Write, under the import lock shared with the order worker:
//
//  1. validate the request in full — before any number is touched;
//  2. a key already written answers its stored number and import state, and
//     touches nothing; a key only reserved (a crash) is finished with its
//     stored request and number;
//  3. otherwise take the store's next number: persist the index entry that owns
//     it (state reserved), then move the store on;
//  4. write the history copies, KUPAIN.prm if it is missing or different, and
//     last KUPAIN_{n}.doc — each by atomic rename, so a poller sees a whole
//     file or none, and the .doc appears only when everything else is ready;
//  5. persist state written;
//  6. model (b) only: persist import started, run the BAT, persist ok/failed.
//     A key whose import has started never runs the BAT again.
//
// Recover, run at startup (and by the first Write if it has not succeeded),
// finishes every reserved entry with steps 4–5 and moves the store past every
// number the index owns.
type ReceiptWriter struct {
	cfg        ReceiptWriterConfig
	store      *ReceiptNumberStore
	index      receiptIndex
	historyDir string
	log        logger.LoggerService

	// recovered is guarded by importLock. False until Recover succeeds, and
	// set back to false when the store could not be moved past a reserved
	// number, so no new number is issued until Recover has repaired it.
	recovered bool
	// dataLock is the OS lock on DataDir/receipts/writer.lock (guarded by
	// importLock). recovered is only ever true while it is held.
	dataLock *filelock.Held

	// Test seams.
	crashPoint func(point string)
	runBat     func(ctx context.Context, batPath string) (int, string, error)
	batTimeout time.Duration
}

// NewReceiptWriter builds a writer. It touches nothing on disk; call Recover
// before serving.
func NewReceiptWriter(cfg ReceiptWriterConfig, log logger.LoggerService) (*ReceiptWriter, error) {
	if strings.TrimSpace(cfg.SendReceiptDir) == "" {
		return nil, errors.New("receipt writer: sendReceiptDir is not configured")
	}
	if strings.TrimSpace(cfg.DataDir) == "" {
		return nil, errors.New("receipt writer: data directory is not set")
	}
	return &ReceiptWriter{
		cfg:        cfg,
		store:      NewReceiptNumberStore(ReceiptNumberStorePath(cfg.DataDir)),
		index:      receiptIndex{dir: filepath.Join(cfg.DataDir, "receipts", "index")},
		historyDir: filepath.Join(cfg.DataDir, "history", "receipts"),
		log:        log,
		runBat:     runReceiptBatFile,
		batTimeout: receiptBatTimeout,
	}, nil
}

// Recover finishes every receipt a crash left reserved and makes sure the
// store is past every number the index owns. It never runs the receipt BAT.
// An install with no receipts yet recovers without touching the store, so an
// unseeded connector still starts (and answers receipts 503).
func (w *ReceiptWriter) Recover() error {
	if err := lockImport(context.Background()); err != nil {
		return err
	}
	defer unlockImport()
	return w.recoverLocked()
}

func (w *ReceiptWriter) recoverLocked() error {
	if err := w.lockDataLocked(); err != nil {
		return err
	}
	entries, err := w.index.all()
	if err != nil {
		return fmt.Errorf("recover receipts: %w", err)
	}
	if len(entries) == 0 {
		w.recovered = true
		return nil
	}

	highest := entries[len(entries)-1]
	next, err := w.store.Peek()
	if err != nil {
		return fmt.Errorf("recover receipts: %w", err)
	}
	if next <= highest.Number {
		if highest.State == receiptWritten {
			// Not a crash: the store was moved back (restored, hand-edited).
			// Moving it forward is safe — it never reissues a number — but a
			// person should know.
			w.log.Warn(fmt.Sprintf("receipt number store offered %d but receipt %d is already written; moving the store past it", next, highest.Number))
		}
		if err := w.store.advancePast(highest.Number); err != nil {
			return fmt.Errorf("recover receipts: %w", err)
		}
	}

	for _, e := range entries {
		if e.State != receiptReserved {
			continue
		}
		// Model (b): Recover never runs the BAT and no repeat will either, so
		// import stays not_run — which the backend would otherwise read as an
		// ordinary model-(a) answer. The note is saved with state written, so
		// every replay of the key returns it.
		var notes []string
		if w.batConfigured() {
			notes = append(notes, recoveredWithoutBatWarning)
		}
		if err := w.complete(e, notes...); err != nil {
			return fmt.Errorf("recover receipt %d (key %s): %w", e.Number, e.Key, err)
		}
		msg := fmt.Sprintf("recovered receipt %d key=%s: file written after an interrupted write", e.Number, e.Key)
		if w.batConfigured() {
			msg += "; the receipt BAT was NOT run for it — import it by hand"
		}
		w.log.Warn(msg)
	}
	w.recovered = true
	return nil
}

// lockDataLocked takes the data-directory lock if this writer does not hold
// it yet. Caller holds importLock.
func (w *ReceiptWriter) lockDataLocked() error {
	if w.dataLock != nil {
		return nil
	}
	dir := filepath.Join(w.cfg.DataDir, "receipts")
	if err := privateDirs(dir, w.index.dir, filepath.Dir(w.historyDir), w.historyDir); err != nil {
		return fmt.Errorf("lock receipt data: %w", err)
	}
	path := filepath.Join(dir, receiptLockName)
	l, err := filelock.Lock(path)
	if errors.Is(err, filelock.ErrLocked) {
		return fmt.Errorf("%w (%s)", ErrReceiptDataInUse, path)
	}
	if err != nil {
		return fmt.Errorf("lock receipt data: %w", err)
	}
	w.dataLock = l
	return nil
}

// Close releases the data-directory lock, so another connector process may
// take over. The writer can be used again: its next Write recovers (and locks)
// first. The OS releases the lock anyway when the process exits.
func (w *ReceiptWriter) Close() error {
	if err := lockImport(context.Background()); err != nil {
		return err
	}
	defer unlockImport()
	w.recovered = false
	if w.dataLock == nil {
		return nil
	}
	err := w.dataLock.Unlock()
	w.dataLock = nil
	return err
}

// Write produces the receipt for req, or answers the one its idempotency key
// already has. Errors: *FieldError and *CardNamesConfigError (nothing
// reserved), ErrStoreUnavailable, ErrReceiptIndexUnreadable, ctx's error while
// waiting for the import lock (nothing reserved), or a filesystem error (the
// number stays owned by the key; a repeat or Recover finishes it).
func (w *ReceiptWriter) Write(ctx context.Context, req ReceiptRequest) (*ReceiptResult, error) {
	key, ok := canonicalReceiptKey(req.IdempotencyKey)
	if !ok {
		return nil, &FieldError{Field: "idempotencyKey", Reason: "must be a UUID (8-4-4-4-12 hex digits)"}
	}
	// Full validation before any number is touched. The receipt number only
	// fills F3, so any in-range number gives the same verdict.
	if _, _, err := buildKUPAINRecord(req, 1, w.cfg.CardNames); err != nil {
		return nil, err
	}

	if err := lockImport(ctx); err != nil {
		return nil, err
	}
	defer unlockImport()

	if !w.recovered {
		if err := w.recoverLocked(); err != nil {
			return nil, err
		}
	}

	e, found, err := w.index.load(key)
	if err != nil {
		return nil, err
	}
	if found {
		var notes []string
		if !sameReceiptRequest(e.Request, req, key) {
			notes = append(notes, fmt.Sprintf("this request differs from the one first sent with idempotency key %s; receipt %d was written from the first", key, e.Number))
			w.log.Warn(fmt.Sprintf("receipt key=%s repeated with a different body; answering receipt %d as first written", key, e.Number))
		}
		if e.State == receiptReserved {
			if err := w.complete(e); err != nil {
				return nil, err
			}
			notes = append(notes, w.runImport(ctx, e)...)
		}
		return w.result(e, true, notes), nil
	}

	n, err := w.store.Peek()
	if err != nil {
		return nil, err
	}
	// A history folder for a number the store calls fresh means the store was
	// moved back (a partial restore, a hand edit) and n was issued before.
	// Never write over a receipt already given; a person sorts out the store.
	if _, err := os.Lstat(w.historyPath(n)); err == nil {
		return nil, fmt.Errorf("%w: the store offers %d, but %s already exists — receipt %d was issued before; move the store past it",
			ErrStoreUnavailable, n, w.historyPath(n), n)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: check %s: %v", ErrStoreUnavailable, w.historyPath(n), err)
	}
	e = &receiptIndexEntry{
		Key:        key,
		Number:     n,
		State:      receiptReserved,
		Request:    req,
		Import:     ImportNotRun,
		ReservedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := w.index.save(e); err != nil {
		return nil, err
	}
	w.crash(crashAfterIndexReserved)
	if err := w.store.advancePast(n); err != nil {
		// The index owns n but the store still offers it. Issue nothing more
		// until Recover has moved the store past it.
		w.recovered = false
		return nil, fmt.Errorf("receipt %d reserved for key %s, but the number store could not move on: %w", n, key, err)
	}
	w.crash(crashAfterStoreAdvanced)

	if err := w.complete(e); err != nil {
		// n stays reserved. Have the next Write — whatever its key — finish
		// n before issuing n+1, so the importer never sees a gap closed late.
		w.recovered = false
		return nil, err
	}
	notes := w.runImport(ctx, e)
	w.log.Success(fmt.Sprintf("receipt %d written key=%s import=%s", e.Number, key, e.Import))
	return w.result(e, false, notes), nil
}

// complete is steps 4–5: files in place, then state written. It is safe to
// repeat: a file already holding the right bytes is not rewritten. notes are
// stored with the builder's warnings in the same save as state written, so
// every later answer for the key carries them.
func (w *ReceiptWriter) complete(e *receiptIndexEntry, notes ...string) error {
	doc, warnings, err := generateKUPAINDOC(e.Request, e.Number, w.cfg.CardNames)
	if err != nil {
		var fe *FieldError
		if errors.As(err, &fe) {
			// %v, not %w: no FieldError may be found in this chain.
			return fmt.Errorf("%w: receipt %d (key %s): %v", ErrStoredReceiptUnbuildable, e.Number, e.Key, err)
		}
		return fmt.Errorf("build receipt %d: %w", e.Number, err) // a CardNamesConfigError stays a config fault
	}
	prm := generateKUPAINPRM()

	base := "KUPAIN_" + strconv.FormatInt(e.Number, 10)
	histDir := w.historyPath(e.Number)
	histDoc := filepath.Join(histDir, base+".doc")
	histPrm := filepath.Join(histDir, base+".prm")
	prmPath := filepath.Join(w.cfg.SendReceiptDir, KUPAINPRMName)
	docPath := filepath.Join(w.cfg.SendReceiptDir, base+".doc")

	// History first and the .doc last: its rename is the moment the importer
	// can see the receipt, so everything it depends on is in place before it.
	// The history copies hold personal data and are the service's alone; the
	// herp pair is the importer's to read.
	if err := privateDirs(histDir); err != nil {
		return err
	}
	for _, f := range []struct {
		path string
		data []byte
		perm os.FileMode
	}{
		{histDoc, doc, 0o600},
		{histPrm, prm, 0o600},
		{prmPath, prm, 0o644},
		{docPath, doc, 0o644},
	} {
		if err := writeIfDifferent(f.path, f.data, f.perm); err != nil {
			return err
		}
	}

	e.State = receiptWritten
	e.Files = []string{docPath, prmPath, histDoc, histPrm}
	e.Warnings = append(warnings, notes...)
	e.WrittenAt = time.Now().UTC().Format(time.RFC3339)
	return w.index.save(e)
}

// runImport is step 6 (model (b) only). It returns notes for the caller.
func (w *ReceiptWriter) runImport(ctx context.Context, e *receiptIndexEntry) []string {
	if !w.batConfigured() || e.Import != ImportNotRun {
		return nil
	}
	e.Import = ImportStarted
	w.crash(crashBeforeImportStarted)
	if err := w.index.save(e); err != nil {
		// Unrecorded, a crash during the BAT would let a repeat run it again.
		e.Import = ImportNotRun
		w.log.Error(fmt.Sprintf("receipt %d: BAT not run, could not record that the import started", e.Number), err)
		return []string{"the receipt BAT was not run: the connector could not record that the import started"}
	}

	// The import outlives the HTTP request: a caller that hangs up must not
	// kill Hashavshevet half-way through an import.
	batCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.batTimeout)
	defer cancel()
	start := time.Now()
	exitCode, output, execErr := w.runBat(batCtx, w.cfg.HasReceiptBatFile)
	w.log.Info(fmt.Sprintf("receipt BAT exit=%d durationMs=%d output=%q receiptNumber=%d",
		exitCode, time.Since(start).Milliseconds(), output, e.Number))

	if execErr != nil || exitCode != 0 {
		e.Import = ImportFailed
		detail := fmt.Sprintf("exit=%d", exitCode)
		if execErr != nil {
			detail += " error=" + execErr.Error()
		}
		if output != "" {
			detail += " output=" + output
		}
		if len(detail) > maxImportDetail {
			detail = detail[:maxImportDetail]
		}
		e.ImportDetail = detail
		w.log.Error(fmt.Sprintf("receipt BAT failed receiptNumber=%d exit=%d", e.Number, exitCode), execErr)
	} else {
		e.Import = ImportOK
	}
	if err := w.index.save(e); err != nil {
		w.log.Error(fmt.Sprintf("receipt %d: could not record the import outcome %s", e.Number, e.Import), err)
		return []string{fmt.Sprintf("the import outcome (%s) could not be recorded; a repeat of this key will report the import as started", e.Import)}
	}
	return nil
}

// historyPath is DataDir/history/receipts/{n}.
func (w *ReceiptWriter) historyPath(n int64) string {
	return filepath.Join(w.historyDir, strconv.FormatInt(n, 10))
}

func (w *ReceiptWriter) batConfigured() bool {
	return strings.TrimSpace(w.cfg.HasReceiptBatFile) != ""
}

func (w *ReceiptWriter) result(e *receiptIndexEntry, replayed bool, notes []string) *ReceiptResult {
	warnings := append(append([]string{}, e.Warnings...), notes...)
	return &ReceiptResult{
		ReceiptNumber:  e.Number,
		IdempotencyKey: e.Key,
		Written:        true,
		Import:         e.Import,
		Warnings:       warnings,
		Files:          append([]string(nil), e.Files...),
		Replayed:       replayed,
	}
}

func (w *ReceiptWriter) crash(point string) {
	if w.crashPoint != nil {
		w.crashPoint(point)
	}
}

// sameReceiptRequest compares a repeat with the stored request, ignoring how
// the key itself was spelled (its case).
func sameReceiptRequest(stored, got ReceiptRequest, key string) bool {
	stored.IdempotencyKey, got.IdempotencyKey = key, key
	a, errA := json.Marshal(stored)
	b, errB := json.Marshal(got)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

// writeIfDifferent atomically writes data to path unless path already holds
// exactly data, in which case the file (and its mtime) is left alone.
func writeIfDifferent(path string, data []byte, perm os.FileMode) error {
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, data) {
		return nil
	}
	return atomicfile.Write(path, data, perm)
}

// privateDirs makes each directory exist with mode 0700 — also one that
// already exists wider (cutover-seed's store write creates receipts/ 0755).
// The receipt data holds names, phones, emails, card digits and codes.
func privateDirs(dirs ...string) error {
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		if err := chmodPrivateDir(d); err != nil {
			return err
		}
	}
	return nil
}

// chmodPrivateDir tightens an existing directory to 0700.
var chmodPrivateDir = chmodPrivateDirFor(runtime.GOOS)

// chmodPrivateDirFor is a no-op on Windows: the mode bits mean nothing there
// (the data directory's ACLs apply), and os.Chmod is SetFileAttributes, which
// needs FILE_WRITE_ATTRIBUTES and fails under a locked-down ACL — that must
// not make every receipt a 500. Elsewhere a failing chmod is an error.
func chmodPrivateDirFor(goos string) func(dir string) error {
	if goos == "windows" {
		return func(string) error { return nil }
	}
	return func(dir string) error { return os.Chmod(dir, 0o700) }
}
