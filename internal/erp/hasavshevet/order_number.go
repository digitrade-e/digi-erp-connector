package hasavshevet

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/atomicfile"
)

// ErrOrderCounterUnreadable means lastOrderNumber.json exists but does not
// hold a usable number. No order number is issued until a person repairs it:
// restarting at 1 would hand Hashavshevet numbers it already has.
var ErrOrderCounterUnreadable = errors.New("order counter unreadable")

// OrderNumberStore is a concurrency-safe, file-backed order number counter.
// The JSON file format {"lastOrderNumber": N} matches the legacy Node app
// (config/lastOrderNumber.json) so existing sequences can be continued.
//
// A missing file is a fresh install: the first order is 1. A file that exists
// but cannot be read — not JSON, no number, a negative or fractional one — is
// ErrOrderCounterUnreadable and is left exactly as found (until DIG-1291 it
// silently restarted at 1). Other fields in a legacy file are ignored.
//
// Taking a number for the key index (OrderQueue with UseKeyIndex) is two
// steps, so a crash never leaves a number owned by nobody: Peek, persist the
// index entry that owns the number, then advancePast. A crash between the two
// leaves the entry ahead of the counter; OrderQueue.Recover moves the counter
// past it. Next does both at once, for callers without an index.
//
// Plan 087: with named counters configured the store is a counter's
// (store: lastOrderNumber) and refuses a missing file (refuseMissing,
// ErrCounterMissing) instead of starting at 1; and it never persists a number
// past 999 999 999 (MyID's nine digits: a tenth was cut silently).
type OrderNumberStore struct {
	mu   sync.Mutex
	path string
	// refuseMissing: a missing file is ErrCounterMissing, not a fresh
	// install (NewCounterSet sets it when counters are configured).
	refuseMissing bool
}

type orderNumberFile struct {
	LastOrderNumber int64 `json:"lastOrderNumber"`
}

// NewOrderNumberStore creates a store backed by the file at path.
// The file is created on first use if it does not exist.
func NewOrderNumberStore(path string) *OrderNumberStore {
	return &OrderNumberStore{path: path}
}

// Next atomically increments the order number, persists it, and returns the new value.
// It is safe for concurrent use from multiple goroutines; the backing mutex
// serialises all reads and writes.
func (s *OrderNumberStore) Next() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	last, err := s.read()
	if err != nil {
		return 0, err
	}
	if last+1 > config.MaxDocumentNumber {
		return 0, fmt.Errorf("%w: %s is at %d, the last nine-digit order number", ErrCounterExhausted, s.path, last)
	}
	if err := s.write(last + 1); err != nil {
		return 0, err
	}
	return last + 1, nil
}

// Peek returns the number the next order will get, without taking it.
func (s *OrderNumberStore) Peek() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	last, err := s.read()
	if err != nil {
		return 0, err
	}
	return last + 1, nil
}

// advancePast records that n has been issued: the next number becomes n+1. It
// never lowers the counter — advancing past a number already behind it writes
// nothing — so recovery can call it with any number the index holds.
func (s *OrderNumberStore) advancePast(n int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	last, err := s.read()
	if err != nil {
		return err
	}
	if n <= last {
		return nil
	}
	return s.write(n)
}

// raiseTo makes last the last number issued (the counters command, with the
// service stopped): a missing file is created, one already at last is left
// alone, one above it is ErrCounterLower.
func (s *OrderNumberStore) raiseTo(last int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Lstat(s.path); errors.Is(err, os.ErrNotExist) {
		return s.write(last)
	}
	cur, err := s.read()
	switch {
	case err != nil:
		return err
	case cur == last:
		return nil
	case cur > last:
		return fmt.Errorf("%w: %s holds %d, asked %d", ErrCounterLower, s.path, cur, last)
	}
	return s.write(last)
}

// read returns the last number issued: 0 when the file does not exist —
// ErrCounterMissing instead when refuseMissing is set.
func (s *OrderNumberStore) read() (int64, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if s.refuseMissing {
				return 0, fmt.Errorf("%w: %s does not exist; with named counters configured the order counter never starts again at 1 (restore it, or set it with the counters command)", ErrCounterMissing, s.path)
			}
			return 0, nil
		}
		return 0, fmt.Errorf("%w: read %s: %v", ErrOrderCounterUnreadable, s.path, err)
	}
	// Raw, so that only a bare integer literal passes: decoding into int64
	// would also take a missing key or null as 0.
	var f struct {
		LastOrderNumber json.RawMessage `json:"lastOrderNumber"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return 0, fmt.Errorf("%w: %s is not an order counter: %v", ErrOrderCounterUnreadable, s.path, err)
	}
	raw := string(f.LastOrderNumber)
	last, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || last < 0 {
		return 0, fmt.Errorf("%w: %s lastOrderNumber %q is not a whole number of at least 0", ErrOrderCounterUnreadable, s.path, raw)
	}
	return last, nil
}

// write persists last by atomic rename: a crash mid-write leaves the old
// counter or the new one, never a truncated file (which read would refuse).
func (s *OrderNumberStore) write(last int64) error {
	if last < 0 || last > config.MaxDocumentNumber {
		return fmt.Errorf("%w: refusing to store order number %d in %s (outside 0-%d)", ErrCounterExhausted, last, s.path, config.MaxDocumentNumber)
	}
	b, err := json.MarshalIndent(orderNumberFile{LastOrderNumber: last}, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicfile.Write(s.path, b, 0o600); err != nil {
		return fmt.Errorf("persist order counter: %w", err)
	}
	return nil
}
