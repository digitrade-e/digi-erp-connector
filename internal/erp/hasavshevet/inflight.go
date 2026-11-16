package hasavshevet

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/formatfloor"
)

// Plan 087 review R6 (connector v1.0.32): resolve ONE unfinished order or
// receipt index entry from afar — the provision command inflight. An entry
// that can only finish after a folder change blocked that change for good
// (reviews m5, m9: the documents, receipts and restore guards wait for it),
// and the only way out was hand-editing the index on the box.
//
//   - fail: a reserved or retryable entry that was never written becomes
//     failed (a receipt: failed, a state of its own). The box first proves
//     no file of it exists: neither the file by its frozen name in its
//     folder, nor the pickup file, nor the history copy
//     (ErrInflightMayBeWritten otherwise; a place that cannot be looked at
//     counts as written).
//   - done: a reserved or retryable entry becomes written, only when the box
//     finds its written file or its history copy (ErrInflightNotWritten
//     otherwise); those files are what the entry then names.
//   - Compare-and-set: the entry's state must still be the one expected
//     (ErrInflightMoved). A written or failed entry is final
//     (ErrInflightFinal).
//   - A number is never reused and no counter moves: the entry keeps its
//     number, owned by its key, and still counts as issued.
//   - The entry records the change set and the time — in its error (orders)
//     or its warnings (receipts): fields a build back to v1.0.31 still reads.
//     A receipt resolved as failed carries the one new value, its state
//     failed, which a build older than v1.0.32 cannot read (it would refuse
//     every receipt): before writing it the box raises DataDir\format-floor
//     to ReceiptFailedFloor, so no automatic rollback goes below v1.0.32 —
//     only on a box that ever resolved a receipt as failed.
//
// The provision command holds the service stopped while it resolves (the
// worker never holds the entry then) and refuses an order the running
// worker has queued or is writing (entry_busy).

// The inflight kinds and resolutions.
const (
	InflightOrders   = "orders"
	InflightReceipts = "receipts"
	ResolveFail      = "fail"
	ResolveDone      = "done"
)

// What inflight refuses (the provision command's codes in brackets).
var (
	// ErrInflightMissing: no entry for that historyId or receipt number (entry_missing).
	ErrInflightMissing = errors.New("INFLIGHT_MISSING")
	// ErrInflightMoved: the entry is not in the state expected (entry_moved).
	ErrInflightMoved = errors.New("INFLIGHT_MOVED")
	// ErrInflightFinal: the entry is written or failed already (entry_final).
	ErrInflightFinal = errors.New("INFLIGHT_FINAL")
	// ErrInflightMayBeWritten: fail refused — a file of the entry exists, or
	// a place it could be cannot be looked at (may_be_written).
	ErrInflightMayBeWritten = errors.New("INFLIGHT_MAY_BE_WRITTEN")
	// ErrInflightNotWritten: done refused — neither the written file nor its
	// history copy is there (not_written).
	ErrInflightNotWritten = errors.New("INFLIGHT_NOT_WRITTEN")
)

// receiptFailed is a receipt resolved as failed by inflight: never written,
// its number void, a repeat of its key answered ErrReceiptFailed.
const receiptFailed receiptState = "failed"

// ReceiptFailedFloor is the oldest release that reads a receipt resolved as
// failed (formatfloor, plan 088 D11): raised to before one is written.
const ReceiptFailedFloor = "1.0.32"

// ErrReceiptFailed is a repeat of a receipt resolved as failed (inflight,
// review R6): it was never written and never will be under its key; the
// route answers 409 RECEIPT_FAILED.
var ErrReceiptFailed = errors.New("RECEIPT_FAILED")

// InflightFile is one place an entry's document could be.
type InflightFile struct {
	Path string `json:"path"`
	// Role: folder (the frozen name in its typed folder), pickup (the
	// scheduler's folder), history (the connector's copy), receipt (the
	// receipt folder).
	Role   string `json:"role"`
	Exists bool   `json:"exists"`
	// Error: the place could not be looked at (it counts as written).
	Error string `json:"error,omitempty"`
}

// InflightRule is an order's frozen documentRule.
type InflightRule struct {
	Counter     string `json:"counter"`
	Folder      string `json:"folder"`
	Temporary   bool   `json:"temporary"`
	FileName    string `json:"fileName"`
	CounterTest bool   `json:"counterTest"`
	FolderTest  bool   `json:"folderTest"`
}

// InflightEntry is an entry as the inflight command shows it.
type InflightEntry struct {
	Kind string `json:"kind"`
	// Key is the historyId (orders) or the idempotency key (receipts).
	Key    string `json:"key"`
	Number int64  `json:"number"`
	// State is the index's: reserved, retryable (orders), written, failed.
	State string `json:"state"`
	// Counter is the counter whose range holds the number ("" when none).
	Counter string `json:"counter,omitempty"`
	// Folder is the typed folder's key, or the role: sendOrderDir, pickup,
	// sendReceiptDir.
	Folder     string        `json:"folder,omitempty"`
	Rule       *InflightRule `json:"rule,omitempty"`
	ReservedAt string        `json:"reservedAt,omitempty"`
	AgeSeconds int64         `json:"ageSeconds"`
	// Files are the files the entry names (a written entry).
	Files []string `json:"files,omitempty"`
	// Note is what the entry records: an order's error, a receipt's warnings
	// (a resolution's change set and time among them).
	Note       string         `json:"note,omitempty"`
	Candidates []InflightFile `json:"candidates"`
}

// written reports whether a candidate exists — or cannot be looked at.
func (e *InflightEntry) written() (exists, unknown bool) {
	for _, c := range e.Candidates {
		exists = exists || c.Exists
		unknown = unknown || c.Error != ""
	}
	return exists, unknown
}

// CheckResolution is whether resolve (fail, done) may be applied to e: nil,
// or ErrInflightFinal, ErrInflightMayBeWritten, ErrInflightNotWritten.
func CheckResolution(e *InflightEntry, resolve string) error {
	switch orderState(e.State) {
	case orderReserved, orderRetryable:
	default:
		return fmt.Errorf("%w: %s %d is %s: there is nothing to resolve", ErrInflightFinal, singular(e.Kind), e.Number, e.State)
	}
	exists, unknown := e.written()
	switch resolve {
	case ResolveFail:
		if exists || unknown {
			return fmt.Errorf("%w: %s %d may have been written: %s", ErrInflightMayBeWritten, singular(e.Kind), e.Number, describeFiles(e.Candidates))
		}
	case ResolveDone:
		if !exists {
			return fmt.Errorf("%w: neither the file of %s %d nor its history copy is there: %s", ErrInflightNotWritten, singular(e.Kind), e.Number, describeFiles(e.Candidates))
		}
	default:
		return fmt.Errorf("resolve %q is neither fail nor done", resolve)
	}
	return nil
}

func singular(kind string) string { return strings.TrimSuffix(kind, "s") }

func describeFiles(fs []InflightFile) string {
	var parts []string
	for _, f := range fs {
		switch {
		case f.Error != "":
			parts = append(parts, fmt.Sprintf("%s (%s) cannot be looked at: %s", f.Path, f.Role, f.Error))
		case f.Exists:
			parts = append(parts, fmt.Sprintf("%s (%s) exists", f.Path, f.Role))
		default:
			parts = append(parts, fmt.Sprintf("%s (%s) is not there", f.Path, f.Role))
		}
	}
	if len(parts) == 0 {
		return "no place to look"
	}
	return strings.Join(parts, "; ")
}

// lookFor is one candidate: Lstat, never following a link.
func lookFor(path, role string) InflightFile {
	f := InflightFile{Path: path, Role: role}
	switch _, err := os.Lstat(path); {
	case err == nil:
		f.Exists = true
	case errors.Is(err, os.ErrNotExist):
	default:
		f.Error = err.Error()
	}
	return f
}

func ageSeconds(reservedAt string, now time.Time) int64 {
	t, err := time.Parse(time.RFC3339, reservedAt)
	if err != nil || now.Before(t) {
		return 0
	}
	return int64(now.Sub(t) / time.Second)
}

// counterHolding is the key of cfg's counter of kind whose range holds n.
func counterHolding(cfg config.Config, dataDir, kind string, n int64) string {
	for _, c := range NewCounterSet(cfg, dataDir).OfKind(kind) {
		if c.InRange(n) {
			return c.Key
		}
	}
	return ""
}

// InspectOrder is historyID's entry in dataDir's order index, and where its
// document could be under cfg. ErrInflightMissing when the key is unknown;
// ErrOrderIndexUnreadable when its entry cannot be read.
func InspectOrder(dataDir string, cfg config.Config, historyID string, now time.Time) (*InflightEntry, error) {
	e, found, err := (orderIndex{dir: OrderIndexDir(dataDir)}).load(historyID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%w: no order was accepted with historyId %q", ErrInflightMissing, historyID)
	}
	return orderView(cfg, dataDir, e, now), nil
}

func orderView(cfg config.Config, dataDir string, e *orderIndexEntry, now time.Time) *InflightEntry {
	v := &InflightEntry{Kind: InflightOrders, Key: e.Key, Number: e.Number, State: string(e.State), ReservedAt: e.ReservedAt,
		AgeSeconds: ageSeconds(e.ReservedAt, now), Files: append([]string(nil), e.Files...), Note: e.Error, Candidates: []InflightFile{}}
	n := strconv.FormatInt(e.Number, 10)
	if r := e.Rule; r != nil {
		v.Rule = &InflightRule{Counter: r.Counter, Folder: r.Folder, Temporary: r.Temporary, FileName: r.FileName, CounterTest: r.CounterTest, FolderTest: r.FolderTest}
		v.Counter, v.Folder = r.Counter, r.Folder
		f, ok := cfg.Documents.Folders[r.Folder]
		if dir := strings.TrimSpace(f.Path); ok && dir != "" {
			v.Candidates = append(v.Candidates, lookFor(filepath.Join(dir, r.FileName), "folder"))
		} else {
			v.Candidates = append(v.Candidates, InflightFile{Path: r.FileName, Role: "folder",
				Error: fmt.Sprintf("folder %s is not configured any more: its file cannot be looked for", r.Folder)})
		}
	} else {
		v.Counter = counterHolding(cfg, dataDir, config.DocumentKindOrders, e.Number)
		v.Folder = "sendOrderDir"
		// Model (a) writes the scheduler's file, model (b) the importer's
		// (whose IMOVEIN.doc the next order overwrites; its history copy is
		// written before the importer runs). Both are looked for: the model
		// may have changed since the reservation.
		if pickup := strings.TrimSpace(cfg.SendOrderPickupDir); pickup != "" {
			if cfg.OrdersPickup() {
				v.Folder = "pickup"
			}
			v.Candidates = append(v.Candidates, lookFor(filepath.Join(pickup, PickupFileName("", e.Number)), "pickup"),
				lookFor(filepath.Join(pickup, PickupFileName("RETURN", e.Number)), "pickup"))
		}
	}
	if dir := strings.TrimSpace(cfg.SendOrderDir); dir != "" && filepath.IsAbs(dir) {
		h := filepath.Join(dir, "history", n)
		v.Candidates = append(v.Candidates, lookFor(filepath.Join(h, "IMOVEIN_"+n+".doc"), "history"),
			lookFor(filepath.Join(h, "IMOVEIN_"+n+".prm"), "history"))
	} else {
		v.Candidates = append(v.Candidates, InflightFile{Path: filepath.Join("history", n), Role: "history",
			Error: "sendOrderDir is not an absolute folder: the history copy cannot be looked for"})
	}
	return v
}

// ResolveOrder resolves historyID's entry as resolve (fail, done) after
// checking it is still in state expect and the resolution's rule (above);
// changeSet and now are recorded on it. It answers the entry as saved, or —
// refused — as read.
func ResolveOrder(dataDir string, cfg config.Config, historyID, expect, resolve, changeSet string, now time.Time) (*InflightEntry, error) {
	ix := orderIndex{dir: OrderIndexDir(dataDir)}
	e, found, err := ix.load(historyID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%w: no order was accepted with historyId %q", ErrInflightMissing, historyID)
	}
	v := orderView(cfg, dataDir, e, now)
	if string(e.State) != expect {
		return v, fmt.Errorf("%w: order %d is %s, the plan expected %s", ErrInflightMoved, e.Number, e.State, expect)
	}
	if err := CheckResolution(v, resolve); err != nil {
		return v, err
	}
	at := now.UTC().Format(time.RFC3339)
	switch resolve {
	case ResolveFail:
		e.State = orderFailed
		e.Error = orderErrorText(fmt.Errorf("resolved as failed by inflight change set %s at %s: never written (none of %s); "+
			"a new order needs a new historyId", changeSet, at, candidatePaths(v.Candidates)))
	case ResolveDone:
		e.State = orderWritten
		e.Files = existingPaths(v.Candidates)
		e.Error = orderErrorText(fmt.Errorf("resolved as written by inflight change set %s at %s: found %s", changeSet, at, strings.Join(e.Files, ", ")))
	}
	e.FinishedAt = at
	if err := ix.save(e); err != nil {
		return v, err
	}
	return orderView(cfg, dataDir, e, now), nil
}

func existingPaths(fs []InflightFile) []string {
	var out []string
	for _, f := range fs {
		if f.Exists {
			out = append(out, f.Path)
		}
	}
	return out
}

func candidatePaths(fs []InflightFile) string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Path)
	}
	return strings.Join(out, ", ")
}

// InspectReceipt is the receipt index entry holding number, and where its
// file could be under cfg. ErrInflightMissing when no readable entry holds
// it; ErrReceiptIndexUnreadable when it may be among entries that cannot be
// read, or two entries hold it.
func InspectReceipt(dataDir string, cfg config.Config, number int64, now time.Time) (*InflightEntry, error) {
	e, err := receiptEntryNumbered(dataDir, number)
	if err != nil {
		return nil, err
	}
	return receiptView(cfg, dataDir, e, now), nil
}

// receiptEntryNumbered reads every receipt index entry and answers the one
// holding number.
func receiptEntryNumbered(dataDir string, number int64) (*receiptIndexEntry, error) {
	ix := receiptIndex{dir: filepath.Join(dataDir, "receipts", "index")}
	des, err := os.ReadDir(ix.dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("list the receipt index %s: %w", ix.dir, err)
	}
	var hit *receiptIndexEntry
	unreadable := 0
	for _, de := range des {
		name := de.Name()
		if de.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		e, found, err := ix.load(strings.TrimSuffix(name, ".json"))
		if err != nil {
			unreadable++
			continue
		}
		if !found || e.Number != number {
			continue
		}
		if hit != nil {
			return nil, fmt.Errorf("%w: keys %s and %s both hold receipt %d", ErrReceiptIndexUnreadable, hit.Key, e.Key, number)
		}
		hit = e
	}
	switch {
	case hit != nil:
		return hit, nil
	case unreadable > 0:
		return nil, fmt.Errorf("%w: no readable entry holds receipt %d, and %d cannot be read (it may be among them)", ErrReceiptIndexUnreadable, number, unreadable)
	}
	return nil, fmt.Errorf("%w: no receipt %d in the receipt index", ErrInflightMissing, number)
}

func receiptView(cfg config.Config, dataDir string, e *receiptIndexEntry, now time.Time) *InflightEntry {
	v := &InflightEntry{Kind: InflightReceipts, Key: e.Key, Number: e.Number, State: string(e.State), ReservedAt: e.ReservedAt,
		AgeSeconds: ageSeconds(e.ReservedAt, now), Files: append([]string(nil), e.Files...), Note: strings.Join(e.Warnings, "; "),
		Counter: counterHolding(cfg, dataDir, config.DocumentKindReceipts, e.Number), Candidates: []InflightFile{}}
	v.Folder = "sendReceiptDir"
	if k := strings.TrimSpace(cfg.Documents.Receipts.Folder); k != "" {
		v.Folder = k
	}
	n := strconv.FormatInt(e.Number, 10)
	if dir := strings.TrimSpace(cfg.ReceiptDir()); dir != "" && filepath.IsAbs(dir) {
		name := "KUPAIN_" + n + ".doc"
		if p := strings.TrimSpace(cfg.ReceiptNamePattern()); p != "" {
			name = config.DocumentFileName(p, "", false, e.Number)
		}
		v.Candidates = append(v.Candidates, lookFor(filepath.Join(dir, name), "receipt"))
	} else {
		v.Candidates = append(v.Candidates, InflightFile{Path: "KUPAIN_" + n + ".doc", Role: "receipt",
			Error: "the receipt folder is not an absolute folder: the file cannot be looked for"})
	}
	v.Candidates = append(v.Candidates, lookFor(filepath.Join(dataDir, "history", "receipts", n, "KUPAIN_"+n+".doc"), "history"))
	return v
}

// ResolveReceipt resolves the receipt holding number as resolve (fail,
// done), as ResolveOrder does an order. done names the files found and says
// the connector ran no importer for it; fail marks it failed, its number void.
func ResolveReceipt(dataDir string, cfg config.Config, number int64, expect, resolve, changeSet string, now time.Time) (*InflightEntry, error) {
	e, err := receiptEntryNumbered(dataDir, number)
	if err != nil {
		return nil, err
	}
	v := receiptView(cfg, dataDir, e, now)
	if string(e.State) != expect {
		return v, fmt.Errorf("%w: receipt %d is %s, the plan expected %s", ErrInflightMoved, e.Number, e.State, expect)
	}
	if err := CheckResolution(v, resolve); err != nil {
		return v, err
	}
	at := now.UTC().Format(time.RFC3339)
	switch resolve {
	case ResolveFail:
		// The one format an older build cannot read: no rollback below it.
		if _, err := formatfloor.Raise(dataDir, ReceiptFailedFloor); err != nil {
			return v, fmt.Errorf("record the format floor %s before a receipt is resolved as failed: %w", ReceiptFailedFloor, err)
		}
		e.State = receiptFailed
		e.Warnings = append(e.Warnings, fmt.Sprintf("resolved as failed by inflight change set %s at %s: never written; receipt %d is void — a new receipt needs a new idempotency key",
			changeSet, at, e.Number))
	case ResolveDone:
		e.State = receiptWritten
		e.Files = existingPaths(v.Candidates)
		e.WrittenAt = at
		note := fmt.Sprintf("resolved as written by inflight change set %s at %s: found %s", changeSet, at, strings.Join(e.Files, ", "))
		if strings.TrimSpace(cfg.HasReceiptBatFile) != "" || strings.TrimSpace(cfg.HasReceiptParamFile) != "" {
			note += "; the connector did not run the receipt importer for it — import it by hand if Hashavshevet does not have it"
		}
		e.Warnings = append(e.Warnings, note)
	}
	if err := (receiptIndex{dir: filepath.Join(dataDir, "receipts", "index")}).save(e); err != nil {
		return v, err
	}
	return receiptView(cfg, dataDir, e, now), nil
}
