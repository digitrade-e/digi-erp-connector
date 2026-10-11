package hasavshevet

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/atomicfile"
)

// ErrReceiptIndexUnreadable means an idempotency index entry exists but cannot
// be trusted. The writer refuses rather than treat the key as new: a key that
// already owns a number must never be given a second one.
var ErrReceiptIndexUnreadable = errors.New("receipt index entry unreadable")

// receiptState is how far a receipt got. reserved: the number is owned by the
// key but the file may not be in place yet. written: the file is in place.
type receiptState string

const (
	receiptReserved receiptState = "reserved"
	receiptWritten  receiptState = "written"
)

// ImportState is what happened to the receipt BAT (model (b)) for a receipt.
type ImportState string

const (
	// ImportNotRun: no BAT was run — model (a), where the receipt is imported
	// by hand, or a receipt completed by Recover.
	ImportNotRun ImportState = "not_run"
	// ImportStarted: the BAT was started and its outcome was never recorded
	// (the process died while it ran). It may or may not have imported; it is
	// never run again for this key.
	ImportStarted ImportState = "started"
	ImportOK      ImportState = "ok"
	ImportFailed  ImportState = "failed"
)

// receiptIndexEntry is DataDir/receipts/index/<key>.json: everything needed to
// answer a repeat of the key, or to finish the receipt after a crash.
type receiptIndexEntry struct {
	Key          string         `json:"key"`
	Number       int64          `json:"number"`
	State        receiptState   `json:"state"`
	Request      ReceiptRequest `json:"request"`
	Files        []string       `json:"files"`
	Import       ImportState    `json:"import"`
	ImportDetail string         `json:"importDetail,omitempty"`
	Warnings     []string       `json:"warnings,omitempty"`
	ReservedAt   string         `json:"reservedAt"`
	WrittenAt    string         `json:"writtenAt,omitempty"`
}

// receiptIndex is one file per idempotency key, so writing one key never
// rewrites another and a damaged file costs one key, not all of them.
type receiptIndex struct {
	dir string
}

func (ix receiptIndex) path(key string) string {
	return filepath.Join(ix.dir, key+".json")
}

// load returns the entry for a canonical key, or found=false when the key has
// never been seen.
func (ix receiptIndex) load(key string) (entry *receiptIndexEntry, found bool, err error) {
	b, err := os.ReadFile(ix.path(key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("%w: %s: %v", ErrReceiptIndexUnreadable, ix.path(key), err)
	}
	e, err := decodeReceiptIndexEntry(b, key)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %s: %v", ErrReceiptIndexUnreadable, ix.path(key), err)
	}
	return e, true, nil
}

func (ix receiptIndex) save(e *receiptIndexEntry) error {
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicfile.Write(ix.path(e.Key), append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("persist receipt index %s: %w", e.Key, err)
	}
	return nil
}

// all returns every entry, sorted by number. Any unreadable entry fails the
// whole call: recovery must not proceed past a key it cannot account for.
func (ix receiptIndex) all() ([]*receiptIndexEntry, error) {
	dirEntries, err := os.ReadDir(ix.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read receipt index %s: %w", ix.dir, err)
	}
	var out []*receiptIndexEntry
	for _, de := range dirEntries {
		name := de.Name()
		// Skip atomicfile's in-flight temp files (".<key>.json.tmp-*"), which
		// a crash can leave behind; the entry itself is the renamed file.
		if de.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		key := strings.TrimSuffix(name, ".json")
		e, found, err := ix.load(key)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

func decodeReceiptIndexEntry(b []byte, key string) (*receiptIndexEntry, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var e receiptIndexEntry
	if err := dec.Decode(&e); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("data after the entry")
	}
	if e.Key != key {
		return nil, fmt.Errorf("entry is for key %q, file is named for %q", e.Key, key)
	}
	if e.Number < 1 || e.Number > maxReceiptNumber {
		return nil, fmt.Errorf("number %d is out of range", e.Number)
	}
	switch e.State {
	case receiptReserved, receiptWritten:
	default:
		return nil, fmt.Errorf("unknown state %q", e.State)
	}
	switch e.Import {
	case ImportNotRun, ImportStarted, ImportOK, ImportFailed:
	default:
		return nil, fmt.Errorf("unknown import state %q", e.Import)
	}
	return &e, nil
}

// canonicalReceiptKey checks that key is a UUID in its 8-4-4-4-12 hex form and
// returns it lower-cased. The key names a file, so this is also what keeps a
// request from choosing a path; and lower-casing makes the index behave the
// same on Windows' case-insensitive filesystem as everywhere else.
func canonicalReceiptKey(key string) (string, bool) {
	if len(key) != 36 {
		return "", false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return "", false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return "", false
			}
		}
	}
	return strings.ToLower(key), true
}
