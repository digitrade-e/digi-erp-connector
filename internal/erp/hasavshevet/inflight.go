package hasavshevet

import (
	"bytes"
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
//     folder, nor the pickup file, nor the importer's *.MASOFON2.BAK rename
//     of either (review m-b), nor the history copy
//     (ErrInflightMayBeWritten otherwise; a place that cannot be looked at
//     counts as written).
//   - done (review R6-1): a reserved or retryable entry becomes written only
//     on proof of DELIVERY — the file the importer reads, in its target
//     folder, or the importer's rename of it (*.MASOFON2.BAK), with the same
//     bytes as the history copy wherever both exist (review m-a: a planted
//     file proves nothing). The history copy alone proves nothing — every
//     path writes it before the document reaches Hashavshevet
//     (ErrInflightHistoryOnly); only an order may be resolved on it, with
//     acceptHistoryOnly — model (b) truly leaves nothing else after a real
//     import, so a person confirms Hashavshevet has it. A receipt with only
//     its history copy is never marked written: Recover delivers it
//     (complete() is idempotent). Nothing of either: ErrInflightNotWritten.
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
	// ErrInflightNotWritten: done refused — no delivered file (nor its
	// rename) with the history copy's bytes, and no history copy either
	// (not_written).
	ErrInflightNotWritten = errors.New("INFLIGHT_NOT_WRITTEN")
	// ErrInflightHistoryOnly: done refused — only the history copy is there,
	// which every path writes before the document is delivered
	// (history_only; an order: acceptHistoryOnly).
	ErrInflightHistoryOnly = errors.New("INFLIGHT_HISTORY_ONLY")
	// ErrRecoverDelivers rides with ErrInflightHistoryOnly for a receipt:
	// Recover (or a repeat of its key) delivers it from its stored request.
	ErrRecoverDelivers = errors.New("RECOVER_DELIVERS")
)

// The candidates' roles (review R6-1).
const (
	RoleDelivered = "delivered" // the file the importer reads, in its target folder
	RoleImported  = "imported"  // the importer's rename of it (*.MASOFON2.BAK)
	RoleHistory   = "history"   // the connector's history copy
)

// The proof done found (InflightEntry.Proof).
const (
	ProofDelivered   = "delivered"
	ProofImported    = "imported"
	ProofHistoryOnly = "history-only"
	ProofNone        = "none"
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
	// Role: delivered (the file the importer reads: the frozen name in its
	// typed folder, the pickup file, the receipt's file), imported (the
	// importer's *.MASOFON2.BAK rename of it), history (the connector's copy).
	Role   string `json:"role"`
	Exists bool   `json:"exists"`
	// MatchesHistory: a delivered or imported file has the history copy's
	// bytes (nil when there is no history copy to compare with).
	MatchesHistory *bool `json:"matchesHistory,omitempty"`
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
	// Proof is what done would stand on: delivered, imported (a file with the
	// history copy's bytes, or with none to compare), history-only, none.
	Proof string `json:"proof"`
}

// proofFiles are the delivered or imported files that prove delivery: they
// exist and have the history copy's bytes (or there is no history copy).
func (e *InflightEntry) proofFiles() []InflightFile {
	var out []InflightFile
	for _, c := range e.Candidates {
		if (c.Role == RoleDelivered || c.Role == RoleImported) && c.Exists && (c.MatchesHistory == nil || *c.MatchesHistory) {
			out = append(out, c)
		}
	}
	return out
}

func (e *InflightEntry) hasHistory() bool {
	for _, c := range e.Candidates {
		if c.Role == RoleHistory && c.Exists {
			return true
		}
	}
	return false
}

// setProof fills e.Proof from its candidates.
func (e *InflightEntry) setProof() {
	e.Proof = ProofNone
	for _, c := range e.proofFiles() {
		if c.Role == RoleDelivered {
			e.Proof = ProofDelivered
			return
		}
		e.Proof = ProofImported
	}
	if e.Proof == ProofNone && e.hasHistory() {
		e.Proof = ProofHistoryOnly
	}
}

// CheckResolution is whether resolve (fail, done) may be applied to e: nil,
// or ErrInflightFinal, ErrInflightMayBeWritten, ErrInflightNotWritten,
// ErrInflightHistoryOnly. acceptHistoryOnly lets done stand on an order's
// history copy alone (a person confirmed Hashavshevet has it).
func CheckResolution(e *InflightEntry, resolve string, acceptHistoryOnly bool) error {
	switch orderState(e.State) {
	case orderReserved, orderRetryable:
	default:
		return fmt.Errorf("%w: %s %d is %s: there is nothing to resolve", ErrInflightFinal, singular(e.Kind), e.Number, e.State)
	}
	switch resolve {
	case ResolveFail:
		// Review m-b: the importer's rename counts too — history pruned and
		// the delivered file renamed *.MASOFON2.BAK is an order the books have.
		for _, c := range e.Candidates {
			if c.Error != "" || c.Exists {
				return fmt.Errorf("%w: %s %d may have been written: %s", ErrInflightMayBeWritten, singular(e.Kind), e.Number, describeFiles(e.Candidates))
			}
		}
	case ResolveDone:
		switch {
		case len(e.proofFiles()) > 0:
		case e.hasHistory() && e.Kind == InflightReceipts:
			return fmt.Errorf("%w (%w): receipt %d has only its history copy, written before the receipt reaches the folder: it was not delivered (%s)",
				ErrInflightHistoryOnly, ErrRecoverDelivers, e.Number, describeFiles(e.Candidates))
		case e.hasHistory() && !acceptHistoryOnly:
			return fmt.Errorf("%w: order %d has only its history copy, written before the document is delivered: nothing proves Hashavshevet got it (%s)",
				ErrInflightHistoryOnly, e.Number, describeFiles(e.Candidates))
		case e.hasHistory():
		default:
			return fmt.Errorf("%w: no file of %s %d with its history copy's bytes, and no history copy: %s", ErrInflightNotWritten, singular(e.Kind), e.Number, describeFiles(e.Candidates))
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
		case f.Exists && f.MatchesHistory != nil && !*f.MatchesHistory:
			parts = append(parts, fmt.Sprintf("%s (%s) exists, NOT the history copy's bytes", f.Path, f.Role))
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

// maxCompareBytes bounds a document read to compare it with its history copy.
const maxCompareBytes = 8 << 20

// readSmall reads a candidate (never following a link: Lstat first) up to
// maxCompareBytes; ok false when it cannot be.
func readCandidate(path string) ([]byte, bool) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxCompareBytes {
		return nil, false
	}
	b, err := os.ReadFile(path)
	return b, err == nil
}

// renamesOf are the importer's renames in dir of a delivered file whose name
// (lower-cased, without its extension) is base: <base>.….MASOFON2.BAK, any
// case. A folder that cannot be read is one candidate with its error.
func renamesOf(dir, base string) []InflightFile {
	des, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return []InflightFile{{Path: filepath.Join(dir, base+".*.MASOFON2.BAK"), Role: RoleImported, Error: err.Error()}}
	}
	var out []InflightFile
	for _, de := range des {
		lower := strings.ToLower(de.Name())
		if strings.HasPrefix(lower, base+".") && strings.HasSuffix(lower, ".masofon2.bak") {
			out = append(out, InflightFile{Path: filepath.Join(dir, de.Name()), Role: RoleImported, Exists: true})
		}
	}
	return out
}

// delivered are a delivered file's candidates: the file itself and its
// importer's renames, in dir.
func delivered(dir, name string) []InflightFile {
	out := []InflightFile{lookFor(filepath.Join(dir, name), RoleDelivered)}
	return append(out, renamesOf(dir, strings.ToLower(strings.TrimSuffix(name, filepath.Ext(name))))...)
}

// compareWithHistory sets MatchesHistory on every existing delivered or
// imported candidate, against the history copy at historyDoc (when it
// exists and reads).
func compareWithHistory(fs []InflightFile, historyDoc string) {
	h, ok := readCandidate(historyDoc)
	if !ok {
		return
	}
	for i := range fs {
		c := &fs[i]
		if (c.Role != RoleDelivered && c.Role != RoleImported) || !c.Exists {
			continue
		}
		b, ok := readCandidate(c.Path)
		same := ok && bytes.Equal(b, h)
		c.MatchesHistory = &same
	}
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
	sendDir := strings.TrimSpace(cfg.SendOrderDir)
	historyDoc := ""
	if sendDir != "" && filepath.IsAbs(sendDir) {
		historyDoc = filepath.Join(sendDir, "history", n, "IMOVEIN_"+n+".doc")
	}
	if r := e.Rule; r != nil {
		v.Rule = &InflightRule{Counter: r.Counter, Folder: r.Folder, Temporary: r.Temporary, FileName: r.FileName, CounterTest: r.CounterTest, FolderTest: r.FolderTest}
		v.Counter, v.Folder = r.Counter, r.Folder
		f, ok := cfg.Documents.Folders[r.Folder]
		if dir := strings.TrimSpace(f.Path); ok && dir != "" {
			v.Candidates = append(v.Candidates, delivered(dir, r.FileName)...)
		} else {
			v.Candidates = append(v.Candidates, InflightFile{Path: r.FileName, Role: RoleDelivered,
				Error: fmt.Sprintf("folder %s is not configured any more: its file cannot be looked for", r.Folder)})
		}
	} else {
		v.Counter = counterHolding(cfg, dataDir, config.DocumentKindOrders, e.Number)
		v.Folder = "sendOrderDir"
		// Model (a) delivers the scheduler's file into the pickup folder; the
		// model may have changed since the reservation, so it is looked for
		// whenever a pickup folder is configured. Model (b) delivers
		// sendOrderDir\IMOVEIN.doc, which the next order overwrites: it proves
		// nothing about this order — only an importer's rename of it with the
		// history copy's bytes does (or, failing that, acceptHistoryOnly).
		if pickup := strings.TrimSpace(cfg.SendOrderPickupDir); pickup != "" {
			if cfg.OrdersPickup() {
				v.Folder = "pickup"
			}
			v.Candidates = append(v.Candidates, delivered(pickup, PickupFileName("", e.Number))...)
			v.Candidates = append(v.Candidates, delivered(pickup, PickupFileName("RETURN", e.Number))...)
		}
		if historyDoc != "" {
			for _, c := range renamesOf(sendDir, "imovein") {
				v.Candidates = append(v.Candidates, c)
			}
		}
	}
	if historyDoc != "" {
		v.Candidates = append(v.Candidates, lookFor(historyDoc, RoleHistory), lookFor(strings.TrimSuffix(historyDoc, ".doc")+".prm", RoleHistory))
		compareWithHistory(v.Candidates, historyDoc)
		// Model (b)'s renames name no number: one counts only with this
		// order's bytes; the others belong to other orders.
		if e.Rule == nil {
			kept := v.Candidates[:0]
			for _, c := range v.Candidates {
				if c.Role == RoleImported && filepath.Dir(c.Path) == sendDir && (c.MatchesHistory == nil || !*c.MatchesHistory) && c.Error == "" {
					continue
				}
				kept = append(kept, c)
			}
			v.Candidates = kept
		}
	} else {
		v.Candidates = append(v.Candidates, InflightFile{Path: filepath.Join("history", n), Role: RoleHistory,
			Error: "sendOrderDir is not an absolute folder: the history copy cannot be looked for"})
	}
	v.setProof()
	return v
}

// ResolveOrder resolves historyID's entry as resolve (fail, done) after
// checking it is still in state expect and the resolution's rule (above);
// changeSet and now are recorded on it. It answers the entry as saved, or —
// refused — as read.
func ResolveOrder(dataDir string, cfg config.Config, historyID, expect, resolve, changeSet string, acceptHistoryOnly bool, now time.Time) (*InflightEntry, error) {
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
	if err := CheckResolution(v, resolve, acceptHistoryOnly); err != nil {
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
		on := "delivered: " + strings.Join(pathsOf(v.proofFiles()), ", ")
		if v.Proof == ProofHistoryOnly {
			on = "on its history copy alone, a person having confirmed Hashavshevet has it (acceptHistoryOnly)"
		}
		e.Error = orderErrorText(fmt.Errorf("resolved as written by inflight change set %s at %s, %s", changeSet, at, on))
	}
	e.FinishedAt = at
	if err := ix.save(e); err != nil {
		return v, err
	}
	return orderView(cfg, dataDir, e, now), nil
}

// existingPaths are the files that exist and are this entry's: the history
// copies and the proof files (never a delivered file with other bytes).
func existingPaths(fs []InflightFile) []string {
	var out []string
	for _, f := range fs {
		if f.Exists && (f.MatchesHistory == nil || *f.MatchesHistory) {
			out = append(out, f.Path)
		}
	}
	return out
}

func pathsOf(fs []InflightFile) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Path)
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
		v.Candidates = append(v.Candidates, delivered(dir, name)...)
	} else {
		v.Candidates = append(v.Candidates, InflightFile{Path: "KUPAIN_" + n + ".doc", Role: RoleDelivered,
			Error: "the receipt folder is not an absolute folder: the file cannot be looked for"})
	}
	historyDoc := filepath.Join(dataDir, "history", "receipts", n, "KUPAIN_"+n+".doc")
	v.Candidates = append(v.Candidates, lookFor(historyDoc, RoleHistory))
	compareWithHistory(v.Candidates, historyDoc)
	v.setProof()
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
	if err := CheckResolution(v, resolve, false); err != nil {
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
		note := fmt.Sprintf("resolved as written by inflight change set %s at %s, delivered: %s", changeSet, at, strings.Join(pathsOf(v.proofFiles()), ", "))
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
