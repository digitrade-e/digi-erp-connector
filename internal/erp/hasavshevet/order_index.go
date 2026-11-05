package hasavshevet

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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

// ErrOrderIndexUnreadable means an order index entry exists but cannot be
// trusted. The queue refuses rather than treat the key as new: a key that
// already owns an order number must never be given a second one.
var ErrOrderIndexUnreadable = errors.New("order index entry unreadable")

// maxOrderErrorText caps the processing error kept on a failed entry.
const maxOrderErrorText = 1000

// orderState is how far an order got, as far as the index knows.
//
//	reserved: the number is owned by the key; the order is queued, being
//	          written, or — found after a restart — its outcome is unknown.
//	written:  the worker wrote the IMOVEIN files (and ran the importer, if any).
//	failed:   the worker gave up; the key is never written again.
type orderState string

const (
	orderReserved orderState = "reserved"
	orderWritten  orderState = "written"
	orderFailed   orderState = "failed"
)

// orderIndexEntry is DataDir/orders/index/<sha256(key)>.json: what a repeat
// of the key (POST /api/sendOrder with the same historyId) is answered with.
// It holds no order content: only RequestHash, to notice a key reused for a
// different body.
type orderIndexEntry struct {
	Key         string     `json:"key"`
	Number      int64      `json:"number"`
	State       orderState `json:"state"`
	RequestHash string     `json:"requestSha256,omitempty"`
	Files       []string   `json:"files,omitempty"`
	Error       string     `json:"error,omitempty"`
	ReservedAt  string     `json:"reservedAt"`
	FinishedAt  string     `json:"finishedAt,omitempty"`
}

// OrderIndexDir is where the historyId → order number index lives inside the
// connector's data directory. The order counter is NOT here: it stays in
// sendOrderDir (lastOrderNumber.json, the legacy app's place).
func OrderIndexDir(dataDir string) string {
	return filepath.Join(dataDir, "orders", "index")
}

// orderIndex is one file per key, so writing one key never rewrites another
// and a damaged file costs one key, not all of them.
type orderIndex struct {
	dir string
}

// orderIndexStem names a key's file. The key is the caller's free text, so it
// never becomes a path itself: its SHA-256 is fixed-length, lower-case hex
// (the same on a case-insensitive filesystem) and cannot leave the directory.
// The raw key is stored inside the entry and checked against the name.
func orderIndexStem(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func (ix orderIndex) pathFor(stem string) string {
	return filepath.Join(ix.dir, stem+".json")
}

// load returns the entry for key, or found=false when the key has never been
// seen.
func (ix orderIndex) load(key string) (entry *orderIndexEntry, found bool, err error) {
	e, found, err := ix.loadStem(orderIndexStem(key))
	if err != nil || !found {
		return nil, found, err
	}
	if e.Key != key { // a hash collision; unreachable in practice
		return nil, false, fmt.Errorf("%w: %s holds another key", ErrOrderIndexUnreadable, ix.pathFor(orderIndexStem(key)))
	}
	return e, true, nil
}

// loadStem reads the entry stored under a file stem.
func (ix orderIndex) loadStem(stem string) (*orderIndexEntry, bool, error) {
	path := ix.pathFor(stem)
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("%w: %s: %v", ErrOrderIndexUnreadable, path, err)
	}
	e, err := decodeOrderIndexEntry(b, stem)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %s: %v", ErrOrderIndexUnreadable, path, err)
	}
	return e, true, nil
}

func (ix orderIndex) save(e *orderIndexEntry) error {
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicfile.Write(ix.pathFor(orderIndexStem(e.Key)), append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("persist order index entry for order %d: %w", e.Number, err)
	}
	return nil
}

// all returns every entry, sorted by number. Any unreadable entry fails the
// whole call: recovery must not proceed past a key it cannot account for. So
// do two entries holding one number — two keys answering the same order —
// which no correct run produces.
func (ix orderIndex) all() ([]*orderIndexEntry, error) {
	dirEntries, err := os.ReadDir(ix.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: read %s: %v", ErrOrderIndexUnreadable, ix.dir, err)
	}
	var out []*orderIndexEntry
	for _, de := range dirEntries {
		name := de.Name()
		// Skip atomicfile's in-flight temp files (".<stem>.json.tmp-*"), which
		// a crash can leave behind; the entry itself is the renamed file.
		if de.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		e, found, err := ix.loadStem(strings.TrimSuffix(name, ".json"))
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	for i := 1; i < len(out); i++ {
		if out[i].Number == out[i-1].Number {
			return nil, fmt.Errorf("%w: historyIds %q and %q both hold order %d",
				ErrOrderIndexUnreadable, out[i-1].Key, out[i].Key, out[i].Number)
		}
	}
	return out, nil
}

func decodeOrderIndexEntry(b []byte, stem string) (*orderIndexEntry, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var e orderIndexEntry
	if err := dec.Decode(&e); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("data after the entry")
	}
	if e.Key == "" {
		return nil, errors.New("entry has no key")
	}
	if orderIndexStem(e.Key) != stem {
		return nil, errors.New("entry is for a key other than the one its file is named for")
	}
	if e.Number < 1 {
		return nil, fmt.Errorf("order number %d is out of range", e.Number)
	}
	switch e.State {
	case orderReserved, orderWritten, orderFailed:
	default:
		return nil, fmt.Errorf("unknown state %q", e.State)
	}
	return &e, nil
}

// orderErrorText is the processing error as kept on a failed entry: capped,
// and valid UTF-8 after the cut.
func orderErrorText(err error) string {
	s := err.Error()
	if len(s) > maxOrderErrorText {
		s = s[:maxOrderErrorText]
	}
	return strings.ToValidUTF8(s, "")
}
