package hasavshevet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// Plan 087: named counters and documentRule in the order queue.

// countersHarness is an install with named counters and typed folders: b2b
// (today's lastOrderNumber.json), shopify and b2b_test of their own; folders
// herp (production) and herp_test.
type countersHarness struct {
	*orderHarness
	cfg config.Config
}

func newCountersHarness(t *testing.T) *countersHarness {
	t.Helper()
	h := &countersHarness{orderHarness: newOrderHarness(t)}
	root := filepath.Dir(h.sendDir)
	mk := func(name string) string {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	h.cfg = config.Config{
		ERP: config.ERPHasavshevet, SendOrderDir: h.sendDir,
		Documents: config.DocumentsConfig{
			Counters: map[string]config.CounterConfig{
				"b2b":      {Kind: config.DocumentKindOrders, Min: 1_000_000, Max: 1_999_999, Store: config.CounterStoreLastOrderNumber},
				"shopify":  {Kind: config.DocumentKindOrders, Min: 5_000_000, Max: 5_999_999},
				"b2b_test": {Kind: config.DocumentKindOrders, Min: 9_000_000, Max: 9_000_002, Test: true},
				"old":      {Kind: config.DocumentKindOrders, Min: 8_000_000, Max: 8_999_999, Retired: true},
			},
			Folders: map[string]config.FolderConfig{
				"herp": {Kind: config.DocumentKindOrders, Path: mk("herp"), NamePattern: "{prefix}IMOVEIN_{n}.doc",
					TempPrefix: "TMP_", Layout: config.OrderFormatLegacy},
				"herp_test": {Kind: config.DocumentKindOrders, Path: mk("herp_test"), NamePattern: "ORD_{n}.doc",
					Layout: config.OrderFormatOfficial, Test: true},
			},
		},
	}
	if err := h.cfg.ValidateOrders(); err != nil {
		t.Fatalf("harness config: %v", err)
	}
	writeLastOrderNumber(t, h.sendDir, 1_000_049)
	set := NewCounterSet(h.cfg, h.dataDir)
	for k, last := range map[string]int64{"shopify": 4_999_999, "b2b_test": 8_999_999, "old": 8_000_100} {
		if err := mustCounter(t, set, k).RaiseTo(last); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func writeLastOrderNumber(t *testing.T, dir string, n int64) {
	t.Helper()
	if err := NewOrderNumberStore(filepath.Join(dir, "lastOrderNumber.json")).write(n); err != nil {
		t.Fatal(err)
	}
}

// queue is a new process over the harness's folders, with the counters.
func (h *countersHarness) queue(start bool) *OrderQueue {
	h.t.Helper()
	set := NewCounterSet(h.cfg, h.dataDir)
	q := NewOrderQueue(NewSender(nil, h.cfg, set.OrderNumberStore(), h.log), h.log)
	q.UseCounters(set)
	q.UseKeyIndex(h.dataDir)
	if err := q.Recover(); err != nil {
		h.t.Fatalf("Recover: %v", err)
	}
	if start {
		h.start(q)
	}
	return q
}

func (h *countersHarness) last(key string) int64 {
	h.t.Helper()
	n, err := mustCounter(h.t, NewCounterSet(h.cfg, h.dataDir), key).Last()
	if err != nil {
		h.t.Fatal(err)
	}
	return n
}

func ruled(key, counter, folder string, temporary bool) OrderRequest {
	r := keyedOrder(key)
	r.Rule = &DocumentRule{Counter: counter, Folder: folder, Temporary: temporary}
	return r
}

// The research's scenario (DIG-1563 A, §6 B5): b2b in 1 000 000-1 999 999 and
// a second counter in 5 000 000-5 999 999. With one global "highest", the
// first shopify order set highest = 5 000 000, the next b2b Peek (1 000 050)
// was refused, and recovery moved b2b's counter into shopify's range.
func TestQueue_SecondCounterInAnotherRange(t *testing.T) {
	h := newCountersHarness(t)
	q := h.queue(false)

	shop, err := q.Accept(ruled("shop-1", "shopify", "herp", false))
	if err != nil || shop.JobID != "5000000" || shop.Rule == nil || shop.Rule.Number != 5_000_000 {
		t.Fatalf("shopify order: %+v %v", shop, err)
	}
	b2b, err := q.Accept(keyedOrder("b2b-1"))
	if err != nil || b2b.JobID != "1000050" {
		t.Fatalf("the next b2b order after a shopify one: %+v %v; want 1000050", b2b, err)
	}
	if b2b.Rule != nil {
		t.Errorf("an order without a rule echoes none: %+v", b2b.Rule)
	}
	if got := h.last("b2b"); got != 1_000_050 {
		t.Fatalf("b2b's counter is %d: pushed out of its range", got)
	}

	// A restart recovers each counter over its own range.
	q2 := h.queue(false)
	if h.last("b2b") != 1_000_050 || h.last("shopify") != 5_000_000 {
		t.Fatalf("after recovery: b2b %d, shopify %d", h.last("b2b"), h.last("shopify"))
	}
	again, err := q2.Accept(keyedOrder("b2b-2"))
	if err != nil || again.JobID != "1000051" {
		t.Fatalf("b2b after a restart: %+v %v", again, err)
	}
	s2, err := q2.Accept(ruled("shop-2", "shopify", "herp", false))
	if err != nil || s2.JobID != "5000001" {
		t.Fatalf("shopify after a restart: %+v %v", s2, err)
	}
}

// Recovery is per counter: an entry ahead of b2b's counter (a crash between
// the entry and the counter) moves b2b only, never shopify.
func TestQueue_RecoveryIsPerCounter(t *testing.T) {
	h := newCountersHarness(t)
	ix := orderIndex{dir: OrderIndexDir(h.dataDir)}
	if err := os.MkdirAll(ix.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ix.save(&orderIndexEntry{Key: "crashed", Number: 1_000_060, State: orderReserved, ReservedAt: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(h.sendDir, "history", "5000007"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.queue(false)
	if h.last("b2b") != 1_000_060 || h.last("shopify") != 5_000_007 || h.last("b2b_test") != 8_999_999 {
		t.Fatalf("recovered: b2b %d, shopify %d, b2b_test %d", h.last("b2b"), h.last("shopify"), h.last("b2b_test"))
	}
}

// A counter that cannot be read refuses its own orders only.
func TestQueue_OneBrokenCounterDoesNotStopTheOthers(t *testing.T) {
	h := newCountersHarness(t)
	if err := os.WriteFile(CounterStorePath(h.dataDir, "shopify"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	set := NewCounterSet(h.cfg, h.dataDir)
	q := NewOrderQueue(NewSender(nil, h.cfg, set.OrderNumberStore(), h.log), h.log)
	q.UseCounters(set)
	q.UseKeyIndex(h.dataDir)
	_ = q.Recover() // shopify has issued nothing yet: its store is only read when it numbers an order
	if acc, err := q.Accept(keyedOrder("b2b-1")); err != nil || acc.JobID != "1000050" {
		t.Fatalf("b2b while shopify is broken: %+v %v", acc, err)
	}
	if _, err := q.Accept(ruled("shop-1", "shopify", "herp", false)); !errors.Is(err, ErrOrderCounterUnreadable) {
		t.Fatalf("shopify order: %v", err)
	}
}

func TestQueue_RuleIsFrozenAtReservation(t *testing.T) {
	h := newCountersHarness(t)
	q := h.queue(false)
	acc, err := q.Accept(ruled("k1", "shopify", "herp", true))
	if err != nil {
		t.Fatal(err)
	}
	want := AppliedRule{Counter: "shopify", Number: 5_000_000, FileName: "TMP_IMOVEIN_5000000.doc", Folder: "herp"}
	if acc.Rule == nil || *acc.Rule != want {
		t.Fatalf("echo %+v, want %+v", acc.Rule, want)
	}
	e := h.entry("k1")
	if e.Rule == nil || e.Rule.Counter != "shopify" || e.Rule.Folder != "herp" || !e.Rule.Temporary || e.Rule.FileName != "TMP_IMOVEIN_5000000.doc" {
		t.Fatalf("frozen rule %+v", e.Rule)
	}

	for name, rep := range map[string]OrderRequest{
		"another counter":   ruled("k1", "b2b", "herp", true),
		"another folder":    ruled("k1", "shopify", "herp_test", true),
		"another temporary": ruled("k1", "shopify", "herp", false),
		"no rule":           keyedOrder("k1"),
	} {
		got, err := q.Accept(rep)
		if !errors.Is(err, ErrDocumentRuleConflict) {
			t.Errorf("%s: %+v %v, want ErrDocumentRuleConflict", name, got, err)
		}
	}
	// A rule-less order repeated with a rule is a conflict too.
	if _, err := q.Accept(keyedOrder("plain")); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Accept(ruled("plain", "b2b", "herp", false)); !errors.Is(err, ErrDocumentRuleConflict) {
		t.Errorf("rule added on a repeat: %v", err)
	}

	same, err := q.Accept(ruled("k1", "shopify", "herp", true))
	if err != nil || !same.Replay || same.Rule == nil || *same.Rule != want {
		t.Fatalf("same rule repeated: %+v %v", same, err)
	}
	if h.last("shopify") != 5_000_000 {
		t.Errorf("a repeat moved the counter: %d", h.last("shopify"))
	}
}

func TestQueue_WritesIntoTheTypedFolder(t *testing.T) {
	h := newCountersHarness(t)
	q := h.queue(true)
	acc, err := q.Accept(ruled("t1", "b2b_test", "herp_test", false))
	if err != nil || acc.JobID != "9000000" {
		t.Fatalf("%+v %v", acc, err)
	}
	r := waitForStatus(t, q, acc.JobID, JobStatusDone)
	folder := h.cfg.Documents.Folders["herp_test"].Path
	doc := filepath.Join(folder, "ORD_9000000.doc")
	if len(r.WrittenFiles) == 0 || r.WrittenFiles[0] != doc {
		t.Fatalf("written %v, want %s first", r.WrittenFiles, doc)
	}
	b, err := os.ReadFile(doc)
	if err != nil || len(b) < 2891 || len(b) == 1297 {
		t.Fatalf("herp_test's layout is official (a 2891-byte record): %d %v", len(b), err)
	}
	// History keeps the fixed name, whatever the folder's pattern.
	if _, err := os.Stat(filepath.Join(h.sendDir, "history", "9000000", "IMOVEIN_9000000.doc")); err != nil {
		t.Errorf("history copy: %v", err)
	}
	// No .prm in the folder, and nothing in the pickup or sendOrderDir.
	if _, err := os.Stat(filepath.Join(folder, "ORD_9000000.prm")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a .prm in the typed folder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.sendDir, "IMOVEIN.doc")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("model (b) file written for a ruled order: %v", err)
	}

	// herp is legacy: 1296 bytes + LF; temporary → TMP_.
	acc, err = q.Accept(ruled("t2", "b2b", "herp", true))
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, q, acc.JobID, JobStatusDone)
	b, err = os.ReadFile(filepath.Join(h.cfg.Documents.Folders["herp"].Path, "TMP_IMOVEIN_"+acc.JobID+".doc"))
	if err != nil || len(b) != 1297 {
		t.Fatalf("legacy temporary file: %d bytes, %v", len(b), err)
	}
}

// The "number taken" scan follows the folder's pattern: an imported
// ORD_<n>.doc (Masofon's .MASOFON2.BAK) fails the order for good.
func TestQueue_NumberTakenFollowsThePattern(t *testing.T) {
	h := newCountersHarness(t)
	folder := h.cfg.Documents.Folders["herp_test"].Path
	if err := os.WriteFile(filepath.Join(folder, "ord_9000000.doc.20261116.MASOFON2.BAK"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// imovein_<n> in this folder is not this folder's name: not taken.
	if err := os.WriteFile(filepath.Join(folder, "IMOVEIN_9000001.doc"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	q := h.queue(true)
	acc, err := q.Accept(ruled("t1", "b2b_test", "herp_test", false))
	if err != nil {
		t.Fatal(err)
	}
	r := waitForStatus(t, q, acc.JobID, JobStatusFailed)
	if r.Err == nil || !strings.Contains(r.Err.Error(), "already used") {
		t.Fatalf("taken number: %v", r.Err)
	}
	if h.entry("t1").State != orderFailed {
		t.Errorf("state %s, want failed (final)", h.entry("t1").State)
	}
	acc, err = q.Accept(ruled("t2", "b2b_test", "herp_test", false))
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, q, acc.JobID, JobStatusDone)
}

// A ruled order that failed before anything was written is re-queued under
// its number with the FROZEN rule, even if the repeat comes later.
func TestQueue_RequeueUsesTheFrozenRule(t *testing.T) {
	h := newCountersHarness(t)
	folder := h.cfg.Documents.Folders["herp"].Path
	if err := os.RemoveAll(folder); err != nil { // the share is not there: nothing written
		t.Fatal(err)
	}
	q := h.queue(true)
	acc, err := q.Accept(ruled("r1", "shopify", "herp", true))
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, q, acc.JobID, JobStatusFailed)
	if h.entry("r1").State != orderRetryable {
		t.Fatalf("state %s, want retryable", h.entry("r1").State)
	}
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	again, err := q.Accept(ruled("r1", "shopify", "herp", true))
	if err != nil || !again.Replay || again.JobID != acc.JobID || again.Rule == nil || again.Rule.FileName != "TMP_IMOVEIN_5000000.doc" {
		t.Fatalf("re-queue: %+v %v", again, err)
	}
	waitForStatus(t, q, acc.JobID, JobStatusDone)
	if _, err := os.Stat(filepath.Join(folder, "TMP_IMOVEIN_5000000.doc")); err != nil {
		t.Fatalf("written under the frozen name: %v", err)
	}
	if h.last("shopify") != 5_000_000 {
		t.Errorf("a re-queue took a number: %d", h.last("shopify"))
	}
}

func TestQueue_CounterExhaustedReservesNothing(t *testing.T) {
	h := newCountersHarness(t)
	set := NewCounterSet(h.cfg, h.dataDir)
	if err := mustCounter(t, set, "b2b_test").RaiseTo(9_000_002); err != nil {
		t.Fatal(err)
	}
	q := h.queue(false)
	_, err := q.Accept(ruled("x1", "b2b_test", "herp_test", false))
	if !errors.Is(err, ErrCounterExhausted) {
		t.Fatalf("Accept = %v, want ErrCounterExhausted", err)
	}
	if _, found, _ := (orderIndex{dir: OrderIndexDir(h.dataDir)}).load("x1"); found {
		t.Error("an exhausted counter reserved an entry")
	}
	if h.last("b2b_test") != 9_000_002 {
		t.Error("the counter moved")
	}
}

func TestQueue_RetiredCounterIssuesNoNewNumber(t *testing.T) {
	h := newCountersHarness(t)
	q := h.queue(false)
	if _, err := q.Accept(ruled("o1", "old", "herp", false)); !errors.Is(err, ErrCounterRetired) {
		t.Fatalf("Accept = %v, want ErrCounterRetired", err)
	}
}

func TestQueue_MissingOrderCounterIsRefusedOnceCountersAreConfigured(t *testing.T) {
	h := newCountersHarness(t)
	if err := os.Remove(filepath.Join(h.sendDir, "lastOrderNumber.json")); err != nil {
		t.Fatal(err)
	}
	set := NewCounterSet(h.cfg, h.dataDir)
	q := NewOrderQueue(NewSender(nil, h.cfg, set.OrderNumberStore(), h.log), h.log)
	q.UseCounters(set)
	q.UseKeyIndex(h.dataDir)
	_ = q.Recover()
	if _, err := q.Accept(keyedOrder("b1")); !errors.Is(err, ErrCounterMissing) {
		t.Fatalf("Accept = %v, want ErrCounterMissing (never 1 again)", err)
	}
	if _, err := os.Stat(filepath.Join(h.sendDir, "lastOrderNumber.json")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the counter was recreated")
	}
}

// Today's install (no counter configured) gets the upper bound too: the order
// number never reaches ten digits, which IMOVEIN's MyID would cut.
func TestQueue_LegacyOrderCounterStopsAtNineDigits(t *testing.T) {
	h := newOrderHarness(t)
	writeLastOrderNumber(t, h.sendDir, config.MaxDocumentNumber)
	q := h.queue(false)
	_, err := q.Accept(keyedOrder("k"))
	if !errors.Is(err, ErrCounterExhausted) {
		t.Fatalf("Accept at 999999999 = %v, want ErrCounterExhausted", err)
	}
	if _, found, _ := (orderIndex{dir: OrderIndexDir(h.dataDir)}).load("k"); found {
		t.Error("an entry was reserved for a ten-digit number")
	}
}

func TestOrderIndex_RuleIsValidatedOnRead(t *testing.T) {
	for name, r := range map[string]storedRule{
		"path in the file name": {Counter: "b2b", Folder: "herp", FileName: `..\x\IMOVEIN_1.doc`},
		"not .doc":              {Counter: "b2b", Folder: "herp", FileName: "IMOVEIN_1.bat"},
		"bad folder key":        {Counter: "b2b", Folder: "herp/x", FileName: "IMOVEIN_1.doc"},
	} {
		if err := r.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Review m3: a retryable order whose folder and counter keys are gone (a
// restore, a hand edit) is re-queued against its frozen rule — the worker
// refuses it, so it stays retryable until the folder is back — instead of a
// 400 that strands it.
func TestQueue_RetryableRepeatAfterItsFolderIsGone(t *testing.T) {
	h := newCountersHarness(t)
	folder := h.cfg.Documents.Folders["herp"].Path
	if err := os.RemoveAll(folder); err != nil { // the share is not there: nothing written
		t.Fatal(err)
	}
	q := h.queue(true)
	acc, err := q.Accept(ruled("r1", "shopify", "herp", false))
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, q, acc.JobID, JobStatusFailed)
	h.stop(q)
	if h.entry("r1").State != orderRetryable {
		t.Fatalf("state %s, want retryable", h.entry("r1").State)
	}

	// The service restarts on a config without herp and shopify.
	cfg := h.cfg
	cfg.Documents = h.cfg.Documents.Clone()
	delete(cfg.Documents.Folders, "herp")
	delete(cfg.Documents.Counters, "shopify")
	h.cfg = cfg
	q = h.queue(true)
	again, err := q.Accept(ruled("r1", "shopify", "herp", false))
	if err != nil || !again.Replay || again.JobID != acc.JobID || again.Status != JobStatusQueued {
		t.Fatalf("repeat: %+v %v; want a re-queue under %s", again, err, acc.JobID)
	}
	r := waitForStatus(t, q, acc.JobID, JobStatusFailed)
	if r.Err == nil || !strings.Contains(r.Err.Error(), "no longer an orders folder") || h.entry("r1").State != orderRetryable {
		t.Fatalf("the worker: %v, state %s; want refused, still retryable", r.Err, h.entry("r1").State)
	}
	if n, err := NewCounterFile(CounterStorePath(h.dataDir, "shopify"), config.DocumentKindOrders).Peek(); err != nil || n != 5_000_001 {
		t.Errorf("shopify's store: next %d, %v — the repeat took a number", n, err)
	}
}

// Review R2: a retryable test order whose folder was made production by hand
// (past the guards of documents and restore) is not re-queued into it — nor
// written by the worker if the flag flips after the re-queue. It stays
// retryable (503 FOLDER_TEST_CHANGED) until the flag is back.
func TestQueue_FrozenTestFlags(t *testing.T) {
	h := newCountersHarness(t)
	folder := h.cfg.Documents.Folders["herp_test"].Path
	if err := os.RemoveAll(folder); err != nil { // nothing written: retryable
		t.Fatal(err)
	}
	q := h.queue(true)
	acc, err := q.Accept(ruled("t1", "b2b_test", "herp_test", false))
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, q, acc.JobID, JobStatusFailed)
	h.stop(q)
	if e := h.entry("t1"); e.State != orderRetryable || !e.Rule.CounterTest || !e.Rule.FolderTest {
		t.Fatalf("entry %+v (rule %+v): want retryable with the test flags frozen", e, e.Rule)
	}
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}

	// herp_test is made production by hand; the service restarts.
	cfg := h.cfg
	cfg.Documents = h.cfg.Documents.Clone()
	f := cfg.Documents.Folders["herp_test"]
	f.Test = false
	cfg.Documents.Folders["herp_test"] = f
	h.cfg = cfg
	q = h.queue(true)
	if _, err := q.Accept(ruled("t1", "b2b_test", "herp_test", false)); !errors.Is(err, ErrFolderTestChanged) {
		t.Fatalf("resend into a folder now production: %v, want ErrFolderTestChanged", err)
	}
	if h.entry("t1").State != orderRetryable {
		t.Errorf("the refused resend changed the entry: %s", h.entry("t1").State)
	}
	if des, _ := os.ReadDir(folder); len(des) != 0 {
		t.Fatalf("written into the folder: %v", des)
	}
	if got, err := FolderTestChanged(h.dataDir, h.cfg); err != nil || len(got) != 1 || got[0] != 9_000_000 {
		t.Errorf("FolderTestChanged = %v %v", got, err)
	}

	// The worker checks too (the flag flipped between the re-queue and the write).
	s := NewSender(nil, h.cfg, nil, h.log)
	_, err = s.processOrder(context.Background(), keyedOrder("t1"), 9_000_000,
		&orderDestination{Folder: "herp_test", FileName: "ORD_9000000.doc", CounterTest: true, FolderTest: true})
	if !errors.Is(err, ErrFolderTestChanged) || !errors.Is(err, ErrOrderNotWritten) {
		t.Fatalf("worker: %v, want FOLDER_TEST_CHANGED and not written (retryable)", err)
	}
}
