package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/filelock"
)

// Plan 087: the counters command.

func i64(v int64) *int64 { return &v }

func countersIn(op, key string, f func(*CountersInput)) CountersInput {
	in := CountersInput{Version: 1, ChangeSet: "cs-counters-" + strings.ReplaceAll(op, "-", "") + "-01", Op: op, Key: key}
	if f != nil {
		f(&in)
	}
	return in
}

func writeOrderCounter(t *testing.T, sendOrderDir string, last int64) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(sendOrderDir, orderNumberFile), []byte(`{"lastOrderNumber": `+itoa64(last)+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func itoa64(n int64) string { b, _ := json.Marshal(n); return string(b) }

func counterLastOn(t *testing.T, dir string, cfg config.Config, key string) int64 {
	t.Helper()
	c, ok := hasavshevet.NewCounterSet(cfg, dir).Get(key)
	if !ok {
		t.Fatalf("no counter %s", key)
	}
	last, err := c.Last()
	if err != nil {
		t.Fatalf("counter %s: %v", key, err)
	}
	return last
}

// BFL's cutover: today's order counter adopted as b2b with a range, then a
// test counter of its own beside it; ranges disjoint.
func TestCounters_AdoptTodaysOrderCounterThenATestCounter(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	env := testEnv(sys, fdb)
	writeOrderCounter(t, orders, 1_000_295)

	// A second counter before today's is adopted is refused by the daemon's
	// own check (the default order counter must be a key).
	_, err := ApplyCounters(context.Background(), env, countersIn("create", "b2b_test", func(in *CountersInput) {
		in.Kind, in.Test, in.Min, in.Max = "orders", true, i64(9_000_000), i64(9_099_999)
	}), false)
	if r := (*Refusal)(nil); !errors.As(err, &r) || r.Code != CodeConfigRefused || !strings.Contains(r.Message, "adopts the order counter") {
		t.Fatalf("test counter first: %v", err)
	}

	adopt := countersIn("create", "b2b", func(in *CountersInput) {
		in.Kind, in.Store, in.Min, in.Max = "orders", config.CounterStoreLastOrderNumber, i64(1_000_000), i64(1_999_999)
	})
	chk, err := ApplyCounters(context.Background(), env, adopt, true)
	if err != nil || chk.Check == nil || chk.Check.Config != "changed: counters" || chk.Check.Counter == nil ||
		chk.Check.Counter.Last != 1_000_295 || chk.Check.Counter.SetLast != nil || !chk.Check.Restart {
		t.Fatalf("--check = %+v %+v, %v", chk.Check, chk.Check.Counter, err)
	}
	res, err := ApplyCounters(context.Background(), env, adopt, false)
	if err != nil || !res.Restarted || res.Proof == nil || !res.PreimageCaptured {
		t.Fatalf("adopt: %+v %v", res, err)
	}
	cfg, _ := readConfig(t, sys.dir)
	if got := cfg.Documents.Counters["b2b"]; got.Store != config.CounterStoreLastOrderNumber || got.Min != 1_000_000 {
		t.Fatalf("config %+v", cfg.Documents.Counters)
	}
	if counterLastOn(t, sys.dir, cfg, "b2b") != 1_000_295 {
		t.Fatal("adopting moved the order counter")
	}

	test := countersIn("create", "b2b_test", func(in *CountersInput) {
		in.ChangeSet = "cs-counters-test-01"
		in.Kind, in.Test, in.Min, in.Max = "orders", true, i64(9_000_000), i64(9_099_999)
	})
	if _, err := ApplyCounters(context.Background(), env, test, false); err != nil {
		t.Fatalf("test counter: %v", err)
	}
	cfg, _ = readConfig(t, sys.dir)
	if counterLastOn(t, sys.dir, cfg, "b2b_test") != 8_999_999 {
		t.Fatal("a new counter's store starts below its min")
	}
	if !slices.Contains(sys.calls, "stop") || !slices.Contains(sys.calls, "start") {
		t.Errorf("calls %v: the store is written with the service stopped", sys.calls)
	}

	// Overlap refused, whatever the kind.
	_, err = ApplyCounters(context.Background(), env, countersIn("create", "receipts", func(in *CountersInput) {
		in.Kind, in.Min, in.Max = "receipts", i64(1_500_000), i64(1_600_000)
	}), false)
	if r := (*Refusal)(nil); !errors.As(err, &r) || r.Code != CodeConfigRefused || !strings.Contains(r.Message, "overlap") {
		t.Fatalf("overlap: %v", err)
	}

	// Restore puts config.yaml back, never the store.
	if _, err := Restore(context.Background(), env, RestoreInput{ChangeSet: "cs-counters-test-01"}); err != nil {
		t.Fatal(err)
	}
	cfg, _ = readConfig(t, sys.dir)
	if _, ok := cfg.Documents.Counters["b2b_test"]; ok {
		t.Error("restore kept the counter")
	}
	if _, err := os.Stat(hasavshevet.CounterStorePath(sys.dir, "b2b_test")); err != nil {
		t.Errorf("restore removed the store: %v", err)
	}
}

func TestCounters_CreateNeverStartsAtOrBelowIssued(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	env := testEnv(sys, fdb)
	writeOrderCounter(t, orders, 1_000_295)
	if _, err := ApplyCounters(context.Background(), env, countersIn("create", "b2b", func(in *CountersInput) {
		in.Kind, in.Store, in.Min, in.Max = "orders", config.CounterStoreLastOrderNumber, i64(1_000_000), i64(1_999_999)
	}), false); err != nil {
		t.Fatal(err)
	}
	// An order of an old series sits in the new counter's range.
	if err := os.MkdirAll(filepath.Join(orders, "history", "5000042"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		from *int64
		code string
	}{
		{"from at an issued number", i64(5_000_042), CodeCounterLower},
		{"from below min", i64(4_999_000), CodeCounterLower},
		{"from above max", i64(6_000_000), CodeCounterRange},
	} {
		_, err := ApplyCounters(context.Background(), env, countersIn("create", "shopify", func(in *CountersInput) {
			in.Kind, in.Min, in.Max, in.From = "orders", i64(5_000_000), i64(5_999_999), tc.from
		}), true)
		if r := (*Refusal)(nil); !errors.As(err, &r) || r.Code != tc.code {
			t.Errorf("%s: %v, want %s", tc.name, err, tc.code)
		}
	}
	res, err := ApplyCounters(context.Background(), env, countersIn("create", "shopify", func(in *CountersInput) {
		in.Kind, in.Min, in.Max = "orders", i64(5_000_000), i64(5_999_999)
	}), false)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := readConfig(t, sys.dir)
	if counterLastOn(t, sys.dir, cfg, "shopify") != 5_000_042 {
		t.Fatalf("the default start is above the issued number: %+v", res.Steps)
	}
}

func TestCounters_StartFrom(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	env := testEnv(sys, fdb)
	writeOrderCounter(t, orders, 1_000_295)
	if _, err := ApplyCounters(context.Background(), env, countersIn("create", "b2b", func(in *CountersInput) {
		in.Kind, in.Store, in.Min, in.Max = "orders", config.CounterStoreLastOrderNumber, i64(1_000_000), i64(1_999_999)
	}), false); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(orders, "history", "1000400"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, cfgBytes := readConfig(t, sys.dir)
	refusals := []struct {
		name       string
		from, last int64
		code       string
	}{
		{"moved", 1_000_500, 1_000_294, CodeCounterMoved},
		{"lower", 1_000_200, 1_000_295, CodeCounterLower},
		{"the same", 1_000_295, 1_000_295, CodeCounterLower},
		{"at an issued number", 1_000_400, 1_000_295, CodeCounterLower},
		{"above max", 2_000_000, 1_000_295, CodeCounterRange},
	}
	for _, tc := range refusals {
		_, err := ApplyCounters(context.Background(), env, countersIn("start-from", "b2b", func(in *CountersInput) {
			in.From, in.ExpectLast = i64(tc.from), i64(tc.last)
		}), false)
		if r := (*Refusal)(nil); !errors.As(err, &r) || r.Code != tc.code {
			t.Errorf("%s: %v, want %s", tc.name, err, tc.code)
		}
	}
	if counterLastOn(t, sys.dir, cfg, "b2b") != 1_000_295 {
		t.Fatal("a refusal moved the counter")
	}

	in := countersIn("start-from", "b2b", func(in *CountersInput) { in.From, in.ExpectLast = i64(1_000_500), i64(1_000_295) })
	chk, err := ApplyCounters(context.Background(), env, in, true)
	if err != nil || chk.Check == nil || chk.Check.Counter == nil || *chk.Check.Counter.SetLast != 1_000_499 || chk.Check.Counter.HighestIssued != 1_000_400 {
		t.Fatalf("--check %+v %v", chk.Check, err)
	}
	sys.calls = nil
	res, err := ApplyCounters(context.Background(), env, in, false)
	if err != nil || !res.Restarted || res.Proof == nil {
		t.Fatalf("start-from: %+v %v", res, err)
	}
	if counterLastOn(t, sys.dir, cfg, "b2b") != 1_000_499 {
		t.Fatal("not moved")
	}
	if got := strings.Join(sys.calls, " "); !strings.Contains(got, "stop") || strings.Index(got, "stop") > strings.Index(got, "start") {
		t.Errorf("calls %q: stopped, set, started", got)
	}
	if res.PreimageCaptured {
		t.Error("a counter value is never in a pre-image")
	}
	if _, b := readConfig(t, sys.dir); !bytes.Equal(b, cfgBytes) {
		t.Error("start-from changed config.yaml")
	}
	// The queue holds an order: refused before anything is stopped.
	sys.daemon.set(func(d *fakeDaemon) { d.queued = 1 })
	sys.calls = nil
	_, err = ApplyCounters(context.Background(), env, countersIn("start-from", "b2b", func(in *CountersInput) {
		in.From, in.ExpectLast = i64(1_000_600), i64(1_000_499)
	}), false)
	if r := (*Refusal)(nil); !errors.As(err, &r) || r.Code != CodeOrdersPending || slices.Contains(sys.calls, "stop") {
		t.Fatalf("queued: %v %v", err, sys.calls)
	}
}

// A receipt counter is set only while no process holds the receipt data.
func TestCounters_StartFromReceiptCounterNeedsTheWriterLock(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	env := testEnv(sys, fdb)
	cfg, _ := readConfig(t, sys.dir)
	cfg.SendReceiptDir = mkdir(t, "kupain")
	cfg.Documents.Counters = map[string]config.CounterConfig{
		"b2b":      {Kind: "orders", Min: 1, Max: 999_999, Store: config.CounterStoreLastOrderNumber},
		"receipts": {Kind: "receipts", Min: 500_000, Max: 599_999, Store: config.CounterStoreReceiptNumber},
	}
	writeConfig(t, sys.dir, cfg)
	if err := hasavshevet.NewReceiptNumberStore(hasavshevet.ReceiptNumberStorePath(sys.dir)).Seed(500_010); err != nil {
		t.Fatal(err)
	}
	sys.svc.State = "stopped"
	lockPath := filepath.Join(sys.dir, "receipts", "writer.lock")
	held, err := filelock.Lock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	in := countersIn("start-from", "receipts", func(in *CountersInput) { in.From, in.ExpectLast = i64(500_100), i64(500_009) })
	if _, err := ApplyCounters(context.Background(), env, in, false); err == nil || !strings.Contains(err.Error(), "receipt data") {
		t.Fatalf("with the writer's lock held: %v", err)
	}
	_ = held.Unlock()
	if _, err := ApplyCounters(context.Background(), env, in, false); err != nil {
		t.Fatal(err)
	}
	if n, _ := hasavshevet.NewReceiptNumberStore(hasavshevet.ReceiptNumberStorePath(sys.dir)).Peek(); n != 500_100 {
		t.Fatalf("the next receipt is %d, want 500100", n)
	}
	if counterLastOn(t, sys.dir, cfg, "receipts") != 500_099 {
		t.Fatal("last is one meaning: the last issued")
	}
}

func TestCounters_RangeAndRetire(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	env := testEnv(sys, fdb)
	writeOrderCounter(t, orders, 1_000_295)
	for _, in := range []CountersInput{
		countersIn("create", "b2b", func(in *CountersInput) {
			in.Kind, in.Store, in.Min, in.Max = "orders", config.CounterStoreLastOrderNumber, i64(1_000_000), i64(1_999_999)
		}),
		countersIn("create", "b2b_test", func(in *CountersInput) {
			in.ChangeSet = "cs-counters-test-01"
			in.Kind, in.Test, in.Min, in.Max = "orders", true, i64(9_000_000), i64(9_099_999)
		}),
	} {
		if _, err := ApplyCounters(context.Background(), env, in, false); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name     string
		min, max int64
		code     string
	}{
		{"excludes last from above", 1_000_400, 1_999_999, CodeCounterRange},
		{"excludes last from below", 1_000_000, 1_000_200, CodeCounterRange},
		{"overlaps the test counter", 1_000_000, 9_000_000, CodeConfigRefused},
	} {
		_, err := ApplyCounters(context.Background(), env, countersIn("range", "b2b", func(in *CountersInput) { in.Min, in.Max = i64(tc.min), i64(tc.max) }), false)
		if r := (*Refusal)(nil); !errors.As(err, &r) || r.Code != tc.code {
			t.Errorf("%s: %v, want %s", tc.name, err, tc.code)
		}
	}
	if _, err := ApplyCounters(context.Background(), env, countersIn("range", "b2b", func(in *CountersInput) { in.Min, in.Max = i64(1_000_100), i64(2_999_999) }), false); err != nil {
		t.Fatalf("widen: %v", err)
	}
	if _, err := ApplyCounters(context.Background(), env, countersIn("retire", "b2b", nil), false); err == nil || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("retiring the default order counter: %v", err)
	}
	// An order of b2b_test waits for a resend: not retired.
	ix := filepath.Join(sys.dir, "orders", "index")
	if err := os.MkdirAll(ix, 0o700); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(ix, "x.json")
	if err := os.WriteFile(entry, []byte(`{"key":"k","number":9000000,"state":"retryable","reservedAt":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ApplyCounters(context.Background(), env, countersIn("retire", "b2b_test", nil), false)
	if r := (*Refusal)(nil); !errors.As(err, &r) || r.Code != CodeCounterInFlight {
		t.Fatalf("retire with one in flight: %v", err)
	}
	_ = os.Remove(entry)
	if _, err := ApplyCounters(context.Background(), env, countersIn("retire", "b2b_test", nil), false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := readConfig(t, sys.dir)
	if !cfg.Documents.Counters["b2b_test"].Retired {
		t.Fatal("not retired")
	}
	for _, in := range []CountersInput{
		countersIn("retire", "b2b_test", nil),
		countersIn("range", "b2b_test", func(in *CountersInput) { in.Min, in.Max = i64(9_000_000), i64(9_199_999) }),
		countersIn("start-from", "b2b_test", func(in *CountersInput) { in.From, in.ExpectLast = i64(9_000_100), i64(8_999_999) }),
	} {
		_, err := ApplyCounters(context.Background(), env, in, false)
		if r := (*Refusal)(nil); !errors.As(err, &r) || r.Code != CodeCounterRetired {
			t.Errorf("%s on a retired counter: %v", in.Op, err)
		}
	}
}

func TestCounters_InputValidation(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	env := testEnv(sys, fdb)
	for name, in := range map[string]CountersInput{
		"bad key":               countersIn("retire", "B2B", nil),
		"key with a slash":      countersIn("retire", "herp/x", nil),
		"unknown op":            countersIn("reset", "b2b", nil),
		"create without kind":   countersIn("create", "b2b", func(in *CountersInput) { in.Min, in.Max = i64(1), i64(2) }),
		"create without range":  countersIn("create", "b2b", func(in *CountersInput) { in.Kind = "orders" }),
		"create with expect":    countersIn("create", "b2b", func(in *CountersInput) { in.Kind, in.Min, in.Max, in.ExpectLast = "orders", i64(1), i64(2), i64(0) }),
		"max ten digits":        countersIn("range", "b2b", func(in *CountersInput) { in.Min, in.Max = i64(1), i64(1_000_000_000) }),
		"min above max":         countersIn("range", "b2b", func(in *CountersInput) { in.Min, in.Max = i64(5), i64(4) }),
		"range with kind":       countersIn("range", "b2b", func(in *CountersInput) { in.Min, in.Max, in.Kind = i64(1), i64(2), "orders" }),
		"start-from no expect":  countersIn("start-from", "b2b", func(in *CountersInput) { in.From = i64(5) }),
		"start-from with range": countersIn("start-from", "b2b", func(in *CountersInput) { in.From, in.ExpectLast, in.Max = i64(5), i64(4), i64(9) }),
		"retire with from":      countersIn("retire", "b2b", func(in *CountersInput) { in.From = i64(5) }),
		"bad changeSet":         {Version: 1, ChangeSet: "x", Op: "retire", Key: "b2b"},
	} {
		_, err := ApplyCounters(context.Background(), env, in, false)
		if r := (*Refusal)(nil); !errors.As(err, &r) || (r.Code != CodeSpecInvalid && r.Code != CodeChangeSetID) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The command line: --check or nothing, then --spec -.
	if _, code := Run(context.Background(), env, []string{"counters"}, strings.NewReader("{}")); code != ExitUsage {
		t.Error("counters without --spec - is a usage error")
	}
	out, code := Run(context.Background(), env, []string{"counters", "--spec", "-"}, strings.NewReader(`{"version":1,"changeSet":"cs-counters-01","op":"retire","key":"b2b","x":1}`))
	if code != ExitRefused || !strings.Contains(string(out), CodeSpecInvalid) {
		t.Errorf("unknown field: %d %s", code, out)
	}
}

// BFL writes orders and receipts: today's two stores are adopted one after
// the other, in either order, then the test counters beside them.
func TestCounters_AdoptBothStoresOnAnOrdersAndReceiptsBox(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	env := testEnv(sys, fdb)
	cfg, _ := readConfig(t, sys.dir)
	cfg.SendReceiptDir = mkdir(t, "kupain")
	writeConfig(t, sys.dir, cfg)
	writeOrderCounter(t, orders, 1_000_295)
	if err := hasavshevet.NewReceiptNumberStore(hasavshevet.ReceiptNumberStorePath(sys.dir)).Seed(500_010); err != nil {
		t.Fatal(err)
	}
	steps := []CountersInput{
		countersIn("create", "receipts", func(in *CountersInput) {
			in.ChangeSet = "cs-counters-r-01"
			in.Kind, in.Store, in.Min, in.Max = "receipts", config.CounterStoreReceiptNumber, i64(500_000), i64(599_999)
		}),
		countersIn("create", "b2b", func(in *CountersInput) {
			in.ChangeSet = "cs-counters-o-01"
			in.Kind, in.Store, in.Min, in.Max = "orders", config.CounterStoreLastOrderNumber, i64(1_000_000), i64(1_999_999)
		}),
		countersIn("create", "receipts_test", func(in *CountersInput) {
			in.ChangeSet = "cs-counters-rt-01"
			in.Kind, in.Test, in.Min, in.Max = "receipts", true, i64(9_100_000), i64(9_199_999)
		}),
	}
	for _, in := range steps {
		if _, err := ApplyCounters(context.Background(), env, in, false); err != nil {
			t.Fatalf("%s: %v", in.Key, err)
		}
	}
	cfg, _ = readConfig(t, sys.dir)
	if counterLastOn(t, sys.dir, cfg, "receipts") != 500_009 || counterLastOn(t, sys.dir, cfg, "b2b") != 1_000_295 {
		t.Fatal("adopting moved a store")
	}
	if cfg.ReceiptCounterKey() != "receipts" || cfg.DefaultOrderCounterKey() != "b2b" {
		t.Errorf("keys %q %q", cfg.ReceiptCounterKey(), cfg.DefaultOrderCounterKey())
	}
}

// Review m1: adopting lastOrderNumber on a box that writes no orders is
// refused; before, create passed CheckConfig and its stopped-service step
// wrote {"lastOrderNumber": 0} into the working folder (System32 for the
// service).
func TestCounters_AdoptingLastOrderNumberNeedsOrders(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	env := testEnv(sys, fdb)
	cfg, _ := readConfig(t, sys.dir)
	cfg.SendOrderDir = ""
	writeConfig(t, sys.dir, cfg)
	cwd := t.TempDir()
	t.Chdir(cwd)
	_, err := ApplyCounters(context.Background(), env, countersIn("create", "b2b", func(in *CountersInput) {
		in.Kind, in.Store, in.Min, in.Max = "orders", config.CounterStoreLastOrderNumber, i64(1_000_000), i64(1_999_999)
	}), false)
	if r := wantRefusal(t, err, CodeCounterStore); !strings.Contains(r.Message, "writes no orders") {
		t.Errorf("refusal %+v", r)
	}
	if des, _ := os.ReadDir(cwd); len(des) != 0 {
		t.Fatalf("the working folder got %v", des)
	}
	// A store of its own is fine on such a box.
	if _, err := ApplyCounters(context.Background(), env, countersIn("create", "shop", func(in *CountersInput) {
		in.ChangeSet = "cs-counters-shop-01"
		in.Kind, in.Min, in.Max = "orders", i64(5_000_000), i64(5_999_999)
	}), false); err != nil {
		t.Fatalf("a store of its own: %v", err)
	}
}

// Review m2: the counters and documents commands refuse a device-name key.
func TestCountersAndDocuments_RefuseDeviceNameKeys(t *testing.T) {
	sys, env, _ := countersBox(t)
	_, err := ApplyCounters(context.Background(), env, countersIn("create", "aux", func(in *CountersInput) {
		in.Kind, in.Min, in.Max = "orders", i64(5_000_000), i64(5_999_999)
	}), false)
	if r := wantRefusal(t, err, CodeSpecInvalid); !strings.Contains(r.Fix, "device name") {
		t.Errorf("fix %q", r.Fix)
	}
	if _, err := os.Stat(hasavshevet.CounterStorePath(sys.dir, "aux")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a store was written")
	}
	_, err = ApplyDocuments(context.Background(), env, docsIn(func(in *DocumentsInput) {
		in.Folders = map[string]*FolderInput{"com1": {Kind: "orders", Path: mkdir(t, "herp"), NamePattern: "IMOVEIN_{n}.doc", Layout: "legacy"}}
	}), false)
	wantRefusal(t, err, CodeSpecInvalid)
	_, err = ApplyDocuments(context.Background(), env, docsIn(func(in *DocumentsInput) {
		in.Receipts = &DocumentsReceiptsInput{Counter: strp("lpt1")}
	}), false)
	wantRefusal(t, err, CodeSpecInvalid)
}

// Review m4: range never leaves out an unfinished entry. A receipt reserved
// at the store's last number (the store moved past it, then complete failed)
// passed the last-number check with min = last+1, and outside the range the
// writer answers that key 503 and Recover skips it.
func TestCounters_RangeKeepsWhatIsInFlight(t *testing.T) {
	sys, env, orders := countersBox(t)
	cfg, _ := readConfig(t, sys.dir)
	cfg.SendReceiptDir = mkdir(t, "kupain")
	writeConfig(t, sys.dir, cfg)
	if err := hasavshevet.NewReceiptNumberStore(hasavshevet.ReceiptNumberStorePath(sys.dir)).Seed(500_051); err != nil {
		t.Fatal(err)
	}
	rix := filepath.Join(sys.dir, "receipts", "index")
	if err := os.MkdirAll(rix, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rix, "r.json"), []byte(`{"key":"r","number":500050,"state":"reserved"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ApplyCounters(context.Background(), env, countersIn("range", "receipts", func(in *CountersInput) {
		in.Min, in.Max = i64(500_051), i64(599_999)
	}), false)
	if r := wantRefusal(t, err, CodeCounterInFlight); !strings.Contains(r.Message, "receipt 500050") {
		t.Errorf("refusal %+v", r)
	}
	// Above the range too; and an order reserved (unknown) the same way.
	_, err = ApplyCounters(context.Background(), env, countersIn("range", "receipts", func(in *CountersInput) {
		in.Min, in.Max = i64(400_000), i64(500_049)
	}), false)
	if err == nil {
		t.Fatal("a range below the reserved receipt (and the store's last) was accepted")
	}
	writeOrderCounter(t, orders, 1_000_400)
	ix := hasavshevet.OrderIndexDir(sys.dir)
	if err := os.MkdirAll(ix, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ix, "o.json"), []byte(`{"key":"o","number":1000400,"state":"reserved","reservedAt":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = ApplyCounters(context.Background(), env, countersIn("range", "b2b", func(in *CountersInput) {
		in.Min, in.Max = i64(1_000_401), i64(1_999_999)
	}), false)
	if r := wantRefusal(t, err, CodeCounterInFlight); !strings.Contains(r.Message, "order 1000400") {
		t.Errorf("refusal %+v", r)
	}
	// Finished, the same range is accepted.
	_ = os.Remove(filepath.Join(rix, "r.json"))
	if _, err := ApplyCounters(context.Background(), env, countersIn("range", "receipts", func(in *CountersInput) {
		in.ChangeSet = "cs-counters-range-02"
		in.Min, in.Max = i64(500_051), i64(599_999)
	}), false); err != nil {
		t.Fatalf("nothing in flight: %v", err)
	}
}

// Review m8: an index entry the box cannot read refuses the counters command
// — its number is not counted in create's floor, range's checks or retire's
// in-flight count — unless acceptUnreadable names exactly how many a human
// checked.
func TestCounters_UnreadableIndexEntriesRefuse(t *testing.T) {
	sys, env, _ := countersBox(t)
	ix := hasavshevet.OrderIndexDir(sys.dir)
	if err := os.MkdirAll(ix, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ix, "damaged.json"), []byte("{not an entry"), 0o600); err != nil {
		t.Fatal(err)
	}
	create := func(accept *int) CountersInput {
		return countersIn("create", "shop", func(in *CountersInput) {
			in.Kind, in.Min, in.Max, in.AcceptUnreadable = "orders", i64(5_000_000), i64(5_999_999), accept
		})
	}
	n := func(v int) *int { return &v }
	_, err := ApplyCounters(context.Background(), env, create(nil), false)
	if r := wantRefusal(t, err, CodeIndexUnreadable); !strings.Contains(r.Fix, "acceptUnreadable: 1") {
		t.Errorf("refusal %+v", r)
	}
	_, err = ApplyCounters(context.Background(), env, create(n(2)), false)
	wantRefusal(t, err, CodeIndexUnreadable)
	_, err = ApplyCounters(context.Background(), env, create(n(0)), false)
	wantRefusal(t, err, CodeSpecInvalid)
	res, err := ApplyCounters(context.Background(), env, create(n(1)), false)
	if err != nil || !warned(res, "index_unreadable:") {
		t.Fatalf("accepted by count: %v %v", res.Warnings, err)
	}
	// retire and start-from scan too.
	_, err = ApplyCounters(context.Background(), env, countersIn("retire", "b2b_test", nil), false)
	wantRefusal(t, err, CodeIndexUnreadable)
	_, err = ApplyCounters(context.Background(), env, countersIn("start-from", "b2b", func(in *CountersInput) {
		in.From, in.ExpectLast = i64(1_000_400), i64(1_000_295)
	}), false)
	wantRefusal(t, err, CodeIndexUnreadable)
}

// Review m9: restore runs the counters and documents commands' guards. It
// put config.yaml back after the idle check alone: it could drop a folder
// unfinished orders are frozen on, bring back a range below the counter's
// last number (COUNTER_EXHAUSTED for every order, no command lowers it), or
// put a kind back on today's store, whose recovery would jump into the test
// range.
func TestRestore_RunsTheCountersAndDocumentsGuards(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	env := testEnv(sys, fdb)
	writeOrderCounter(t, orders, 1_000_295)
	steps := []CountersInput{
		countersIn("create", "b2b", func(in *CountersInput) {
			in.ChangeSet = "cs-restore-c1"
			in.Kind, in.Store, in.Min, in.Max = "orders", config.CounterStoreLastOrderNumber, i64(1_000_000), i64(1_999_999)
		}),
		countersIn("range", "b2b", func(in *CountersInput) {
			in.ChangeSet = "cs-restore-c2"
			in.Min, in.Max = i64(1_000_000), i64(2_999_999)
		}),
		countersIn("create", "b2b_test", func(in *CountersInput) {
			in.ChangeSet = "cs-restore-c3"
			in.Kind, in.Test, in.Min, in.Max = "orders", true, i64(9_000_000), i64(9_099_999)
		}),
	}
	for _, in := range steps {
		if _, err := ApplyCounters(context.Background(), env, in, false); err != nil {
			t.Fatalf("%s: %v", in.ChangeSet, err)
		}
	}
	herp := mkdir(t, "herp")
	if _, err := ApplyDocuments(context.Background(), env, docsIn(func(in *DocumentsInput) {
		in.ChangeSet = "cs-restore-d1"
		in.Folders = map[string]*FolderInput{"herp": {Kind: "orders", Path: herp, NamePattern: "IMOVEIN_{n}.doc", Layout: "legacy"}}
	}), false); err != nil {
		t.Fatal(err)
	}

	// An order frozen for herp is not finished: restoring the folder away waits.
	ix := hasavshevet.OrderIndexDir(sys.dir)
	if err := os.MkdirAll(ix, 0o700); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(ix, "e.json")
	if err := os.WriteFile(entry, []byte(`{"key":"k","number":2500000,"state":"retryable","reservedAt":"x","rule":{"counter":"b2b","folder":"herp","temporary":false,"fileName":"IMOVEIN_2500000.doc"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, beforeBytes := readConfig(t, sys.dir)
	_, err := Restore(context.Background(), env, RestoreInput{ChangeSet: "cs-restore-d1"})
	wantRefusal(t, err, CodeFolderInFlight)
	if _, b := readConfig(t, sys.dir); !bytes.Equal(b, beforeBytes) {
		t.Fatal("a refused restore changed config.yaml")
	}

	// b2b issued up to 2 500 000, and that order (no rule) waits for a resend:
	// the old range would leave it out. (cs-restore-c2's pre-image predates
	// herp too; with nothing frozen for herp that part is fine.)
	writeOrderCounter(t, orders, 2_500_000)
	if err := os.WriteFile(entry, []byte(`{"key":"k","number":2500000,"state":"retryable","reservedAt":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Restore(context.Background(), env, RestoreInput{ChangeSet: "cs-restore-c2"})
	wantRefusal(t, err, CodeCounterInFlight)
	_ = os.Remove(entry)
	_, err = Restore(context.Background(), env, RestoreInput{ChangeSet: "cs-restore-c2"})
	if r := wantRefusal(t, err, CodeCounterRange); !strings.Contains(r.Message, "last number 2500000") {
		t.Errorf("refusal %+v", r)
	}

	// The documents change goes back fine now, and so does dropping the test counter.
	for _, cs := range []string{"cs-restore-d1", "cs-restore-c3"} {
		if _, err := Restore(context.Background(), env, RestoreInput{ChangeSet: cs}); err != nil {
			t.Fatalf("restore %s: %v", cs, err)
		}
	}
	// b2b_test issued 9 000 005: with no orders counter left, today's store
	// would recover past it — into the test range.
	if err := os.MkdirAll(filepath.Join(orders, "history", "9000005"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = Restore(context.Background(), env, RestoreInput{ChangeSet: "cs-restore-c1"})
	if r := wantRefusal(t, err, CodeCounterRange); !strings.Contains(r.Message, "today's store") {
		t.Errorf("refusal %+v", r)
	}
}
