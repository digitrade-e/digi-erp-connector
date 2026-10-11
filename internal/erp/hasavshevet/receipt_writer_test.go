package hasavshevet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// receiptHarness is one connector install: a herp folder the importer reads and
// a DataDir holding the number store, the index and the history.
type receiptHarness struct {
	t       *testing.T
	sendDir string
	dataDir string
	cfg     ReceiptWriterConfig
}

func newReceiptHarness(t *testing.T, seed int64) *receiptHarness {
	t.Helper()
	root := t.TempDir()
	h := &receiptHarness{
		t:       t,
		sendDir: filepath.Join(root, "herp"),
		dataDir: filepath.Join(root, "data"),
	}
	h.cfg = ReceiptWriterConfig{SendReceiptDir: h.sendDir, DataDir: h.dataDir}
	if err := os.MkdirAll(h.sendDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if seed > 0 {
		writeStoreFile(t, h.storePath(), fmt.Sprintf(`{"nextReceiptNumber": %d}`, seed))
	}
	return h
}

func (h *receiptHarness) storePath() string {
	return filepath.Join(h.dataDir, "receipts", "receiptNumber.json")
}

// writer builds a writer the way the daemon does at startup: construct, Recover.
func (h *receiptHarness) writer() *ReceiptWriter {
	h.t.Helper()
	w := h.newWriter()
	if err := w.Recover(); err != nil {
		h.t.Fatalf("Recover: %v", err)
	}
	return w
}

func (h *receiptHarness) newWriter() *ReceiptWriter {
	h.t.Helper()
	w, err := NewReceiptWriter(h.cfg, noopLogger{})
	if err != nil {
		h.t.Fatalf("NewReceiptWriter: %v", err)
	}
	return w
}

func (h *receiptHarness) nextNumber() int64 {
	h.t.Helper()
	n, err := NewReceiptNumberStore(h.storePath()).Peek()
	if err != nil {
		h.t.Fatalf("Peek: %v", err)
	}
	return n
}

// docs lists what an importer polling the herp folder would pick up.
func (h *receiptHarness) docs() []string {
	h.t.Helper()
	m, err := filepath.Glob(filepath.Join(h.sendDir, "KUPAIN_*.doc"))
	if err != nil {
		h.t.Fatalf("glob: %v", err)
	}
	sort.Strings(m)
	return m
}

func (h *receiptHarness) docPath(n int64) string {
	return filepath.Join(h.sendDir, "KUPAIN_"+strconv.FormatInt(n, 10)+".doc")
}

func (h *receiptHarness) indexFiles() []string {
	h.t.Helper()
	m, _ := filepath.Glob(filepath.Join(h.dataDir, "receipts", "index", "*.json"))
	return m
}

func testKey(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", i) }

func receiptFor(key string) ReceiptRequest {
	r := goldenReceipt()
	r.IdempotencyKey = key
	return r
}

func mustWrite(t *testing.T, w *ReceiptWriter, req ReceiptRequest) *ReceiptResult {
	t.Helper()
	res, err := w.Write(context.Background(), req)
	if err != nil {
		t.Fatalf("Write(%s): %v", req.IdempotencyKey, err)
	}
	return res
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// expectedDoc is the record the builder produces for req under number n.
func expectedDoc(t *testing.T, req ReceiptRequest, n int64) []byte {
	t.Helper()
	doc, _, err := generateKUPAINDOC(req, n, ReceiptCardNames{})
	if err != nil {
		t.Fatalf("generateKUPAINDOC: %v", err)
	}
	return doc
}

func TestReceiptWriter_FirstWrite(t *testing.T) {
	h := newReceiptHarness(t, 1000)
	w := h.writer()
	req := receiptFor(testKey(1))

	res := mustWrite(t, w, req)

	if res.ReceiptNumber != 1000 || !res.Written || res.IdempotencyKey != testKey(1) {
		t.Fatalf("result = %+v; want number 1000, written, key echoed", res)
	}
	if res.Import != ImportNotRun {
		t.Errorf("import = %q; want not_run (model (a): no receipt BAT configured)", res.Import)
	}
	if res.Replayed {
		t.Error("a first write is not a replay")
	}
	if got := readFile(t, h.docPath(1000)); !bytes.Equal(got, expectedDoc(t, req, 1000)) {
		t.Error("KUPAIN_1000.doc is not the builder's record")
	}
	if got := readFile(t, filepath.Join(h.sendDir, "KUPAIN.prm")); !bytes.Equal(got, generateKUPAINPRM()) {
		t.Error("KUPAIN.prm is not the builder's .prm")
	}
	hist := filepath.Join(h.dataDir, "history", "receipts", "1000")
	if got := readFile(t, filepath.Join(hist, "KUPAIN_1000.doc")); !bytes.Equal(got, expectedDoc(t, req, 1000)) {
		t.Error("history copy of the .doc differs")
	}
	if got := readFile(t, filepath.Join(hist, "KUPAIN_1000.prm")); !bytes.Equal(got, generateKUPAINPRM()) {
		t.Error("history copy of the .prm differs")
	}
	if n := h.nextNumber(); n != 1001 {
		t.Errorf("next number = %d; want 1001", n)
	}
	// Nothing but the receipt pair lands in the importer's folder: store, index
	// and history live in DataDir.
	entries, _ := os.ReadDir(h.sendDir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "KUPAIN.prm,KUPAIN_1000.doc" {
		t.Errorf("herp folder holds %v; want exactly KUPAIN.prm and KUPAIN_1000.doc", names)
	}
}

func TestReceiptWriter_KeepsBuilderWarnings(t *testing.T) {
	h := newReceiptHarness(t, 1)
	w := h.writer()
	req := receiptFor(testKey(1))
	req.CardIssuer, req.CardBrand = "77", "88"

	res := mustWrite(t, w, req)
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "F17") {
		t.Fatalf("warnings = %q; want the builder's card-name warning", res.Warnings)
	}
	// A replay returns the same warnings.
	again := mustWrite(t, w, req)
	if strings.Join(again.Warnings, "|") != strings.Join(res.Warnings, "|") {
		t.Errorf("replay warnings = %q; want %q", again.Warnings, res.Warnings)
	}
}

// Repeat key → same number, one file, the file untouched.
func TestReceiptWriter_RepeatKeyIsTheSameReceipt(t *testing.T) {
	h := newReceiptHarness(t, 1000)
	w := h.writer()
	req := receiptFor(testKey(1))

	first := mustWrite(t, w, req)
	// Push the mtime into the past so any rewrite is visible.
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(h.docPath(first.ReceiptNumber), old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	for i := 0; i < 3; i++ {
		again := mustWrite(t, w, req)
		if again.ReceiptNumber != first.ReceiptNumber {
			t.Fatalf("repeat %d got number %d; want %d", i, again.ReceiptNumber, first.ReceiptNumber)
		}
		if !again.Replayed || !again.Written {
			t.Errorf("repeat %d = %+v; want written and replayed", i, again)
		}
	}
	if docs := h.docs(); len(docs) != 1 {
		t.Fatalf("herp holds %v; want exactly one receipt file", docs)
	}
	info, err := os.Stat(h.docPath(first.ReceiptNumber))
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(old) {
		t.Errorf("mtime changed from %v to %v: a repeat rewrote the file", old, info.ModTime())
	}
	if n := h.nextNumber(); n != 1001 {
		t.Errorf("next number = %d; want 1001 (a repeat must not consume a number)", n)
	}
	// A fresh writer (a restart) answers the same.
	if again := mustWrite(t, h.writer(), req); again.ReceiptNumber != first.ReceiptNumber {
		t.Errorf("after restart got %d; want %d", again.ReceiptNumber, first.ReceiptNumber)
	}
}

// The caller never saw the 200: it retries and must get the same number and a
// byte-identical file, not a second receipt.
func TestReceiptWriter_DroppedResponseRetriesToTheSameBytes(t *testing.T) {
	h := newReceiptHarness(t, 42)
	req := receiptFor(testKey(7))

	first := mustWrite(t, h.writer(), req)
	docBefore := readFile(t, h.docPath(first.ReceiptNumber))
	histBefore := readFile(t, filepath.Join(h.dataDir, "history", "receipts", "42", "KUPAIN_42.doc"))

	retry := mustWrite(t, h.writer(), req)
	if retry.ReceiptNumber != 42 || retry.IdempotencyKey != first.IdempotencyKey || retry.Import != first.Import {
		t.Fatalf("retry = %+v; want the first answer %+v", retry, first)
	}
	if !bytes.Equal(readFile(t, h.docPath(42)), docBefore) {
		t.Error("the receipt file changed on retry")
	}
	if !bytes.Equal(readFile(t, filepath.Join(h.dataDir, "history", "receipts", "42", "KUPAIN_42.doc")), histBefore) {
		t.Error("the history copy changed on retry")
	}
	if docs := h.docs(); len(docs) != 1 {
		t.Errorf("herp holds %v; want one receipt", docs)
	}
}

// A repeat whose body differs is still the stored receipt; the difference is
// reported, never written.
func TestReceiptWriter_RepeatWithADifferentBodyWarnsAndKeepsTheStoredReceipt(t *testing.T) {
	h := newReceiptHarness(t, 1)
	w := h.writer()
	req := receiptFor(testKey(1))
	first := mustWrite(t, w, req)
	before := readFile(t, h.docPath(first.ReceiptNumber))

	changed := req
	changed.AmountMinor = 123456
	again := mustWrite(t, w, changed)

	if again.ReceiptNumber != first.ReceiptNumber {
		t.Fatalf("number = %d; want %d", again.ReceiptNumber, first.ReceiptNumber)
	}
	found := false
	for _, w := range again.Warnings {
		if strings.Contains(w, "differs") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %q; want one saying the request differs from the stored one", again.Warnings)
	}
	if !bytes.Equal(readFile(t, h.docPath(first.ReceiptNumber)), before) {
		t.Error("a differing repeat rewrote the receipt")
	}
}

func TestReceiptWriter_NumbersAreGapless(t *testing.T) {
	h := newReceiptHarness(t, 500)
	w := h.writer()
	for i := 0; i < 5; i++ {
		res := mustWrite(t, w, receiptFor(testKey(i)))
		if want := int64(500 + i); res.ReceiptNumber != want {
			t.Fatalf("receipt %d got %d; want %d", i, res.ReceiptNumber, want)
		}
	}
}

// Invalid input is refused before a number is taken: the store, the index and
// the herp folder are exactly as they were.
func TestReceiptWriter_InvalidInputBurnsNoNumber(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(*ReceiptRequest)
		names ReceiptCardNames
		field string // FieldError field; "" = CardNamesConfigError
	}{
		{name: "bad last4", edit: func(r *ReceiptRequest) { r.Last4 = "12a4" }, field: "last4"},
		{name: "missing account name", edit: func(r *ReceiptRequest) { r.Account.Name = "" }, field: "account/name"},
		{name: "arabic", edit: func(r *ReceiptRequest) { r.Account.City = "حيفا" }, field: "account/city"},
		{name: "amount too large", edit: func(r *ReceiptRequest) { r.AmountMinor = 100000000 }, field: "amountMinor"},
		{name: "no details", edit: func(r *ReceiptRequest) { r.Details = nil }, field: "details"},
		{name: "key not a uuid", edit: func(r *ReceiptRequest) { r.IdempotencyKey = "not-a-uuid" }, field: "idempotencyKey"},
		{name: "key traversal", edit: func(r *ReceiptRequest) { r.IdempotencyKey = "../../../../etc/passwd" }, field: "idempotencyKey"},
		{name: "key missing", edit: func(r *ReceiptRequest) { r.IdempotencyKey = "" }, field: "idempotencyKey"},
		{name: "key with braces", edit: func(r *ReceiptRequest) { r.IdempotencyKey = "{" + testKey(1) + "}" }, field: "idempotencyKey"},
		{
			name:  "unwritable card-name override",
			edit:  func(r *ReceiptRequest) {},
			names: ReceiptCardNames{Issuer: map[string]string{"2": strings.Repeat("א", 26)}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newReceiptHarness(t, 1000)
			h.cfg.CardNames = tc.names
			w := h.writer()
			req := receiptFor(testKey(1))
			tc.edit(&req)

			_, err := w.Write(context.Background(), req)
			if tc.field != "" {
				wantFieldError(t, err, tc.field)
			} else {
				var ce *CardNamesConfigError
				if !errors.As(err, &ce) {
					t.Fatalf("err = %v; want *CardNamesConfigError", err)
				}
			}
			if n := h.nextNumber(); n != 1000 {
				t.Errorf("next number = %d; want 1000 (no number burned)", n)
			}
			if idx := h.indexFiles(); len(idx) != 0 {
				t.Errorf("index holds %v; want nothing", idx)
			}
			if docs := h.docs(); len(docs) != 0 {
				t.Errorf("herp holds %v; want nothing", docs)
			}
		})
	}
}

// Missing, corrupt and negative stores refuse — no file, no index entry, and
// never receipt number 1.
func TestReceiptWriter_UnavailableStoreRefuses(t *testing.T) {
	cases := map[string]*string{
		"missing":  nil,
		"corrupt":  strPtr(`{`),
		"negative": strPtr(`{"nextReceiptNumber": -3}`),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			h := newReceiptHarness(t, 0)
			if content != nil {
				writeStoreFile(t, h.storePath(), *content)
			}
			w := h.writer() // nothing to recover: an unseeded install still starts

			res, err := w.Write(context.Background(), receiptFor(testKey(1)))
			if !errors.Is(err, ErrStoreUnavailable) {
				t.Fatalf("Write = %+v, %v; want ErrStoreUnavailable", res, err)
			}
			if docs := h.docs(); len(docs) != 0 {
				t.Errorf("herp holds %v; want nothing", docs)
			}
			if idx := h.indexFiles(); len(idx) != 0 {
				t.Errorf("index holds %v; want nothing", idx)
			}
			if content == nil {
				if _, err := os.Stat(h.storePath()); !os.IsNotExist(err) {
					t.Error("a missing store was created")
				}
			} else if got := readFile(t, h.storePath()); string(got) != *content {
				t.Errorf("store rewritten to %q", got)
			}
		})
	}
}

func strPtr(s string) *string { return &s }

// A process killed after the number is reserved but before the file is renamed
// into place: on restart, Recover finishes it, and the caller's retry answers
// the same number. Two crash points: before and after the store moved on.
func TestReceiptWriter_KillBetweenReserveAndRenameThenRecover(t *testing.T) {
	for _, point := range []string{crashAfterIndexReserved, crashAfterStoreAdvanced} {
		t.Run(point, func(t *testing.T) {
			h := newReceiptHarness(t, 1000)
			req := receiptFor(testKey(1))

			w := h.writer()
			w.crashPoint = func(p string) {
				if p == point {
					panic("killed at " + p)
				}
			}
			func() {
				defer func() {
					if r := recover(); r == nil {
						t.Fatal("the crash hook did not fire")
					}
				}()
				_, _ = w.Write(context.Background(), req)
			}()
			if docs := h.docs(); len(docs) != 0 {
				t.Fatalf("herp holds %v before recovery; the crash was before the rename", docs)
			}

			// Restart.
			w2 := h.writer()
			if docs := h.docs(); len(docs) != 1 || docs[0] != h.docPath(1000) {
				t.Fatalf("after Recover herp holds %v; want only KUPAIN_1000.doc", docs)
			}
			if !bytes.Equal(readFile(t, h.docPath(1000)), expectedDoc(t, req, 1000)) {
				t.Error("recovered file is not the builder's record")
			}
			if n := h.nextNumber(); n != 1001 {
				t.Errorf("next number after recovery = %d; want 1001", n)
			}

			retry := mustWrite(t, w2, req)
			if retry.ReceiptNumber != 1000 || !retry.Written {
				t.Fatalf("retry = %+v; want written 1000", retry)
			}
			if docs := h.docs(); len(docs) != 1 {
				t.Errorf("herp holds %v after the retry; want one file", docs)
			}
			// Gapless across the crash.
			if next := mustWrite(t, w2, receiptFor(testKey(2))); next.ReceiptNumber != 1001 {
				t.Errorf("next receipt got %d; want 1001", next.ReceiptNumber)
			}
		})
	}
}

// Recover is also what the next Write does when the daemon could not finish it
// at startup (e.g. the store was seeded while the service ran).
func TestReceiptWriter_WriteRecoversFirst(t *testing.T) {
	h := newReceiptHarness(t, 1000)
	req := receiptFor(testKey(1))
	w := h.writer()
	w.crashPoint = func(p string) {
		if p == crashAfterIndexReserved {
			panic("killed")
		}
	}
	func() {
		defer func() { _ = recover() }()
		_, _ = w.Write(context.Background(), req)
	}()

	w2 := h.newWriter() // no explicit Recover
	res := mustWrite(t, w2, receiptFor(testKey(2)))
	if res.ReceiptNumber != 1001 {
		t.Fatalf("a fresh key got %d; want 1001 — 1000 is held by the interrupted receipt", res.ReceiptNumber)
	}
	if docs := h.docs(); len(docs) != 2 {
		t.Errorf("herp holds %v; want 1000 recovered and 1001", docs)
	}
}

func TestReceiptWriter_CorruptIndexFailsClosed(t *testing.T) {
	h := newReceiptHarness(t, 1000)
	w := h.writer()
	mustWrite(t, w, receiptFor(testKey(1)))
	idx := filepath.Join(h.dataDir, "receipts", "index", testKey(1)+".json")
	if err := os.WriteFile(idx, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := w.Write(context.Background(), receiptFor(testKey(1))); err == nil {
		t.Fatal("a corrupt index entry was answered; it must never be re-reserved")
	}
	if n := h.nextNumber(); n != 1001 {
		t.Errorf("next number = %d; want 1001", n)
	}
	if err := h.newWriter().Recover(); err == nil {
		t.Error("Recover over a corrupt index succeeded")
	}
}

// What an importer polling the folder sees is only ever a whole record.
func TestReceiptWriter_PollersNeverSeeAShortFile(t *testing.T) {
	h := newReceiptHarness(t, 1)
	w := h.writer()
	want := len(expectedDoc(t, receiptFor(testKey(0)), 1))

	stop := make(chan struct{})
	var seen, bad atomic.Int64
	var badMsg atomic.Value
	var wg sync.WaitGroup
	for p := 0; p < 3; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				matches, _ := filepath.Glob(filepath.Join(h.sendDir, "KUPAIN_*.doc"))
				for _, m := range matches {
					if strings.Contains(filepath.Base(m), ".tmp-") {
						bad.Add(1)
						badMsg.Store("poller glob matched a temp file " + m)
					}
					b, err := os.ReadFile(m)
					if err != nil {
						continue // already gone is fine; short is not
					}
					seen.Add(1)
					if len(b) != want {
						bad.Add(1)
						badMsg.Store(fmt.Sprintf("%s was %d bytes, want %d", m, len(b), want))
					}
				}
			}
		}()
	}
	for i := 0; i < 40; i++ {
		mustWrite(t, w, receiptFor(testKey(i)))
	}
	close(stop)
	wg.Wait()

	if bad.Load() != 0 {
		t.Fatalf("%d bad observations; last: %v", bad.Load(), badMsg.Load())
	}
	if seen.Load() == 0 {
		t.Fatal("the pollers never saw a file; the test proved nothing")
	}
}

// Model (b): the BAT runs after the receipt is written. A failure leaves the
// receipt written with import failed, and a repeat never runs the BAT again.
func TestReceiptWriter_BatFailureIsWrittenAndNeverReRun(t *testing.T) {
	h := newReceiptHarness(t, 1000)
	h.cfg.HasReceiptBatFile = filepath.Join(h.sendDir, "kupain.bat")
	w := h.writer()
	var calls atomic.Int32
	w.runBat = func(ctx context.Context, bat string) (int, string, error) {
		calls.Add(1)
		if bat != h.cfg.HasReceiptBatFile {
			t.Errorf("ran %q; want the configured receipt BAT", bat)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("the BAT ran without a deadline")
		}
		if _, err := os.Stat(h.docPath(1000)); err != nil {
			t.Errorf("the BAT ran before the receipt file existed: %v", err)
		}
		return 1, "import refused", nil
	}
	req := receiptFor(testKey(1))

	res := mustWrite(t, w, req)
	if !res.Written || res.ReceiptNumber != 1000 || res.Import != ImportFailed {
		t.Fatalf("result = %+v; want written 1000 with import failed", res)
	}
	for i := 0; i < 2; i++ {
		again := mustWrite(t, w, req)
		if again.Import != ImportFailed || again.ReceiptNumber != 1000 {
			t.Fatalf("repeat = %+v; want 1000 / failed", again)
		}
	}
	if again := mustWrite(t, h.writer(), req); again.Import != ImportFailed {
		t.Fatalf("repeat after restart import = %q; want failed", again.Import)
	}
	if c := calls.Load(); c != 1 {
		t.Errorf("the BAT ran %d times; want exactly once", c)
	}
}

func TestReceiptWriter_BatSuccess(t *testing.T) {
	h := newReceiptHarness(t, 1)
	h.cfg.HasReceiptBatFile = filepath.Join(h.sendDir, "kupain.bat")
	w := h.writer()
	w.runBat = func(context.Context, string) (int, string, error) { return 0, "ok", nil }

	if res := mustWrite(t, w, receiptFor(testKey(1))); res.Import != ImportOK {
		t.Fatalf("import = %q; want ok", res.Import)
	}
}

// A crash while the BAT runs leaves import "started"; the import may or may not
// have happened, so neither Recover nor a repeat may run the BAT again.
func TestReceiptWriter_ImportStartedIsNeverReRun(t *testing.T) {
	h := newReceiptHarness(t, 1)
	h.cfg.HasReceiptBatFile = filepath.Join(h.sendDir, "kupain.bat")
	req := receiptFor(testKey(1))

	w := h.writer()
	w.runBat = func(context.Context, string) (int, string, error) { panic("killed during import") }
	func() {
		defer func() { _ = recover() }()
		_, _ = w.Write(context.Background(), req)
	}()

	w2 := h.writer()
	var calls atomic.Int32
	w2.runBat = func(context.Context, string) (int, string, error) { calls.Add(1); return 0, "", nil }
	res := mustWrite(t, w2, req)
	if res.Import != ImportStarted || res.ReceiptNumber != 1 {
		t.Fatalf("result = %+v; want 1 / started", res)
	}
	if calls.Load() != 0 {
		t.Error("the BAT was re-run for a key whose import had started")
	}
}

// The receipt and the order worker share one lock: while a receipt is being
// written and imported, no order job runs, and the order goes through after.
func TestReceiptWriter_OrderJobAndReceiptAreSerialised(t *testing.T) {
	h := newReceiptHarness(t, 1)
	h.cfg.HasReceiptBatFile = filepath.Join(h.sendDir, "kupain.bat")
	w := h.writer()
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseBat := func() { releaseOnce.Do(func() { close(release) }) }
	// A failing assertion must not leave the BAT holding the process-wide
	// import lock: every later test would hang on it.
	t.Cleanup(releaseBat)
	w.runBat = func(context.Context, string) (int, string, error) {
		close(entered)
		<-release
		return 0, "", nil
	}

	orderDir := t.TempDir()
	sender := NewSender(nil, config.Config{SendOrderDir: orderDir},
		NewOrderNumberStore(filepath.Join(orderDir, "lastOrderNumber.json")), noopLogger{})
	queue := NewOrderQueue(sender, noopLogger{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queue.Start(ctx)

	done := make(chan error, 1)
	go func() {
		_, err := w.Write(context.Background(), receiptFor(testKey(1)))
		done <- err
	}()
	<-entered

	jobID, err := queue.Submit(OrderRequest{
		DocumentType: "ORDER", UserExtID: "1234", DueDate: "2026-10-11", CreatedDate: "2026-10-11",
		HistoryID: "H-1", Currency: "ILS", Total: 50,
		Account: &AccountDetails{AccountKey: "1234", FullName: "Test"},
		Details: []OrderLineItem{{Title: "Widget", SKU: "1001", Quantity: 1, SinglePrice: 50, TotalPrice: 50}},
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	imovein := filepath.Join(orderDir, "IMOVEIN.doc")
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(imovein); err == nil {
		t.Fatal("the order job wrote IMOVEIN.doc while the receipt import held the lock")
	}
	if st, _ := queue.Status(jobID); st.Status == JobStatusDone {
		t.Fatal("the order finished while the receipt held the lock")
	}

	releaseBat()
	if err := <-done; err != nil {
		t.Fatalf("Write: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if st, _ := queue.Status(jobID); st.Status == JobStatusDone {
			break
		}
		if time.Now().After(deadline) {
			st, _ := queue.Status(jobID)
			t.Fatalf("the order did not finish after the receipt released the lock: %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(imovein); err != nil {
		t.Errorf("IMOVEIN.doc missing after the order ran: %v", err)
	}
}

// Waiting for the lock honours the caller's context, and a receipt that never
// got the lock reserved nothing.
func TestReceiptWriter_GivesUpWaitingForTheLockWithoutReserving(t *testing.T) {
	h := newReceiptHarness(t, 1)
	w := h.writer()
	if err := lockImport(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := w.Write(ctx, receiptFor(testKey(1)))
	unlockImport()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Write = %v; want the context's deadline", err)
	}
	if n := h.nextNumber(); n != 1 {
		t.Errorf("next number = %d; want 1", n)
	}
	if idx := h.indexFiles(); len(idx) != 0 {
		t.Errorf("index holds %v; want nothing", idx)
	}
}

func TestNewReceiptWriter_RequiresBothDirectories(t *testing.T) {
	if _, err := NewReceiptWriter(ReceiptWriterConfig{DataDir: t.TempDir()}, noopLogger{}); err == nil {
		t.Error("accepted an empty sendReceiptDir")
	}
	if _, err := NewReceiptWriter(ReceiptWriterConfig{SendReceiptDir: t.TempDir()}, noopLogger{}); err == nil {
		t.Error("accepted an empty DataDir")
	}
}

func TestCanonicalReceiptKey(t *testing.T) {
	good := map[string]string{
		"0b8f6a4e-3c1d-4e2f-9a7b-5c6d7e8f9a0b": "0b8f6a4e-3c1d-4e2f-9a7b-5c6d7e8f9a0b",
		"0B8F6A4E-3C1D-4E2F-9A7B-5C6D7E8F9A0B": "0b8f6a4e-3c1d-4e2f-9a7b-5c6d7e8f9a0b",
	}
	for in, want := range good {
		if got, ok := canonicalReceiptKey(in); !ok || got != want {
			t.Errorf("canonicalReceiptKey(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{
		"", "0b8f6a4e3c1d4e2f9a7b5c6d7e8f9a0b", " 0b8f6a4e-3c1d-4e2f-9a7b-5c6d7e8f9a0b",
		"0b8f6a4e-3c1d-4e2f-9a7b-5c6d7e8f9a0g", "0b8f6a4e-3c1d-4e2f-9a7b_5c6d7e8f9a0b",
		"0b8f6a4e-3c1d-4e2f-9a7b-5c6d7e8f9a0b0", `..\..\..\windows\x-xxxx-xxxx-xxxx-xxxxxxx`,
	} {
		if got, ok := canonicalReceiptKey(bad); ok {
			t.Errorf("canonicalReceiptKey(%q) accepted as %q", bad, got)
		}
	}
}
