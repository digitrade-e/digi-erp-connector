package hasavshevet

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/atomicfile"
)

// ErrStoreUnavailable means no receipt number can be issued: the store was never
// seeded, cannot be read, holds something that is not a valid next number, or
// has run past the nine digits F3 holds. The route answers it 503
// RECEIPT_NUMBER_STORE_UNAVAILABLE.
var ErrStoreUnavailable = errors.New("receipt number store unavailable")

// ReceiptNumberStore is the file-backed source of KUPAIN receipt numbers (F3).
//
// Unlike OrderNumberStore it fails closed. That store restarts at 1 when its
// file is missing or corrupt, which for orders is an inconvenience; for
// receipts it would reissue numbers already printed on legal documents. So the
// file must be created by an explicit seed (cutover-seed -receipt-seed N), and
// anything else — missing, unparsable, zero, negative, fractional, a string, an
// extra field — is ErrStoreUnavailable and the file is left exactly as found.
//
// The file holds the number the NEXT receipt gets, {"nextReceiptNumber": N}.
// Seeding therefore states where numbering starts, without assuming what the
// legacy system's last number was (its KUPAIN number was its own row id).
//
// Taking a number is two steps so that numbers stay gapless across a crash:
// the writer reads Peek, persists the index entry that owns the number, and
// only then advancePast moves the store on. A crash between the two leaves an
// index entry ahead of the store, which ReceiptWriter.Recover repairs.
type ReceiptNumberStore struct {
	mu   sync.Mutex
	path string
}

// receiptNumberFile keeps the number raw so read can insist on a bare integer
// literal: decoding into json.Number would also accept the string "1000".
type receiptNumberFile struct {
	NextReceiptNumber json.RawMessage `json:"nextReceiptNumber"`
	SeededAt          string          `json:"seededAt,omitempty"`
}

// NewReceiptNumberStore returns a store backed by the file at path. Nothing is
// read or created until it is used.
func NewReceiptNumberStore(path string) *ReceiptNumberStore {
	return &ReceiptNumberStore{path: path}
}

// Peek returns the number the next receipt will get, without taking it.
func (s *ReceiptNumberStore) Peek() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, _, err := s.read()
	return n, err
}

// advancePast records that n has been issued: the next number becomes n+1. It
// never lowers the store — advancing past a number already behind it is a
// no-op — so recovery can call it with any number the index holds.
func (s *ReceiptNumberStore) advancePast(n int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next, f, err := s.read()
	if err != nil {
		return err
	}
	if n < next {
		return nil
	}
	return s.write(n+1, f.SeededAt)
}

// Seed creates the store with next as the first receipt number. It writes only
// when no store file exists: an existing store — valid or corrupt — is never
// overwritten, so a seed can neither lower the numbering nor hide a damaged
// file.
func (s *ReceiptNumberStore) Seed(next int64) error {
	if next < 1 || next > maxReceiptNumber {
		return fmt.Errorf("receipt number seed must be 1-%d, got %d", maxReceiptNumber, next)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Lstat(s.path); err == nil {
		return fmt.Errorf("receipt number store %s already exists; a seed is written only once", s.path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("receipt number store %s: %w", s.path, err)
	}
	return s.write(next, time.Now().UTC().Format(time.RFC3339))
}

// read loads and validates the store. Every failure wraps ErrStoreUnavailable.
func (s *ReceiptNumberStore) read() (int64, receiptNumberFile, error) {
	var f receiptNumberFile
	b, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, f, fmt.Errorf("%w: %s does not exist (seed it with cutover-seed -receipt-seed N)", ErrStoreUnavailable, s.path)
		}
		return 0, f, fmt.Errorf("%w: read %s: %v", ErrStoreUnavailable, s.path, err)
	}

	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return 0, f, fmt.Errorf("%w: %s is not a receipt number store: %v", ErrStoreUnavailable, s.path, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return 0, f, fmt.Errorf("%w: %s has data after the store object", ErrStoreUnavailable, s.path)
	}
	if len(f.NextReceiptNumber) == 0 {
		return 0, f, fmt.Errorf("%w: %s has no nextReceiptNumber", ErrStoreUnavailable, s.path)
	}
	// A bare decimal integer only: ParseInt refuses null, "1000" (quoted), 1.5
	// and 1e3 rather than coercing or rounding them.
	raw := string(f.NextReceiptNumber)
	next, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, f, fmt.Errorf("%w: %s nextReceiptNumber %s is not an integer", ErrStoreUnavailable, s.path, raw)
	}
	if next < 1 {
		return 0, f, fmt.Errorf("%w: %s nextReceiptNumber is %d, must be at least 1", ErrStoreUnavailable, s.path, next)
	}
	if next > maxReceiptNumber {
		return 0, f, fmt.Errorf("%w: %s nextReceiptNumber %d is past the last nine-digit receipt number (F3 is exhausted)", ErrStoreUnavailable, s.path, next)
	}
	return next, f, nil
}

func (s *ReceiptNumberStore) write(next int64, seededAt string) error {
	num := json.RawMessage(strconv.FormatInt(next, 10))
	b, err := json.MarshalIndent(receiptNumberFile{NextReceiptNumber: num, SeededAt: seededAt}, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicfile.Write(s.path, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("persist receipt number store: %w", err)
	}
	return nil
}
