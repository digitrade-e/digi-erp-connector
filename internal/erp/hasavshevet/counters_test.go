package hasavshevet

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

func TestCounterFile_FailsClosedAndNeverMovesBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "counters", "b2b_test.json")
	f := NewCounterFile(path, config.DocumentKindOrders)
	if _, err := f.Peek(); !errors.Is(err, ErrCounterMissing) {
		t.Fatalf("missing store: %v, want ErrCounterMissing", err)
	}
	if err := f.advancePast(5); !errors.Is(err, ErrCounterMissing) {
		t.Fatalf("advancePast on a missing store: %v", err)
	}
	if err := f.raiseTo(8_999_999); err != nil {
		t.Fatalf("raiseTo creates the store: %v", err)
	}
	if n, err := f.Peek(); err != nil || n != 9_000_000 {
		t.Fatalf("Peek = %d, %v; want 9000000", n, err)
	}
	if err := f.advancePast(9_000_000); err != nil {
		t.Fatal(err)
	}
	if err := f.advancePast(5); err != nil { // behind it: nothing written
		t.Fatal(err)
	}
	if n, _ := f.Peek(); n != 9_000_001 {
		t.Fatalf("Peek = %d after advancing past 9000000", n)
	}
	if err := f.raiseTo(8_999_999); !errors.Is(err, ErrCounterLower) {
		t.Fatalf("raiseTo lower: %v, want ErrCounterLower", err)
	}
	if err := f.raiseTo(9_000_000); err != nil {
		t.Fatalf("raiseTo the same number writes nothing: %v", err)
	}
	if err := f.advancePast(config.MaxDocumentNumber + 1); !errors.Is(err, ErrCounterExhausted) {
		t.Fatalf("a tenth digit is never stored: %v", err)
	}
	for _, body := range []string{`{}`, `{"last":"5"}`, `{"last":-1}`, `{"last":1.5}`, `{"last":5,"x":1}`, `{"last":5}{}`, `{"last":1000000000}`, `nope`} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Peek(); !errors.Is(err, ErrOrderCounterUnreadable) {
			t.Errorf("%s: %v, want ErrOrderCounterUnreadable", body, err)
		}
		if b, _ := os.ReadFile(path); string(b) != body {
			t.Errorf("%s: the store was changed", body)
		}
	}
	r := NewCounterFile(path, config.DocumentKindReceipts)
	if _, err := r.Peek(); !errors.Is(err, ErrStoreUnavailable) {
		t.Errorf("a damaged receipt counter is the receipt store's error: %v", err)
	}
}

func TestOrderNumberStore_UpperBoundAndRefuseMissing(t *testing.T) {
	dir := t.TempDir()
	s := NewOrderNumberStore(filepath.Join(dir, "lastOrderNumber.json"))
	if err := s.write(config.MaxDocumentNumber); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Next(); !errors.Is(err, ErrCounterExhausted) {
		t.Fatalf("Next past 999999999: %v, want ErrCounterExhausted", err)
	}
	if err := s.advancePast(config.MaxDocumentNumber + 1); !errors.Is(err, ErrCounterExhausted) {
		t.Fatalf("advancePast to ten digits: %v", err)
	}

	m := NewOrderNumberStore(filepath.Join(dir, "missing.json"))
	if n, err := m.Peek(); err != nil || n != 1 {
		t.Fatalf("today: a missing order counter starts at 1, got %d %v", n, err)
	}
	m.refuseMissing = true
	if _, err := m.Peek(); !errors.Is(err, ErrCounterMissing) {
		t.Fatalf("with counters configured a missing order counter is refused: %v", err)
	}
	if err := m.raiseTo(41); err != nil {
		t.Fatal(err)
	}
	if n, err := m.Peek(); err != nil || n != 42 {
		t.Fatalf("after raiseTo 41: %d %v", n, err)
	}
	if err := m.raiseTo(40); !errors.Is(err, ErrCounterLower) {
		t.Fatalf("raiseTo lower: %v", err)
	}
}

func TestReceiptNumberStore_RaiseToSpeaksLast(t *testing.T) {
	s := NewReceiptNumberStore(filepath.Join(t.TempDir(), "receiptNumber.json"))
	if err := s.raiseTo(499_999); err != nil { // creates it: the next receipt is 500000
		t.Fatal(err)
	}
	if n, err := s.Peek(); err != nil || n != 500_000 {
		t.Fatalf("next = %d %v", n, err)
	}
	if err := s.raiseTo(499_998); !errors.Is(err, ErrCounterLower) {
		t.Fatalf("lower: %v", err)
	}
	if err := s.raiseTo(600_000); err != nil {
		t.Fatal(err)
	}
	c := &Counter{Key: "receipts", src: s}
	if last, err := c.Last(); err != nil || last != 600_000 {
		t.Fatalf("Last = %d %v: one meaning (last issued) for both kinds", last, err)
	}
}

func countersConfig(sendDir string) config.Config {
	return config.Config{
		ERP: config.ERPHasavshevet, SendOrderDir: sendDir,
		Documents: config.DocumentsConfig{Counters: map[string]config.CounterConfig{
			"b2b":     {Kind: config.DocumentKindOrders, Min: 1_000_000, Max: 1_999_999, Store: config.CounterStoreLastOrderNumber},
			"shopify": {Kind: config.DocumentKindOrders, Min: 5_000_000, Max: 5_999_999},
		}},
	}
}

func TestNewCounterSet(t *testing.T) {
	sendDir, dataDir := t.TempDir(), t.TempDir()

	legacy := NewCounterSet(config.Config{ERP: config.ERPHasavshevet, SendOrderDir: sendDir, SendReceiptDir: t.TempDir()}, dataDir)
	o, r := legacy.DefaultOrder(), legacy.Receipts()
	if o == nil || o.Key != "orders" || !o.Implicit || o.Min != 1 || o.Max != config.MaxDocumentNumber || o.Path != filepath.Join(sendDir, "lastOrderNumber.json") {
		t.Fatalf("implicit order counter %+v", o)
	}
	if r == nil || r.Key != "receipts" || !r.Implicit || r.Path != ReceiptNumberStorePath(dataDir) {
		t.Fatalf("implicit receipt counter %+v", r)
	}
	if n, err := o.Peek(); err != nil || n != 1 {
		t.Errorf("legacy: a missing order counter still starts at 1: %d %v", n, err)
	}

	set := NewCounterSet(countersConfig(sendDir), dataDir)
	if keys := len(set.All()); keys != 2 || set.Receipts() != nil {
		t.Fatalf("configured set: %d counters, receipts %v", keys, set.Receipts())
	}
	b2b, shop := set.DefaultOrder(), mustCounter(t, set, "shopify")
	if b2b.Key != "b2b" || b2b.Implicit || set.OrderNumberStore() == nil {
		t.Fatalf("default order counter %+v", b2b)
	}
	if _, err := b2b.Peek(); !errors.Is(err, ErrCounterMissing) {
		t.Errorf("configured: a missing lastOrderNumber.json is refused, got %v", err)
	}
	if shop.Path != CounterStorePath(dataDir, "shopify") {
		t.Errorf("own store at %s", shop.Path)
	}
	if _, err := shop.next(); !errors.Is(err, ErrCounterMissing) {
		t.Errorf("own store missing: %v", err)
	}
	if err := shop.RaiseTo(4_999_999); err != nil {
		t.Fatal(err)
	}
	if n, err := shop.next(); err != nil || n != 5_000_000 {
		t.Errorf("next = %d %v", n, err)
	}
	if err := shop.RaiseTo(5_999_999); err != nil {
		t.Fatal(err)
	}
	if _, err := shop.next(); !errors.Is(err, ErrCounterExhausted) || !strings.Contains(err.Error(), "shopify") {
		t.Errorf("past max: %v", err)
	}
}

func mustCounter(t *testing.T, s *CounterSet, key string) *Counter {
	t.Helper()
	c, ok := s.Get(key)
	if !ok {
		t.Fatalf("no counter %s", key)
	}
	return c
}

func TestScanIssued_PerRange(t *testing.T) {
	sendDir, dataDir := t.TempDir(), t.TempDir()
	ix := orderIndex{dir: OrderIndexDir(dataDir)}
	if err := os.MkdirAll(ix.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, e := range []*orderIndexEntry{
		{Key: "a", Number: 1_000_010, State: orderWritten, ReservedAt: "x"},
		{Key: "b", Number: 1_000_011, State: orderRetryable, ReservedAt: "x"},
		{Key: "c", Number: 5_000_000, State: orderReserved, ReservedAt: "x",
			Rule: &storedRule{Counter: "shopify", Folder: "herp", FileName: "IMOVEIN_5000000.doc"}},
	} {
		if err := ix.save(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(sendDir, "history", "1000020"), 0o700); err != nil {
		t.Fatal(err)
	}
	b2b, err := ScanIssued(config.DocumentKindOrders, dataDir, sendDir, 1_000_000, 1_999_999)
	if err != nil || b2b.Highest != 1_000_020 || b2b.InFlight != 1 {
		t.Errorf("b2b scan %+v %v", b2b, err)
	}
	shop, err := ScanIssued(config.DocumentKindOrders, dataDir, sendDir, 5_000_000, 5_999_999)
	if err != nil || shop.Highest != 5_000_000 || shop.InFlight != 1 {
		t.Errorf("shopify scan %+v %v", shop, err)
	}
	inFlight, err := ScanOrdersInFlight(dataDir)
	if err != nil || inFlight.ByFolder["herp"] != 1 || inFlight.ByFolder["herp_test"] != 0 || inFlight.Default != 1 || inFlight.Unreadable != 0 {
		t.Errorf("in flight %+v %v", inFlight, err)
	}
}

// Review m1: a counter whose store has no absolute folder — lastOrderNumber
// adopted with sendOrderDir empty or relative, a store of its own with a
// relative data directory — never reads or writes the working folder (the
// service's is System32): it fails closed.
func TestNewCounterSet_NeverARelativeStorePath(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	for name, tc := range map[string]struct {
		sendOrderDir, dataDir, key string
	}{
		"lastOrderNumber, no sendOrderDir":       {"", t.TempDir(), "b2b"},
		"lastOrderNumber, relative sendOrderDir": {"orders", t.TempDir(), "b2b"},
		"its own store, relative data directory": {t.TempDir(), "", "shop"},
		"receiptNumber, relative data directory": {t.TempDir(), "data", "rcpt"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := config.Config{ERP: config.ERPHasavshevet, SendOrderDir: tc.sendOrderDir, Documents: config.DocumentsConfig{
				Counters: map[string]config.CounterConfig{
					"b2b":  {Kind: config.DocumentKindOrders, Min: 1, Max: 999, Store: config.CounterStoreLastOrderNumber},
					"shop": {Kind: config.DocumentKindOrders, Min: 1000, Max: 1999},
					"rcpt": {Kind: config.DocumentKindReceipts, Min: 2000, Max: 2999, Store: config.CounterStoreReceiptNumber},
				}}}
			c := mustCounter(t, NewCounterSet(cfg, tc.dataDir), tc.key)
			if c.Path != "" || c.StorePresent() {
				t.Errorf("path %q, present %v: want no path", c.Path, c.StorePresent())
			}
			if _, err := c.Peek(); !errors.Is(err, ErrCounterMissing) {
				t.Errorf("Peek = %v, want ErrCounterMissing", err)
			}
			if err := c.RaiseTo(0); err == nil {
				t.Error("RaiseTo wrote a store without a folder")
			}
			if des, _ := os.ReadDir(cwd); len(des) != 0 {
				t.Errorf("the working folder got %v", des)
			}
		})
	}
}
