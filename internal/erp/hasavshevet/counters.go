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
	"strconv"
	"strings"
	"sync"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/atomicfile"
)

// Plan 087 (DIG-1563): named counters.
//
// A counter is a key (config.yaml's documents.counters), a kind (orders or
// receipts), a range, a test flag, and a store file holding the last number it
// issued. Today's two stores become the first two counters in place — the
// order counter in sendOrderDir (legacy Node format, "last"), the receipt store
// in the data directory ("next") — and every other counter has a store of its
// own in DataDir\counters\<key>.json ("last"). Whatever the store holds, a
// Counter speaks one meaning: Last is the last number issued.
//
// Taking a number stays two steps, as before: Peek, persist the index entry
// that owns the number, then advancePast. Recovery and the "never at or below
// the highest issued" guard are per counter, over the numbers in its range:
// ranges on one box are disjoint (config.ValidateDocuments), so every issued
// number belongs to exactly one counter, and a second counter in another range
// can never push the first into its range.

// ErrCounterExhausted means the counter's next number is outside its range:
// the box's state, not a fault in the request. Nothing is reserved; a resend
// with the same key is safe once the range is widened (the counters command).
// The routes answer it 503 COUNTER_EXHAUSTED.
var ErrCounterExhausted = errors.New("counter exhausted")

// ErrCounterMissing means a counter's store file does not exist where the
// counter fails closed: a counter of its own, or — with counters configured —
// the order counter in sendOrderDir, which before plan 087 started again at 1.
// The routes answer it 503 COUNTER_MISSING.
var ErrCounterMissing = errors.New("counter store missing")

// ErrCounterRetired means a new number was asked of a retired counter.
var ErrCounterRetired = errors.New("counter retired")

// ErrCounterLower is RaiseTo asked to move a counter back.
var ErrCounterLower = errors.New("a counter never moves back")

// ErrFolderWritable means the document's folder is one accounts besides the
// administrators can write to, on a box behind the link, so the connector
// does not write into it (plan 087 review M1; the daemon checks at its
// start). Nothing is reserved; the routes answer it 503 FOLDER_WRITABLE.
var ErrFolderWritable = errors.New("folder writable by accounts besides the administrators")

// numberSource is a counter's store: the next number, and moving past one.
type numberSource interface {
	Peek() (int64, error)
	advancePast(n int64) error
}

// Counter is one named counter.
type Counter struct {
	Key      string
	Kind     string // config.DocumentKindOrders | config.DocumentKindReceipts
	Min, Max int64
	Test     bool
	Retired  bool
	// Store is the adopted pre-087 store (config.CounterStore*), or "".
	Store string
	// Implicit: no counter is configured and this is one of today's two
	// stores under its implicit key.
	Implicit bool
	// Path is the store file.
	Path string

	src numberSource
}

// InRange reports whether n is one of the counter's numbers.
func (c *Counter) InRange(n int64) bool { return n >= c.Min && n <= c.Max }

// Peek is the store's next number, range or not.
func (c *Counter) Peek() (int64, error) { return c.src.Peek() }

// Last is the last number issued (the store's next minus one).
func (c *Counter) Last() (int64, error) {
	n, err := c.src.Peek()
	return n - 1, err
}

// next is the number the next document gets: ErrCounterExhausted when it is
// outside the range.
func (c *Counter) next() (int64, error) {
	n, err := c.src.Peek()
	if err != nil {
		return 0, err
	}
	if !c.InRange(n) {
		return 0, fmt.Errorf("%w: counter %s would issue %d, outside its range %d-%d", ErrCounterExhausted, c.Key, n, c.Min, c.Max)
	}
	return n, nil
}

func (c *Counter) advancePast(n int64) error { return c.src.advancePast(n) }

// RaiseTo makes last the counter's last issued number, creating the store when
// it does not exist. It never lowers one: a store already at last writes
// nothing, one above it is ErrCounterLower. Only the provision commands call
// it, with the service stopped (no writer racing it).
func (c *Counter) RaiseTo(last int64) error {
	if last < 0 || last > config.MaxDocumentNumber {
		return fmt.Errorf("counter %s: %d is outside 0-%d", c.Key, last, config.MaxDocumentNumber)
	}
	switch s := c.src.(type) {
	case *OrderNumberStore:
		return s.raiseTo(last)
	case *ReceiptNumberStore:
		return s.raiseTo(last)
	case *CounterFile:
		return s.raiseTo(last)
	case unusableStore:
		return fmt.Errorf("counter %s: %w", c.Key, s.err)
	}
	return fmt.Errorf("counter %s: its store cannot be set", c.Key)
}

// StorePresent reports whether the counter's store file exists (never for a
// store without a folder: Path "").
func (c *Counter) StorePresent() bool {
	if c.Path == "" {
		return false
	}
	_, err := os.Lstat(c.Path)
	return err == nil
}

// CounterSet is the box's counters, built from config.yaml.
type CounterSet struct {
	byKey      map[string]*Counter
	keys       []string
	defaultKey string // orders without a documentRule
	receiptKey string // receipts
	orderStore *OrderNumberStore
}

// CounterStorePath is a counter's own store, DataDir\counters\<key>.json.
func CounterStorePath(dataDir, key string) string {
	return filepath.Join(dataDir, "counters", key+".json")
}

// unusableStore is the store of a counter whose file has no folder: an empty
// or relative sendOrderDir or data directory (review m1). A relative path
// would be the process's working folder — the service's is
// C:\Windows\System32 — written as SYSTEM, so the path is never built:
// every read is err, and RaiseTo refuses it.
type unusableStore struct{ err error }

func (u unusableStore) Peek() (int64, error)    { return 0, u.err }
func (u unusableStore) advancePast(int64) error { return u.err }

// absFolder is dir trimmed when it is an absolute folder, else "".
func absFolder(dir string) string {
	if d := strings.TrimSpace(dir); d != "" && filepath.IsAbs(d) {
		return d
	}
	return ""
}

// NewCounterSet builds the counters of cfg. With none configured it holds
// today's two stores under their implicit keys — the order counter when orders
// are configured, the receipt store when receipts are — with the range
// 1-999 999 999 and today's behaviour (a missing order counter starts at 1).
// With counters configured it holds exactly those, and the order counter in
// sendOrderDir fails closed when missing. cfg is assumed valid
// (config.ValidateDocuments).
func NewCounterSet(cfg config.Config, dataDir string) *CounterSet {
	s := &CounterSet{byKey: map[string]*Counter{}}
	// Never a relative store path (review m1): with sendOrderDir empty or
	// relative the order counter's store has no folder, and with dataDir
	// relative neither has any other store. Such a counter fails closed.
	orderDir, data := absFolder(cfg.SendOrderDir), absFolder(dataDir)
	orderStore := func(refuseMissing bool) (string, numberSource) {
		if orderDir == "" {
			return "", unusableStore{fmt.Errorf("%w: the order counter lives in sendOrderDir, and sendOrderDir %q is not an absolute folder",
				ErrCounterMissing, strings.TrimSpace(cfg.SendOrderDir))}
		}
		p := filepath.Join(orderDir, "lastOrderNumber.json")
		s.orderStore = NewOrderNumberStore(p)
		s.orderStore.refuseMissing = refuseMissing
		return p, s.orderStore
	}
	dataStore := func(p func(string) string, open func(string) numberSource) (string, numberSource) {
		if data == "" {
			return "", unusableStore{fmt.Errorf("%w: the data directory %q is not an absolute folder", ErrCounterMissing, dataDir)}
		}
		path := p(data)
		return path, open(path)
	}
	receiptStore := func() (string, numberSource) {
		return dataStore(ReceiptNumberStorePath, func(p string) numberSource { return NewReceiptNumberStore(p) })
	}
	if !cfg.CountersConfigured() {
		if cfg.OrdersConfigured() {
			p, src := orderStore(false)
			s.add(&Counter{Key: config.ImplicitOrderCounterKey, Kind: config.DocumentKindOrders, Min: 1, Max: config.MaxDocumentNumber,
				Store: config.CounterStoreLastOrderNumber, Implicit: true, Path: p, src: src})
			s.defaultKey = config.ImplicitOrderCounterKey
		}
		if cfg.ReceiptsConfigured() {
			p, src := receiptStore()
			s.add(&Counter{Key: config.ImplicitReceiptCounterKey, Kind: config.DocumentKindReceipts, Min: 1, Max: config.MaxDocumentNumber,
				Store: config.CounterStoreReceiptNumber, Implicit: true, Path: p, src: src})
			s.receiptKey = config.ImplicitReceiptCounterKey
		}
		return s
	}
	for _, k := range cfg.CounterKeys() {
		cc := cfg.Documents.Counters[k]
		c := &Counter{Key: k, Kind: cc.Kind, Min: cc.Min, Max: cc.Max, Test: cc.Test, Retired: cc.Retired, Store: cc.Store}
		switch cc.Store {
		case config.CounterStoreLastOrderNumber:
			c.Path, c.src = orderStore(true)
		case config.CounterStoreReceiptNumber:
			c.Path, c.src = receiptStore()
		default:
			kind := cc.Kind
			c.Path, c.src = dataStore(func(d string) string { return CounterStorePath(d, k) },
				func(p string) numberSource { return NewCounterFile(p, kind) })
		}
		s.add(c)
	}
	// A kind with no configured counter yet keeps today's store under its
	// implicit key (counters are adopted one kind at a time) — the order
	// counter failing closed now that counters are configured.
	if cfg.OrdersConfigured() && cfg.ImplicitCounter(config.DocumentKindOrders) {
		p, src := orderStore(true)
		s.add(&Counter{Key: config.ImplicitOrderCounterKey, Kind: config.DocumentKindOrders, Min: 1, Max: config.MaxDocumentNumber,
			Store: config.CounterStoreLastOrderNumber, Implicit: true, Path: p, src: src})
	}
	if cfg.ReceiptsConfigured() && cfg.ImplicitCounter(config.DocumentKindReceipts) {
		p, src := receiptStore()
		s.add(&Counter{Key: config.ImplicitReceiptCounterKey, Kind: config.DocumentKindReceipts, Min: 1, Max: config.MaxDocumentNumber,
			Store: config.CounterStoreReceiptNumber, Implicit: true, Path: p, src: src})
	}
	if cfg.OrdersConfigured() {
		s.defaultKey = cfg.DefaultOrderCounterKey()
	}
	if cfg.ReceiptsConfigured() {
		s.receiptKey = cfg.ReceiptCounterKey()
	}
	return s
}

// implicitOrderCounters is the set of a queue built without one (tests, and
// callers older than plan 087): the sender's order counter under the implicit
// key, today's behaviour.
func implicitOrderCounters(store *OrderNumberStore) *CounterSet {
	s := &CounterSet{byKey: map[string]*Counter{}, orderStore: store}
	if store != nil {
		s.add(&Counter{Key: config.ImplicitOrderCounterKey, Kind: config.DocumentKindOrders, Min: 1, Max: config.MaxDocumentNumber,
			Store: config.CounterStoreLastOrderNumber, Implicit: true, Path: store.path, src: store})
		s.defaultKey = config.ImplicitOrderCounterKey
	}
	return s
}

func (s *CounterSet) add(c *Counter) {
	s.byKey[c.Key] = c
	s.keys = append(s.keys, c.Key)
	sort.Strings(s.keys)
}

// Get is the counter of key.
func (s *CounterSet) Get(key string) (*Counter, bool) {
	if s == nil {
		return nil, false
	}
	c, ok := s.byKey[key]
	return c, ok
}

// All is every counter, by key.
func (s *CounterSet) All() []*Counter {
	if s == nil {
		return nil
	}
	out := make([]*Counter, 0, len(s.keys))
	for _, k := range s.keys {
		out = append(out, s.byKey[k])
	}
	return out
}

// OfKind is every counter of one kind, by key.
func (s *CounterSet) OfKind(kind string) []*Counter {
	var out []*Counter
	for _, c := range s.All() {
		if c.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

// DefaultOrder numbers the orders sent without a documentRule; nil when the
// box writes no orders.
func (s *CounterSet) DefaultOrder() *Counter {
	c, _ := s.Get(s.defaultKeyOrEmpty())
	return c
}

func (s *CounterSet) defaultKeyOrEmpty() string {
	if s == nil {
		return ""
	}
	return s.defaultKey
}

// Receipts numbers the receipts; nil when the box writes none.
func (s *CounterSet) Receipts() *Counter {
	if s == nil {
		return nil
	}
	c, _ := s.Get(s.receiptKey)
	return c
}

// OrderNumberStore is the order counter in sendOrderDir (lastOrderNumber.json)
// the set uses, for a Sender built beside it; nil when no counter adopts it.
func (s *CounterSet) OrderNumberStore() *OrderNumberStore {
	if s == nil {
		return nil
	}
	return s.orderStore
}

// counterFor is the counter whose range holds n, or nil.
func counterFor(cs []*Counter, n int64) *Counter {
	for _, c := range cs {
		if c.InRange(n) {
			return c
		}
	}
	return nil
}

// CounterFile is a counter's own store, {"last": N}: the last number issued.
// It fails closed: a missing file is ErrCounterMissing (only the counters
// command creates one), and anything but a bare whole number 0-999 999 999 is
// refused and left as found — as ErrOrderCounterUnreadable for an order
// counter, ErrStoreUnavailable for a receipt counter (the codes the routes
// already answer for today's two stores).
type CounterFile struct {
	mu         sync.Mutex
	path       string
	unreadable error
}

type counterFileBody struct {
	Last json.RawMessage `json:"last"`
}

// NewCounterFile is the store at path of a counter of kind; nothing is read
// until it is used.
func NewCounterFile(path, kind string) *CounterFile {
	f := &CounterFile{path: path, unreadable: ErrOrderCounterUnreadable}
	if kind == config.DocumentKindReceipts {
		f.unreadable = ErrStoreUnavailable
	}
	return f
}

// Peek is the next number.
func (f *CounterFile) Peek() (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	last, err := f.read()
	return last + 1, err
}

func (f *CounterFile) advancePast(n int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	last, err := f.read()
	if err != nil {
		return err
	}
	if n <= last {
		return nil
	}
	return f.write(n)
}

func (f *CounterFile) raiseTo(last int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, err := f.read()
	switch {
	case errors.Is(err, ErrCounterMissing):
		return f.write(last)
	case err != nil:
		return err
	case cur == last:
		return nil
	case cur > last:
		return fmt.Errorf("%w: %s holds %d, asked %d", ErrCounterLower, f.path, cur, last)
	}
	return f.write(last)
}

func (f *CounterFile) read() (int64, error) {
	b, err := os.ReadFile(f.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, fmt.Errorf("%w: %s does not exist (the counters command creates it)", ErrCounterMissing, f.path)
		}
		return 0, fmt.Errorf("%w: read %s: %v", f.unreadable, f.path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var body counterFileBody
	if err := dec.Decode(&body); err != nil {
		return 0, fmt.Errorf("%w: %s is not a counter: %v", f.unreadable, f.path, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return 0, fmt.Errorf("%w: %s has data after the counter", f.unreadable, f.path)
	}
	raw := string(body.Last)
	last, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || last < 0 || last > config.MaxDocumentNumber {
		return 0, fmt.Errorf("%w: %s last %q is not a whole number 0-%d", f.unreadable, f.path, raw, config.MaxDocumentNumber)
	}
	return last, nil
}

func (f *CounterFile) write(last int64) error {
	if last < 0 || last > config.MaxDocumentNumber {
		return fmt.Errorf("%w: refusing to store %d in %s (outside 0-%d)", ErrCounterExhausted, last, f.path, config.MaxDocumentNumber)
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return fmt.Errorf("persist counter %s: %w", f.path, err)
	}
	b, _ := json.Marshal(counterFileBody{Last: json.RawMessage(strconv.FormatInt(last, 10))})
	if err := atomicfile.Write(f.path, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("persist counter %s: %w", f.path, err)
	}
	return nil
}

// IssuedScan is what a scan of a kind's index and history says about a
// counter's range: read out of process (provision) or for a route.
type IssuedScan struct {
	// Highest is the highest number issued in the range: an index entry or a
	// history folder.
	Highest int64
	// InFlight counts the range's index entries that are not finished: orders
	// reserved or retryable (a resend writes them), receipts reserved.
	InFlight int
	// Unreadable counts index entries the scan could not read.
	Unreadable int
}

// ScanIssued reads a kind's index and history for the range min-max. For
// orders the index is DataDir\orders\index and the history
// sendOrderDir\history; for receipts DataDir\receipts\index and
// DataDir\history\receipts. Entries are read leniently (number and state
// only); a directory that does not exist holds nothing.
func ScanIssued(kind, dataDir, sendOrderDir string, min, max int64) (IssuedScan, error) {
	var out IssuedScan
	indexDir, historyDir := OrderIndexDir(dataDir), ""
	inFlight := map[string]bool{string(orderReserved): true, string(orderRetryable): true}
	if strings.TrimSpace(sendOrderDir) != "" {
		historyDir = filepath.Join(sendOrderDir, "history")
	}
	if kind == config.DocumentKindReceipts {
		indexDir, historyDir = filepath.Join(dataDir, "receipts", "index"), filepath.Join(dataDir, "history", "receipts")
		inFlight = map[string]bool{string(receiptReserved): true}
	}
	err := eachIndexEntry(indexDir, func(e scannedEntry) {
		if e.Number < min || e.Number > max {
			return
		}
		if e.Number > out.Highest {
			out.Highest = e.Number
		}
		if inFlight[e.State] {
			out.InFlight++
		}
	}, func() { out.Unreadable++ })
	if err != nil {
		return out, err
	}
	if historyDir != "" {
		h, err := highestHistoryInRange(historyDir, min, max)
		if err != nil {
			return out, err
		}
		if h > out.Highest {
			out.Highest = h
		}
	}
	return out, nil
}

// OrdersInFlightForFolder counts the order index entries whose frozen rule
// names folder and that are not finished (reserved or retryable): a resend
// writes them into that folder.
func OrdersInFlightForFolder(dataDir, folder string) (int, error) {
	n := 0
	err := eachIndexEntry(OrderIndexDir(dataDir), func(e scannedEntry) {
		if e.Rule != nil && e.Rule.Folder == folder && (e.State == string(orderReserved) || e.State == string(orderRetryable)) {
			n++
		}
	}, func() {})
	return n, err
}

// ReceiptsInFlight counts the receipt index entries still reserved: Recover
// or a repeat finishes them into the receipt folder in use.
func ReceiptsInFlight(dataDir string) (int, error) {
	n := 0
	err := eachIndexEntry(filepath.Join(dataDir, "receipts", "index"), func(e scannedEntry) {
		if e.State == string(receiptReserved) {
			n++
		}
	}, func() {})
	return n, err
}

type scannedEntry struct {
	Number int64  `json:"number"`
	State  string `json:"state"`
	Rule   *struct {
		Folder string `json:"folder"`
	} `json:"rule"`
}

// maxScanEntryBytes bounds one index entry a scan reads (a receipt entry
// holds its request; an order entry is small).
const maxScanEntryBytes = 1 << 20

func eachIndexEntry(dir string, f func(scannedEntry), unreadable func()) error {
	des, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list %s: %w", dir, err)
	}
	for _, de := range des {
		name := de.Name()
		if de.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		fi, err := de.Info()
		if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxScanEntryBytes {
			unreadable()
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		var e scannedEntry
		if err != nil || json.Unmarshal(b, &e) != nil || e.Number < 1 {
			unreadable()
			continue
		}
		f(e)
	}
	return nil
}

// highestHistoryInRange is the highest number in min-max with a folder under
// dir (history/<n>); 0 when there is none.
func highestHistoryInRange(dir string, min, max int64) (int64, error) {
	ns, err := historyNumbers(dir)
	if err != nil {
		return 0, err
	}
	var top int64
	for _, n := range ns {
		if n >= min && n <= max && n > top {
			top = n
		}
	}
	return top, nil
}

// historyNumbers lists the numbers with a folder under dir: names that are a
// plain decimal (no sign, no leading zero). A dir that does not exist holds
// none.
func historyNumbers(dir string) ([]int64, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("list %s: %w", dir, err)
	}
	var out []int64
	for _, de := range des {
		n, err := strconv.ParseInt(de.Name(), 10, 64)
		if err != nil || !de.IsDir() || strconv.FormatInt(n, 10) != de.Name() {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

// CounterStatus is one counter as the read routes and provision's state
// report it.
type CounterStatus struct {
	Key           string `json:"key"`
	Kind          string `json:"kind"`
	Min           int64  `json:"min"`
	Max           int64  `json:"max"`
	Last          int64  `json:"last"`
	HighestIssued int64  `json:"highestIssued"`
	Remaining     int64  `json:"remaining"`
	Test          bool   `json:"test"`
	Retired       bool   `json:"retired,omitempty"`
	// Store is the adopted pre-087 store, or "" for a store of its own;
	// Implicit: no counter is configured and this is today's store.
	Store        string `json:"store,omitempty"`
	Implicit     bool   `json:"implicit,omitempty"`
	Path         string `json:"path"`
	StorePresent bool   `json:"storePresent"`
	// InFlight counts the range's unfinished index entries (orders reserved
	// or retryable, receipts reserved); -1 when not counted (the API route
	// answers from memory).
	InFlight int    `json:"inFlight"`
	Error    string `json:"error,omitempty"`
}

// ReportCounters reads every counter of set: its last number, the highest
// issued in its range, what remains. In the daemon q and w answer from
// memory what they hold (the order index's numbers, the receipt index's);
// otherwise — provision's state, or a counter no live writer serves — the
// index and history are scanned. Nothing is written.
func ReportCounters(set *CounterSet, q *OrderQueue, w *ReceiptWriter, dataDir, sendOrderDir string) []CounterStatus {
	var out []CounterStatus
	for _, c := range set.All() {
		st := CounterStatus{Key: c.Key, Kind: c.Kind, Min: c.Min, Max: c.Max, Test: c.Test, Retired: c.Retired,
			Store: c.Store, Implicit: c.Implicit, Path: c.Path, StorePresent: c.StorePresent(), InFlight: -1}
		var errs []string
		if last, err := c.Last(); err != nil {
			errs = append(errs, err.Error())
		} else {
			st.Last = last
		}
		var highest int64
		var err error
		known := false
		switch {
		case c.Kind == config.DocumentKindOrders && q != nil:
			highest, known, err = q.highestIssued(c)
		case c.Kind == config.DocumentKindReceipts && w != nil && w.counter != nil && w.counter.Key == c.Key:
			highest, known, err = w.HighestIssued(c.Min, c.Max)
		}
		if err == nil && !known {
			var scan IssuedScan
			scan, err = ScanIssued(c.Kind, dataDir, sendOrderDir, c.Min, c.Max)
			highest, st.InFlight = scan.Highest, scan.InFlight
		}
		if err != nil {
			errs = append(errs, err.Error())
		}
		st.HighestIssued = highest
		top := max(st.Last, st.HighestIssued, c.Min-1)
		if st.Remaining = c.Max - top; st.Remaining < 0 {
			st.Remaining = 0
		}
		st.Error = strings.Join(errs, "; ")
		out = append(out, st)
	}
	return out
}
