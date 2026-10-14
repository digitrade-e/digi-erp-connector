package hasavshevet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// receiptFixture is one installation: a data dir and a sendReceiptDir.
type receiptFixture struct {
	dataDir string
	outDir  string
	lock    *ImportLock
}

func newReceiptFixture(t *testing.T) receiptFixture {
	t.Helper()
	root := t.TempDir()
	f := receiptFixture{
		dataDir: filepath.Join(root, "data"),
		outDir:  filepath.Join(root, "herp", "receipts"),
		lock:    NewImportLock(),
	}
	if err := os.MkdirAll(f.outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f receiptFixture) writer() *ReceiptWriter {
	return NewReceiptWriter(ReceiptWriterConfig{
		DataDir:        f.dataDir,
		SendReceiptDir: f.outDir,
		Lock:           f.lock,
		Log:            noopLogger{},
	})
}

func (f receiptFixture) seed(t *testing.T, next int64) {
	t.Helper()
	if err := SeedReceiptNumber(f.dataDir, next); err != nil {
		t.Fatalf("SeedReceiptNumber(%d): %v", next, err)
	}
}

func (f receiptFixture) storePath() string {
	return filepath.Join(f.dataDir, "receipts", "receiptNumber.json")
}

func (f receiptFixture) setStore(t *testing.T, raw string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.storePath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.storePath(), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f receiptFixture) storeNext(t *testing.T) int64 {
	t.Helper()
	b, err := os.ReadFile(f.storePath())
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	var s struct {
		NextReceiptNumber int64 `json:"nextReceiptNumber"`
	}
	if err := jsonUnmarshalStrict(b, &s); err != nil {
		t.Fatalf("store %q: %v", b, err)
	}
	return s.NextReceiptNumber
}

func (f receiptFixture) indexPath(key string) string {
	return filepath.Join(f.dataDir, "receipts", "index", key+".json")
}

// outNames lists sendReceiptDir, which must only ever hold KUPAIN files.
func (f receiptFixture) outNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(f.outDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func receiptWithKey(key string) ReceiptRequest {
	r := goldenReceipt()
	r.IdempotencyKey = key
	return r
}

func testKey(i int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
}

func TestReceiptWriteAssignsNumberAndWritesFiles(t *testing.T) {
	f := newReceiptFixture(t)
	f.seed(t, 4217)
	w := f.writer()

	res, err := w.Write(context.Background(), goldenReceipt())
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if res.ReceiptNumber != 4217 || res.IdempotencyKey != goldenReceipt().IdempotencyKey ||
		res.Import != ReceiptImportNotRun || res.Warnings == nil || len(res.Warnings) != 0 {
		t.Fatalf("result = %+v", res)
	}

	if got := f.outNames(t); strings.Join(got, ",") != "KUPAIN.prm,KUPAIN_4217.doc" {
		t.Fatalf("sendReceiptDir holds %v, want only the KUPAIN pair", got)
	}
	assertFileBytes(t, filepath.Join(f.outDir, "KUPAIN_4217.doc"), readGolden(t, "kupain_golden.doc"))
	assertFileBytes(t, filepath.Join(f.outDir, "KUPAIN.prm"), readGolden(t, "kupain_golden.prm"))

	hist := filepath.Join(f.dataDir, "history", "receipts", "4217")
	assertFileBytes(t, filepath.Join(hist, "KUPAIN_4217.doc"), readGolden(t, "kupain_golden.doc"))
	assertFileBytes(t, filepath.Join(hist, "KUPAIN.prm"), readGolden(t, "kupain_golden.prm"))

	if next := f.storeNext(t); next != 4218 {
		t.Fatalf("store next = %d, want 4218", next)
	}
	entry, err := readReceiptIndexFile(f.indexPath(goldenReceipt().IdempotencyKey), goldenReceipt().IdempotencyKey)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if entry.State != receiptStateWritten || entry.ReceiptNumber != 4217 || entry.Request != goldenReceipt() {
		t.Fatalf("index entry = %+v", entry)
	}
}

func assertFileBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs\n got: %q\nwant: %q", path, got, want)
	}
}

// A dropped response: the caller retries the same key and must get the same
// number, with no second number spent and the file untouched.
func TestReceiptRepeatReturnsSameAnswer(t *testing.T) {
	f := newReceiptFixture(t)
	f.seed(t, 100)
	w := f.writer()

	first, err := w.Write(context.Background(), goldenReceipt())
	if err != nil {
		t.Fatal(err)
	}
	doc := filepath.Join(f.outDir, "KUPAIN_100.doc")
	before, _ := os.Stat(doc)
	beforeBytes, _ := os.ReadFile(doc)

	for i := 0; i < 3; i++ {
		again, err := w.Write(context.Background(), goldenReceipt())
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(again) != fmt.Sprint(first) {
			t.Fatalf("repeat %d = %+v, want %+v", i, again, first)
		}
	}
	// A restart in between changes nothing either.
	again, err := f.writer().Write(context.Background(), goldenReceipt())
	if err != nil || fmt.Sprint(again) != fmt.Sprint(first) {
		t.Fatalf("repeat after restart = %+v, %v", again, err)
	}

	after, _ := os.Stat(doc)
	assertFileBytes(t, doc, beforeBytes)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("a repeat rewrote KUPAIN_100.doc")
	}
	if next := f.storeNext(t); next != 101 {
		t.Fatalf("store next = %d, want 101 (no second number)", next)
	}
	if got := f.outNames(t); len(got) != 2 {
		t.Fatalf("sendReceiptDir = %v, want one receipt", got)
	}
}

func TestReceiptNumbersAreSequentialPerKey(t *testing.T) {
	f := newReceiptFixture(t)
	f.seed(t, 7)
	w := f.writer()
	for i := 0; i < 5; i++ {
		res, err := w.Write(context.Background(), receiptWithKey(testKey(i)))
		if err != nil {
			t.Fatal(err)
		}
		if res.ReceiptNumber != int64(7+i) {
			t.Fatalf("key %d got %d, want %d", i, res.ReceiptNumber, 7+i)
		}
	}
}

func TestReceiptNumberStoreFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		raw  string // "" = no store at all
	}{
		{"missing", ""},
		{"empty file", " "},
		{"corrupt", `{"nextReceiptNumber":`},
		{"zero", `{"nextReceiptNumber":0}`},
		{"negative", `{"nextReceiptNumber":-4}`},
		{"string", `{"nextReceiptNumber":"12"}`},
		{"fraction", `{"nextReceiptNumber":12.5}`},
		{"past the last number", `{"nextReceiptNumber":1000000000}`},
		{"legacy key", `{"lastReceiptNumber":12}`},
		{"extra key", `{"nextReceiptNumber":12,"x":1}`},
		{"trailing data", `{"nextReceiptNumber":12}{}`},
		{"no number", `{}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newReceiptFixture(t)
			if c.raw != "" {
				f.setStore(t, c.raw)
			}
			_, err := f.writer().Write(context.Background(), goldenReceipt())
			if !errors.Is(err, ErrReceiptNumberStore) {
				t.Fatalf("err = %v, want ErrReceiptNumberStore", err)
			}
			if _, err := os.Stat(f.indexPath(goldenReceipt().IdempotencyKey)); !os.IsNotExist(err) {
				t.Error("an index entry was written while the store was unusable")
			}
			if got := f.outNames(t); len(got) != 0 {
				t.Errorf("sendReceiptDir = %v, want empty", got)
			}
			if c.raw != "" {
				if b, _ := os.ReadFile(f.storePath()); string(b) != c.raw {
					t.Errorf("store rewritten to %q", b)
				}
			}
		})
	}
}

func TestReceiptLastNumberThenExhausted(t *testing.T) {
	f := newReceiptFixture(t)
	f.seed(t, 999999999)
	w := f.writer()
	res, err := w.Write(context.Background(), receiptWithKey(testKey(1)))
	if err != nil || res.ReceiptNumber != 999999999 {
		t.Fatalf("last number: %+v, %v", res, err)
	}
	if _, err := w.Write(context.Background(), receiptWithKey(testKey(2))); !errors.Is(err, ErrReceiptNumberStore) {
		t.Fatalf("past the last number: err = %v, want ErrReceiptNumberStore", err)
	}
}

func TestReceiptStoreMovedBackIsRefused(t *testing.T) {
	f := newReceiptFixture(t)
	f.seed(t, 50)
	w := f.writer()
	for i := 1; i <= 3; i++ { // 50, 51, 52
		if _, err := w.Write(context.Background(), receiptWithKey(testKey(i))); err != nil {
			t.Fatal(err)
		}
	}

	// Moved back below what was issued: refused, in this process and after a restart.
	f.setStore(t, `{"nextReceiptNumber":51}`)
	if _, err := w.Write(context.Background(), receiptWithKey(testKey(9))); !errors.Is(err, ErrReceiptNumberStore) {
		t.Fatalf("moved back: err = %v", err)
	}
	restarted := f.writer()
	if err := restarted.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Write(context.Background(), receiptWithKey(testKey(9))); !errors.Is(err, ErrReceiptNumberStore) {
		t.Fatalf("moved back after restart: err = %v", err)
	}
	if _, err := os.Stat(f.indexPath(testKey(9))); !os.IsNotExist(err) {
		t.Fatal("a moved-back store reserved a number")
	}

	// Even with the index gone, the history of an issued number blocks it.
	if err := os.RemoveAll(filepath.Join(f.dataDir, "receipts", "index")); err != nil {
		t.Fatal(err)
	}
	blind := f.writer()
	if err := blind.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := blind.Write(context.Background(), receiptWithKey(testKey(9))); !errors.Is(err, ErrReceiptNumberStore) {
		t.Fatalf("history of 51 exists: err = %v", err)
	}

	// Set by hand to highest-in-history + 1, as the runbook says: works again.
	f.setStore(t, `{"nextReceiptNumber":53}`)
	res, err := blind.Write(context.Background(), receiptWithKey(testKey(9)))
	if err != nil || res.ReceiptNumber != 53 {
		t.Fatalf("after repair: %+v, %v", res, err)
	}
}

func TestSeedReceiptNumber(t *testing.T) {
	f := newReceiptFixture(t)
	for _, n := range []int64{0, -1, 1000000000} {
		if err := SeedReceiptNumber(f.dataDir, n); err == nil {
			t.Errorf("seed %d accepted", n)
		}
	}
	if _, err := os.Stat(f.storePath()); !os.IsNotExist(err) {
		t.Fatal("a refused seed created the store")
	}

	f.seed(t, 300)
	if next := f.storeNext(t); next != 300 {
		t.Fatalf("seeded next = %d", next)
	}
	// Never over an existing store, higher or lower.
	for _, n := range []int64{1, 300, 5000} {
		if err := SeedReceiptNumber(f.dataDir, n); err == nil {
			t.Errorf("seed %d over an existing store accepted", n)
		}
	}
	if next := f.storeNext(t); next != 300 {
		t.Fatalf("store changed to %d", next)
	}

	// With the store lost, a seed at or below an issued number is refused.
	if _, err := f.writer().Write(context.Background(), goldenReceipt()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.storePath()); err != nil {
		t.Fatal(err)
	}
	if err := SeedReceiptNumber(f.dataDir, 300); err == nil {
		t.Fatal("seed at an issued number accepted")
	}
	if err := SeedReceiptNumber(f.dataDir, 301); err != nil {
		t.Fatalf("seed above the issued numbers: %v", err)
	}
}

// A write that fails after the number was reserved keeps the number with its
// key; the next Write, for any key, finishes it first.
func TestReceiptFailedWriteIsFinishedByNextWrite(t *testing.T) {
	f := newReceiptFixture(t)
	f.seed(t, 10)
	w := f.writer()
	w.testHook = func(stage string) error {
		if stage == receiptStagePrm {
			return errors.New("disk full")
		}
		return nil
	}
	_, err := w.Write(context.Background(), receiptWithKey(testKey(1)))
	if !errors.Is(err, ErrReceiptWriteFailed) {
		t.Fatalf("err = %v, want ErrReceiptWriteFailed", err)
	}
	entry, err := readReceiptIndexFile(f.indexPath(testKey(1)), testKey(1))
	if err != nil || entry.State != receiptStateReserved || entry.ReceiptNumber != 10 {
		t.Fatalf("after failure: %+v, %v", entry, err)
	}

	w.testHook = nil
	res, err := w.Write(context.Background(), receiptWithKey(testKey(2)))
	if err != nil || res.ReceiptNumber != 11 {
		t.Fatalf("next key: %+v, %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(f.outDir, "KUPAIN_10.doc")); err != nil {
		t.Fatal("the reserved receipt was not finished first")
	}
	res, err = w.Write(context.Background(), receiptWithKey(testKey(1)))
	if err != nil || res.ReceiptNumber != 10 {
		t.Fatalf("repeat of the failed key: %+v, %v", res, err)
	}
}

func TestReceiptMissingSendReceiptDirKeepsTheNumber(t *testing.T) {
	f := newReceiptFixture(t)
	f.seed(t, 20)
	if err := os.RemoveAll(f.outDir); err != nil {
		t.Fatal(err)
	}
	w := f.writer()
	if _, err := w.Write(context.Background(), goldenReceipt()); !errors.Is(err, ErrReceiptWriteFailed) {
		t.Fatalf("err = %v, want ErrReceiptWriteFailed", err)
	}
	if _, err := os.Stat(f.outDir); !os.IsNotExist(err) {
		t.Fatal("the writer created a missing sendReceiptDir; a typo would swallow receipts")
	}
	if err := os.MkdirAll(f.outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := w.Write(context.Background(), goldenReceipt())
	if err != nil || res.ReceiptNumber != 20 {
		t.Fatalf("after the dir appeared: %+v, %v", res, err)
	}
}

func TestReceiptNeverOverwritesADifferentDoc(t *testing.T) {
	f := newReceiptFixture(t)
	f.seed(t, 4217)
	foreign := filepath.Join(f.outDir, "KUPAIN_4217.doc")
	if err := os.WriteFile(foreign, []byte("someone else's receipt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.writer().Write(context.Background(), goldenReceipt()); !errors.Is(err, ErrReceiptWriteFailed) {
		t.Fatalf("err = %v, want ErrReceiptWriteFailed", err)
	}
	assertFileBytes(t, foreign, []byte("someone else's receipt\n"))
}

func TestReceiptStoredRequestInvalid(t *testing.T) {
	f := newReceiptFixture(t)
	f.seed(t, 30)
	w := f.writer()
	w.testHook = func(stage string) error {
		if stage == receiptStageNumbered {
			return errors.New("crash")
		}
		return nil
	}
	if _, err := w.Write(context.Background(), receiptWithKey(testKey(1))); !errors.Is(err, ErrReceiptWriteFailed) {
		t.Fatalf("err = %v", err)
	}
	w.testHook = nil

	// The stored request no longer builds (edited by hand, or a rule changed).
	path := f.indexPath(testKey(1))
	b, _ := os.ReadFile(path)
	if err := os.WriteFile(path, bytes.Replace(b, []byte(`"last4":"4580"`), []byte(`"last4":"45x0"`), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	restarted := f.writer()
	_ = restarted.Recover(context.Background())
	if _, err := restarted.Write(context.Background(), receiptWithKey(testKey(1))); !errors.Is(err, ErrReceiptStoredRequestInvalid) {
		t.Fatalf("same key: err = %v", err)
	}
	// It blocks every other receipt too, and nothing is reserved for them.
	if _, err := restarted.Write(context.Background(), receiptWithKey(testKey(2))); !errors.Is(err, ErrReceiptStoredRequestInvalid) {
		t.Fatalf("other key: err = %v", err)
	}
	if _, err := os.Stat(f.indexPath(testKey(2))); !os.IsNotExist(err) {
		t.Fatal("a number was reserved behind an unfinished receipt")
	}
}

func TestReceiptIndexUnreadable(t *testing.T) {
	f := newReceiptFixture(t)
	f.seed(t, 40)
	w := f.writer()
	if _, err := w.Write(context.Background(), goldenReceipt()); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"{", `{"state":"weird"}`, ""} {
		if err := os.WriteFile(f.indexPath(goldenReceipt().IdempotencyKey), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		// Never a second number for the key.
		if _, err := w.Write(context.Background(), goldenReceipt()); !errors.Is(err, ErrReceiptIndexUnreadable) {
			t.Fatalf("index %q: err = %v", raw, err)
		}
		// After a restart every receipt is refused: an unreadable entry may own
		// any number.
		restarted := f.writer()
		_ = restarted.Recover(context.Background())
		if _, err := restarted.Write(context.Background(), receiptWithKey(testKey(5))); !errors.Is(err, ErrReceiptIndexUnreadable) {
			t.Fatalf("other key after restart, index %q: err = %v", raw, err)
		}
	}
	if next := f.storeNext(t); next != 41 {
		t.Fatalf("store next = %d, want 41", next)
	}
}

func TestReceiptRecoverRefusesDuplicateNumbers(t *testing.T) {
	f := newReceiptFixture(t)
	f.seed(t, 60)
	w := f.writer()
	if _, err := w.Write(context.Background(), receiptWithKey(testKey(1))); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f.indexPath(testKey(1)))
	clone := bytes.Replace(b, []byte(testKey(1)), []byte(testKey(2)), -1)
	if err := os.WriteFile(f.indexPath(testKey(2)), clone, 0o600); err != nil {
		t.Fatal(err)
	}
	restarted := f.writer()
	_ = restarted.Recover(context.Background())
	if _, err := restarted.Write(context.Background(), receiptWithKey(testKey(3))); !errors.Is(err, ErrReceiptIndexUnreadable) {
		t.Fatalf("two keys own 60: err = %v", err)
	}
}

func TestReceiptWriterBusy(t *testing.T) {
	f := newReceiptFixture(t)
	f.seed(t, 70)
	w := f.writer()
	w.lockWait = 50 * time.Millisecond
	if err := f.lock.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := w.Write(context.Background(), goldenReceipt())
	if !errors.Is(err, ErrReceiptWriterBusy) {
		t.Fatalf("err = %v, want ErrReceiptWriterBusy", err)
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Fatal("did not wait for the lock")
	}
	if next := f.storeNext(t); next != 70 {
		t.Fatalf("store moved to %d while busy", next)
	}
	if _, err := os.Stat(f.indexPath(goldenReceipt().IdempotencyKey)); !os.IsNotExist(err) {
		t.Fatal("busy reserved a number")
	}
	f.lock.Release()
	if res, err := w.Write(context.Background(), goldenReceipt()); err != nil || res.ReceiptNumber != 70 {
		t.Fatalf("after release: %+v, %v", res, err)
	}
}

func TestReceiptInvalidRequestTouchesNothing(t *testing.T) {
	f := newReceiptFixture(t)
	f.seed(t, 80)
	req := goldenReceipt()
	req.Name = strings.Repeat("א", 51)
	_, err := f.writer().Write(context.Background(), req)
	var fe *ReceiptFieldError
	if !errors.As(err, &fe) || fe.Field != "name" {
		t.Fatalf("err = %v", err)
	}
	if next := f.storeNext(t); next != 80 {
		t.Fatal("an invalid request moved the store")
	}
}

// A reader polling sendReceiptDir, as Hashavshevet's scheduler would, only
// ever finds whole files under the names it consumes.
func TestReceiptReaderNeverSeesAHalfFile(t *testing.T) {
	f := newReceiptFixture(t)
	f.seed(t, 1)
	w := f.writer()
	wantPrm := readGolden(t, "kupain_golden.prm")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var bad []string
	var mu sync.Mutex
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			matches, _ := filepath.Glob(filepath.Join(f.outDir, "KUPAIN*"))
			for _, m := range matches {
				b, err := os.ReadFile(m)
				if err != nil {
					continue // renamed or replaced under us: not a partial read
				}
				ok := len(b) == kupainRecordLength+1 && b[len(b)-1] == '\n'
				if strings.HasSuffix(m, ".prm") {
					ok = bytes.Equal(b, wantPrm)
				}
				if !ok {
					mu.Lock()
					bad = append(bad, fmt.Sprintf("%s (%d bytes)", filepath.Base(m), len(b)))
					mu.Unlock()
				}
			}
		}
	}()
	for i := 0; i < 40; i++ {
		if _, err := w.Write(context.Background(), receiptWithKey(testKey(i))); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	if len(bad) > 0 {
		t.Fatalf("reader saw partial files: %v", bad)
	}
}

func TestReceiptRecoverSweepsTempFiles(t *testing.T) {
	f := newReceiptFixture(t)
	f.seed(t, 1)
	leftovers := []string{
		filepath.Join(f.outDir, ".KUPAIN_1.doc.tmp-123"),
		filepath.Join(f.outDir, ".KUPAIN.prm.tmp-9"),
	}
	keep := filepath.Join(f.outDir, "someone-elses.txt")
	for _, p := range append(leftovers, keep) {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.writer().Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, p := range leftovers {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s left behind", filepath.Base(p))
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("Recover removed a file it does not own")
	}
}

// The kill tests run the writer in a child process that dies (os.Exit, no
// cleanup) at one stage of a write, then restart in this process.
const crashChildEnv = "DIGI_RECEIPT_CRASH_CHILD"

func TestReceiptCrashChild(t *testing.T) {
	if os.Getenv(crashChildEnv) == "" {
		t.Skip("helper for TestReceiptKillAndRestart")
	}
	stage := os.Getenv(crashChildEnv)
	w := NewReceiptWriter(ReceiptWriterConfig{
		DataDir:        os.Getenv("DIGI_RECEIPT_DATA"),
		SendReceiptDir: os.Getenv("DIGI_RECEIPT_OUT"),
		Lock:           NewImportLock(),
		Log:            noopLogger{},
	})
	w.testHook = func(s string) error {
		if s == stage {
			os.Exit(3)
		}
		return nil
	}
	_, err := w.Write(context.Background(), goldenReceipt())
	fmt.Fprintf(os.Stderr, "child finished without crashing: %v\n", err)
	os.Exit(4)
}

func TestReceiptKillAndRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns child processes")
	}
	stages := []string{
		receiptStageLocked, receiptStageReserved, receiptStageNumbered,
		receiptStageHistory, receiptStagePrm, receiptStageDoc,
	}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			f := newReceiptFixture(t)
			f.seed(t, 4217)

			cmd := exec.Command(os.Args[0], "-test.run=^TestReceiptCrashChild$", "-test.count=1")
			cmd.Env = append(os.Environ(), crashChildEnv+"="+stage,
				"DIGI_RECEIPT_DATA="+f.dataDir, "DIGI_RECEIPT_OUT="+f.outDir)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 3 {
				t.Fatalf("child did not crash at %s: %v\n%s", stage, err, out)
			}

			w := f.writer()
			if err := w.Recover(context.Background()); err != nil {
				t.Fatalf("Recover: %v", err)
			}
			// The repeat of the key answers written with 4217, whatever the stage.
			res, err := w.Write(context.Background(), goldenReceipt())
			if err != nil || res.ReceiptNumber != 4217 {
				t.Fatalf("repeat after the crash: %+v, %v", res, err)
			}
			// Exactly one KUPAIN_{n}, byte-identical to the legacy writer's.
			if got := f.outNames(t); strings.Join(got, ",") != "KUPAIN.prm,KUPAIN_4217.doc" {
				t.Fatalf("sendReceiptDir = %v", got)
			}
			assertFileBytes(t, filepath.Join(f.outDir, "KUPAIN_4217.doc"), readGolden(t, "kupain_golden.doc"))
			// And the next receipt takes the next number.
			res, err = w.Write(context.Background(), receiptWithKey(testKey(1)))
			if err != nil || res.ReceiptNumber != 4218 {
				t.Fatalf("next key: %+v, %v", res, err)
			}
			if next := f.storeNext(t); next != 4219 {
				t.Fatalf("store next = %d, want 4219", next)
			}
		})
	}
}

func TestReceiptRecoverFinishesReservedFilesOnly(t *testing.T) {
	f := newReceiptFixture(t)
	f.seed(t, 90)
	w := f.writer()
	w.testHook = func(stage string) error {
		if stage == receiptStageNumbered {
			return errors.New("crash")
		}
		return nil
	}
	if _, err := w.Write(context.Background(), goldenReceipt()); err == nil {
		t.Fatal("expected the injected failure")
	}

	restarted := f.writer()
	if err := restarted.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.outDir, "KUPAIN_90.doc")); err != nil {
		t.Fatal("Recover did not finish the reserved receipt")
	}
	entry, err := readReceiptIndexFile(f.indexPath(goldenReceipt().IdempotencyKey), goldenReceipt().IdempotencyKey)
	if err != nil || entry.State != receiptStateWritten || entry.Import != ReceiptImportNotRun {
		t.Fatalf("after Recover: %+v, %v", entry, err)
	}
}

func TestReceiptIndexEntryNamesItsKey(t *testing.T) {
	f := newReceiptFixture(t)
	f.seed(t, 1)
	if _, err := f.writer().Write(context.Background(), goldenReceipt()); err != nil {
		t.Fatal(err)
	}
	// Copied under another key's name, the entry is not that key's.
	other := testKey(77)
	b, _ := os.ReadFile(f.indexPath(goldenReceipt().IdempotencyKey))
	if err := os.WriteFile(f.indexPath(other), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readReceiptIndexFile(f.indexPath(other), other); err == nil {
		t.Fatal("an entry stored under the wrong key was accepted")
	}
}
