package hasavshevet

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/atomicfile"
)

// Receipt state lives in the connector's data dir, never in sendReceiptDir:
//
//	DataDir/receipts/receiptNumber.json   {"nextReceiptNumber":N} — the NEXT number
//	DataDir/receipts/index/<key>.json     one entry per idempotency key
//	DataDir/history/receipts/<n>/         the files written for receipt n
//
// sendReceiptDir (herp\receipts on BFL) sits under a web root and is consumed
// by Hashavshevet; it only ever receives KUPAIN_{n}.doc and KUPAIN.prm.

const (
	receiptStateReserved = "reserved"
	receiptStateWritten  = "written"

	statePerm = 0o600
)

func receiptsDir(dataDir string) string { return filepath.Join(dataDir, "receipts") }

func receiptStorePath(dataDir string) string {
	return filepath.Join(receiptsDir(dataDir), "receiptNumber.json")
}

func receiptIndexDir(dataDir string) string { return filepath.Join(receiptsDir(dataDir), "index") }

func receiptHistoryRoot(dataDir string) string {
	return filepath.Join(dataDir, "history", "receipts")
}

func receiptHistoryDir(dataDir string, n int64) string {
	return filepath.Join(receiptHistoryRoot(dataDir), strconv.FormatInt(n, 10))
}

// receiptNumberFile is the number store. It holds the number the NEXT receipt
// gets, so a store seeded from the legacy system reads as "legacy last + 1".
type receiptNumberFile struct {
	NextReceiptNumber int64 `json:"nextReceiptNumber"`
}

// jsonUnmarshalStrict decodes exactly one JSON document with no unknown fields.
// The connector's own state files are read this way so that a damaged or
// hand-edited file is an error, never a silently zeroed value.
func jsonUnmarshalStrict(b []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the JSON document")
	}
	return nil
}

// readReceiptNumber reads the store and fails closed: a missing, unreadable,
// zero, negative or exhausted store is ErrReceiptNumberStore. It never
// restarts at 1 — that would reissue numbers Hashavshevet already holds.
func readReceiptNumber(dataDir string) (int64, error) {
	path := receiptStorePath(dataDir)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("%w: %s does not exist — seed it with cutover-seed -receipt-seed N",
			ErrReceiptNumberStore, path)
	}
	if err != nil {
		return 0, fmt.Errorf("%w: read %s: %v", ErrReceiptNumberStore, path, err)
	}
	// RawMessage, not json.Number: encoding/json would accept the string "12"
	// into a Number, and the store holds a JSON integer or it is corrupt.
	var f struct {
		NextReceiptNumber json.RawMessage `json:"nextReceiptNumber"`
	}
	if err := jsonUnmarshalStrict(b, &f); err != nil {
		return 0, fmt.Errorf("%w: %s is corrupt: %v", ErrReceiptNumberStore, path, err)
	}
	if f.NextReceiptNumber == nil {
		return 0, fmt.Errorf("%w: %s has no nextReceiptNumber", ErrReceiptNumberStore, path)
	}
	raw := string(f.NextReceiptNumber)
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%w: %s holds %s, not a receipt number", ErrReceiptNumberStore, path, raw)
	}
	if n > maxReceiptNumber {
		return 0, fmt.Errorf("%w: receipt numbers are exhausted (next %d is past %d)",
			ErrReceiptNumberStore, n, maxReceiptNumber)
	}
	return n, nil
}

func writeReceiptNumber(dataDir string, next int64) error {
	b, err := json.Marshal(receiptNumberFile{NextReceiptNumber: next})
	if err != nil {
		return err
	}
	return atomicfile.Write(receiptStorePath(dataDir), b, statePerm)
}

// SeedReceiptNumber creates the number store so the next receipt is numbered
// next. It is the only way the store comes into being: it refuses when a store
// already exists (it never moves one, up or down) and when next is at or below
// a number this installation already issued, per its index or history.
func SeedReceiptNumber(dataDir string, next int64) error {
	if next < 1 || next > maxReceiptNumber {
		return fmt.Errorf("receipt seed must be between 1 and %d, got %d", maxReceiptNumber, next)
	}
	path := receiptStorePath(dataDir)
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%s already exists; a receipt number store is never re-seeded or lowered", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check %s: %w", path, err)
	}
	issued, err := highestIssuedReceipt(dataDir)
	if err != nil {
		return err
	}
	if next <= issued {
		return fmt.Errorf("receipt %d was already issued here; seed above it (at least %d)", issued, issued+1)
	}
	return writeReceiptNumber(dataDir, next)
}

// highestIssuedReceipt is the largest number found in history or the index.
func highestIssuedReceipt(dataDir string) (int64, error) {
	var high int64
	hist, err := os.ReadDir(receiptHistoryRoot(dataDir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("read receipt history: %w", err)
	}
	for _, e := range hist {
		if n, err := strconv.ParseInt(e.Name(), 10, 64); err == nil && n > high {
			high = n
		}
	}
	entries, err := os.ReadDir(receiptIndexDir(dataDir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("read receipt index: %w", err)
	}
	for _, e := range entries {
		key, ok := indexFileKey(e.Name())
		if !ok {
			continue
		}
		entry, err := readReceiptIndexFile(filepath.Join(receiptIndexDir(dataDir), e.Name()), key)
		if err != nil {
			return 0, fmt.Errorf("receipt index entry %s is unreadable (%v); fix it before seeding", e.Name(), err)
		}
		if entry.ReceiptNumber > high {
			high = entry.ReceiptNumber
		}
	}
	return high, nil
}

// ReceiptImport is the outcome of the import step for one receipt.
type ReceiptImport string

const (
	// ReceiptImportNotRun — model (a), files only; or a receipt finished on
	// someone else's behalf, whose BAT is never run.
	ReceiptImportNotRun ReceiptImport = "not_run"
	// ReceiptImportStarted — the BAT was launched and its outcome is unknown
	// (the connector stopped while it ran). It is never re-run.
	ReceiptImportStarted ReceiptImport = "started"
	ReceiptImportOK      ReceiptImport = "ok"
	ReceiptImportFailed  ReceiptImport = "failed"
)

// receiptIndexEntry is DataDir/receipts/index/<key>.json. The request is
// stored whole so a reserved receipt can be finished with exactly the values
// its number was reserved for.
type receiptIndexEntry struct {
	IdempotencyKey string         `json:"idempotencyKey"`
	ReceiptNumber  int64          `json:"receiptNumber"`
	State          string         `json:"state"`
	Import         ReceiptImport  `json:"import,omitempty"`
	Warnings       []string       `json:"warnings"`
	ReservedAt     string         `json:"reservedAt"`
	WrittenAt      string         `json:"writtenAt,omitempty"`
	Request        ReceiptRequest `json:"request"`
}

func (e receiptIndexEntry) result() ReceiptResult {
	warnings := append([]string{}, e.Warnings...)
	imp := e.Import
	if imp == "" {
		imp = ReceiptImportNotRun
	}
	return ReceiptResult{
		ReceiptNumber:  e.ReceiptNumber,
		IdempotencyKey: e.IdempotencyKey,
		Import:         imp,
		Warnings:       warnings,
	}
}

// indexFileKey maps an index file name to its key; temp files and anything
// else that is not "<uuid>.json" are not entries.
func indexFileKey(name string) (string, bool) {
	key, ok := strings.CutSuffix(name, ".json")
	if !ok || !idempotencyKeyPattern.MatchString(key) {
		return "", false
	}
	return key, true
}

// readReceiptIndexFile reads one entry and checks it is whole and belongs to
// key. Any doubt is an error: an entry that cannot be trusted may own a number.
func readReceiptIndexFile(path, key string) (receiptIndexEntry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return receiptIndexEntry{}, err
	}
	var e receiptIndexEntry
	if err := jsonUnmarshalStrict(b, &e); err != nil {
		return receiptIndexEntry{}, err
	}
	switch {
	case e.IdempotencyKey != key || e.Request.IdempotencyKey != key:
		return receiptIndexEntry{}, fmt.Errorf("entry belongs to key %q, not %q", e.IdempotencyKey, key)
	case e.ReceiptNumber < 1 || e.ReceiptNumber > maxReceiptNumber:
		return receiptIndexEntry{}, fmt.Errorf("receipt number %d out of range", e.ReceiptNumber)
	case e.State != receiptStateReserved && e.State != receiptStateWritten:
		return receiptIndexEntry{}, fmt.Errorf("unknown state %q", e.State)
	}
	switch e.Import {
	case "", ReceiptImportNotRun, ReceiptImportStarted, ReceiptImportOK, ReceiptImportFailed:
	default:
		return receiptIndexEntry{}, fmt.Errorf("unknown import outcome %q", e.Import)
	}
	return e, nil
}

func writeReceiptIndex(dataDir string, e receiptIndexEntry) error {
	if e.Warnings == nil {
		e.Warnings = []string{}
	}
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(receiptIndexDir(dataDir), e.IdempotencyKey+".json"), b, statePerm)
}
