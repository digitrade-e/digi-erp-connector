package hasavshevet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// orderHarness is one connector install for orders: sendOrderDir (IMOVEIN
// files, history, lastOrderNumber.json) and a DataDir holding the key index.
type orderHarness struct {
	t       *testing.T
	sendDir string
	dataDir string
	log     *recordingLogger
}

func newOrderHarness(t *testing.T) *orderHarness {
	t.Helper()
	root := t.TempDir()
	h := &orderHarness{t: t, sendDir: filepath.Join(root, "orders"), dataDir: filepath.Join(root, "data"), log: &recordingLogger{}}
	if err := os.MkdirAll(h.sendDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return h
}

// queue builds a queue the way the daemon does at startup — a new process
// over the same folders: key index on, Recover, then (when start) the worker.
// The test must not have another started queue on these folders running.
func (h *orderHarness) queue(start bool) *OrderQueue {
	h.t.Helper()
	q := h.newQueue()
	if err := q.Recover(); err != nil {
		h.t.Fatalf("Recover: %v", err)
	}
	if start {
		h.start(q)
	}
	return q
}

func (h *orderHarness) newQueue() *OrderQueue {
	h.t.Helper()
	sender := NewSender(nil, config.Config{SendOrderDir: h.sendDir},
		NewOrderNumberStore(filepath.Join(h.sendDir, "lastOrderNumber.json")), h.log)
	q := NewOrderQueue(sender, h.log)
	q.UseKeyIndex(h.dataDir)
	return q
}

func (h *orderHarness) start(q *OrderQueue) context.CancelFunc {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel and wait for the worker, before TempDir's cleanup removes the
	// folders it writes into (cleanups run last-in first-out).
	h.t.Cleanup(func() {
		cancel()
		select {
		case <-q.done:
		case <-time.After(10 * time.Second):
			h.t.Error("the order worker did not exit within 10 s of its cancel")
		}
	})
	q.Start(ctx)
	return cancel
}

// stop drains a started queue, as the daemon's Stop does before a restart.
func (h *orderHarness) stop(q *OrderQueue) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := q.Drain(ctx); err != nil {
		h.t.Fatalf("Drain: %v", err)
	}
}

func (h *orderHarness) counter() int64 { return lastOrderNumber(h.t, h.sendDir) }

func (h *orderHarness) entryPath(key string) string {
	return filepath.Join(OrderIndexDir(h.dataDir), orderIndexStem(key)+".json")
}

func (h *orderHarness) entry(key string) *orderIndexEntry {
	h.t.Helper()
	e, found, err := orderIndex{dir: OrderIndexDir(h.dataDir)}.load(key)
	if err != nil || !found {
		h.t.Fatalf("index entry for %q = found %v, %v", key, found, err)
	}
	return e
}

func (h *orderHarness) historyDirs() int {
	h.t.Helper()
	entries, err := os.ReadDir(filepath.Join(h.sendDir, "history"))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return len(entries)
}

// keyedOrder is a valid order for historyId key; it writes (account supplied).
func keyedOrder(key string) OrderRequest {
	r := drainTestOrder(0)
	r.HistoryID = key
	return r
}

// failingOrder fails in the worker before any file is written: no account
// details and no database. Its key is retryable, not failed.
func failingOrder(key string) OrderRequest {
	r := keyedOrder(key)
	r.Account = nil
	return r
}

func mustAccept(t *testing.T, q *OrderQueue, req OrderRequest) Acceptance {
	t.Helper()
	acc, err := q.Accept(req)
	if err != nil {
		t.Fatalf("Accept(%s): %v", req.HistoryID, err)
	}
	return acc
}

func waitForStatus(t *testing.T, q *OrderQueue, jobID string, want JobStatus) *JobResult {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if r, ok := q.Status(jobID); ok && r.Status == want {
			return r
		}
		if time.Now().After(deadline) {
			r, _ := q.Status(jobID)
			t.Fatalf("job %s never reached %s; last %+v", jobID, want, r)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type recordingLogger struct {
	mu     sync.Mutex
	warns  []string
	errors []string
}

func (*recordingLogger) Info(string)    {}
func (*recordingLogger) Success(string) {}
func (*recordingLogger) Close() error   { return nil }
func (l *recordingLogger) Warn(msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg)
}
func (l *recordingLogger) Error(msg string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errors = append(l.errors, fmt.Sprintf("%s: %v", msg, err))
}

func (l *recordingLogger) warned(substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, w := range l.warns {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

// The first POST of a key reserves a number and queues the order; every repeat
// answers that same job and touches nothing — no number, no queue slot, no
// file. A different key is a different order.
func TestOrderQueue_RepeatOfAKeyAnswersTheFirstJob(t *testing.T) {
	h := newOrderHarness(t)
	q := h.queue(false) // not started: the job stays queued

	first := mustAccept(t, q, keyedOrder("4711-rel-a"))
	if first.JobID != "1" || first.Status != JobStatusQueued || first.Replay {
		t.Fatalf("first = %+v; want job 1, queued, not a replay", first)
	}
	indexBefore := readFile(t, h.entryPath("4711-rel-a"))
	for i := 0; i < 3; i++ {
		again := mustAccept(t, q, keyedOrder("4711-rel-a"))
		if again.JobID != "1" || again.Status != JobStatusQueued || !again.Replay {
			t.Fatalf("repeat %d = %+v; want job 1, queued, replay", i, again)
		}
	}
	if q.Pending() != 1 {
		t.Errorf("queue holds %d jobs; repeats must not enqueue", q.Pending())
	}
	if n := h.counter(); n != 1 {
		t.Errorf("counter = %d; repeats must not reserve numbers", n)
	}
	if string(readFile(t, h.entryPath("4711-rel-a"))) != string(indexBefore) {
		t.Error("a repeat rewrote the index entry")
	}

	other := mustAccept(t, q, keyedOrder("4711-rel-b"))
	if other.JobID != "2" || other.Replay {
		t.Fatalf("another key = %+v; want job 2, not a replay", other)
	}
	if e := h.entry("4711-rel-b"); e.Number != 2 || e.State != orderReserved {
		t.Errorf("entry for the second key = %+v; want 2 reserved", e)
	}
}

// Replays are answered even when the queue is full: they need no slot.
func TestOrderQueue_ReplayNeedsNoQueueSlot(t *testing.T) {
	h := newOrderHarness(t)
	q := h.queue(false)
	for i := 0; i < defaultQueueSize; i++ {
		mustAccept(t, q, keyedOrder("k-"+strconv.Itoa(i)))
	}
	if _, err := q.Accept(keyedOrder("one-more")); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("a new key on a full queue = %v; want ErrQueueFull", err)
	}
	if n := h.counter(); n != defaultQueueSize {
		t.Errorf("counter = %d; the refusal burned a number", n)
	}
	if _, err := os.Stat(h.entryPath("one-more")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused key has an index entry: %v", err)
	}
	if acc := mustAccept(t, q, keyedOrder("k-0")); acc.JobID != "1" || !acc.Replay {
		t.Errorf("replay on a full queue = %+v; want job 1, replay", acc)
	}
}

// Two POSTs of one key racing each other get one number between them.
func TestOrderQueue_ConcurrentRepeatsOfOneKeyGetOneNumber(t *testing.T) {
	h := newOrderHarness(t)
	q := h.queue(false)
	const n = 24
	results := make([]Acceptance, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = q.Accept(keyedOrder("4711-rel-a"))
		}(i)
	}
	wg.Wait()
	fresh := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("Accept %d: %v", i, errs[i])
		}
		if results[i].JobID != "1" {
			t.Errorf("Accept %d answered job %s; want 1", i, results[i].JobID)
		}
		if !results[i].Replay {
			fresh++
		}
	}
	if fresh != 1 || q.Pending() != 1 || h.counter() != 1 {
		t.Errorf("fresh answers %d, queued %d, counter %d; want 1, 1, 1", fresh, q.Pending(), h.counter())
	}
}

// The worker records the outcome on the key's entry. A written order replays
// as done with its first jobId; it is never written again.
func TestOrderQueue_WorkerSuccessMarksTheEntryWritten(t *testing.T) {
	h := newOrderHarness(t)
	q := h.queue(true)
	acc := mustAccept(t, q, keyedOrder("4711-rel-a"))
	res := waitForStatus(t, q, acc.JobID, JobStatusDone)

	e := h.entry("4711-rel-a")
	if e.State != orderWritten || e.Number != 1 || len(e.Files) != 4 || e.FinishedAt == "" || e.Error != "" {
		t.Fatalf("entry after success = %+v; want written, number 1, the four files", e)
	}
	if strings.Join(e.Files, "|") != strings.Join(res.WrittenFiles, "|") {
		t.Errorf("entry files %v; job files %v", e.Files, res.WrittenFiles)
	}
	again := mustAccept(t, q, keyedOrder("4711-rel-a"))
	if again.JobID != "1" || again.Status != JobStatusDone || !again.Replay {
		t.Fatalf("replay after success = %+v; want job 1, done, replay", again)
	}
	time.Sleep(50 * time.Millisecond) // a wrongly queued job would run now
	if h.historyDirs() != 1 || h.counter() != 1 {
		t.Errorf("history folders %d, counter %d; want 1, 1", h.historyDirs(), h.counter())
	}
}

// A failure once a file may exist is recorded on the entry and answered as
// failed with the same jobId — never written again under that key (a
// re-release is a new key).
func TestOrderQueue_WorkerFailureIsRecordedAndNeverRequeued(t *testing.T) {
	h := newOrderHarness(t)
	q := h.queue(true)
	unblock := h.block("IMOVEIN.prm") // IMOVEIN.doc is written, IMOVEIN.prm is not
	acc := mustAccept(t, q, keyedOrder("4711-rel-a"))
	waitForStatus(t, q, acc.JobID, JobStatusFailed)
	unblock()

	e := h.entry("4711-rel-a")
	if e.State != orderFailed || e.Number != 1 || !strings.Contains(e.Error, "IMOVEIN.prm") || len(e.Files) != 0 {
		t.Fatalf("entry after failure = %+v; want failed, number 1, the error text", e)
	}
	// Even a repeat whose body would now succeed answers the recorded failure.
	for _, req := range []OrderRequest{failingOrder("4711-rel-a"), keyedOrder("4711-rel-a")} {
		again := mustAccept(t, q, req)
		if again.JobID != "1" || again.Status != JobStatusFailed || !again.Replay {
			t.Fatalf("replay after failure = %+v; want job 1, failed, replay", again)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(h.historyDoc(1)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the failed order was written again: %v", err)
	}
	if queued, running := q.Unfinished(); queued+running != 0 || h.counter() != 1 {
		t.Errorf("unfinished %d/%d, counter %d; want nothing re-queued", queued, running, h.counter())
	}
}

// A restart forgets the in-memory jobs; the index does not. Each outcome is
// answered from disk with the first jobId, and nothing is reserved or queued.
func TestOrderQueue_ReplayAfterRestartAnswersFromTheIndex(t *testing.T) {
	h := newOrderHarness(t)
	q := h.queue(true)
	done := mustAccept(t, q, keyedOrder("rel-done"))
	waitForStatus(t, q, done.JobID, JobStatusDone)
	unblock := h.block("IMOVEIN.prm") // a failure after IMOVEIN.doc is written: final
	failed := mustAccept(t, q, keyedOrder("rel-failed"))
	waitForStatus(t, q, failed.JobID, JobStatusFailed)
	unblock()
	h.stop(q)

	q2 := h.queue(true) // the restart
	for _, tc := range []struct {
		req  OrderRequest
		job  string
		want JobStatus
	}{
		{keyedOrder("rel-done"), "1", JobStatusDone},
		{keyedOrder("rel-failed"), "2", JobStatusFailed},
	} {
		acc := mustAccept(t, q2, tc.req)
		if acc.JobID != tc.job || acc.Status != tc.want || !acc.Replay {
			t.Errorf("%s after restart = %+v; want job %s, %s, replay", tc.req.HistoryID, acc, tc.job, tc.want)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := h.counter(); n != 2 {
		t.Errorf("counter = %d; want 2", n)
	}
	if _, err := os.Stat(h.historyDoc(2)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the failed order was written by a replay: %v", err)
	}
	if next := mustAccept(t, q2, keyedOrder("rel-new")); next.JobID != "3" || next.Replay {
		t.Errorf("a new key after restart = %+v; want job 3", next)
	}
}

// An order accepted (202) but not written before the process died: its entry
// is still reserved. Whether the file and the import happened is unknown, so
// the key is never written again; it answers "unknown" and says so loudly.
func TestOrderQueue_ReservedEntryAfterRestartIsUnknownAndNeverRewritten(t *testing.T) {
	h := newOrderHarness(t)
	q := h.queue(false) // never started: the order is never written
	mustAccept(t, q, keyedOrder("rel-lost"))

	q2 := h.queue(true) // the restart
	if !h.log.warned("rel-lost") {
		t.Error("recovery did not name the key whose outcome is unknown")
	}
	acc := mustAccept(t, q2, keyedOrder("rel-lost"))
	if acc.JobID != "1" || acc.Status != JobStatusUnknown || !acc.Replay {
		t.Fatalf("replay of a reserved entry after restart = %+v; want job 1, unknown, replay", acc)
	}
	time.Sleep(50 * time.Millisecond)
	if e := h.entry("rel-lost"); e.State != orderReserved {
		t.Errorf("entry = %+v; a replay must not change it", e)
	}
	if h.historyDirs() != 0 || h.counter() != 1 || q2.Pending() != 0 {
		t.Errorf("history %d, counter %d, queued %d; want nothing written or queued", h.historyDirs(), h.counter(), q2.Pending())
	}
}

// The status endpoint's source: after a restart a jobId the memory no longer
// has is answered from the index.
func TestOrderQueue_StatusFallsBackToTheIndex(t *testing.T) {
	h := newOrderHarness(t)
	q := h.queue(true)
	waitForStatus(t, q, mustAccept(t, q, keyedOrder("rel-done")).JobID, JobStatusDone)
	waitForStatus(t, q, mustAccept(t, q, failingOrder("rel-failed")).JobID, JobStatusFailed)
	h.stop(q)
	q2 := h.newQueue()
	if err := q2.Recover(); err != nil {
		t.Fatal(err)
	}
	mustAccept(t, q2, keyedOrder("rel-queued")) // job 3, in memory, never started

	q3 := h.queue(false) // and restart once more: 3 is reserved on disk only
	for _, tc := range []struct {
		job     string
		want    JobStatus
		files   int
		withErr bool
	}{
		{"1", JobStatusDone, 4, false},
		{"2", JobStatusFailed, 0, true},
		{"3", JobStatusUnknown, 0, false},
	} {
		r, ok := q3.Status(tc.job)
		if !ok {
			t.Errorf("Status(%s) not found", tc.job)
			continue
		}
		if r.ID != tc.job || r.Status != tc.want || strconv.FormatInt(r.OrderNumber, 10) != tc.job ||
			len(r.WrittenFiles) != tc.files || (r.Err != nil) != tc.withErr {
			t.Errorf("Status(%s) = %+v; want %s with %d files, error %v", tc.job, r, tc.want, tc.files, tc.withErr)
		}
	}
	for _, job := range []string{"4", "0", "-1", "abc", ""} {
		if r, ok := q3.Status(job); ok {
			t.Errorf("Status(%q) = %+v; want not found", job, r)
		}
	}
}

// An index entry that cannot be trusted refuses its key — it is never given a
// second number — and the counter does not move.
func TestOrderQueue_UnreadableEntryRefusesItsKey(t *testing.T) {
	h := newOrderHarness(t)
	q := h.queue(false)
	mustAccept(t, q, keyedOrder("rel-a"))
	if err := os.WriteFile(h.entryPath("rel-a"), []byte(`{"key":"rel-a","number":1,"state":"written","oops":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if acc, err := q.Accept(keyedOrder("rel-a")); !errors.Is(err, ErrOrderIndexUnreadable) {
		t.Fatalf("Accept over an unreadable entry = %+v, %v; want ErrOrderIndexUnreadable", acc, err)
	}
	if n := h.counter(); n != 1 || q.Pending() != 1 {
		t.Errorf("counter %d, queued %d; want 1, 1 — the refusal reserved or queued", n, q.Pending())
	}
	// Other keys still work while the process runs.
	if acc := mustAccept(t, q, keyedOrder("rel-b")); acc.JobID != "2" {
		t.Errorf("another key = %+v; want job 2", acc)
	}

	// After a restart recovery skips the damaged entry (logged) and the other
	// keys keep working; the damaged key is still refused.
	q2 := h.newQueue()
	if err := q2.Recover(); err != nil {
		t.Fatalf("Recover over an unreadable entry = %v; want it skipped", err)
	}
	if _, err := q2.Accept(keyedOrder("rel-a")); !errors.Is(err, ErrOrderIndexUnreadable) {
		t.Errorf("the damaged key after a restart = %v; want ErrOrderIndexUnreadable", err)
	}
	if acc := mustAccept(t, q2, keyedOrder("rel-c")); acc.JobID != "3" {
		t.Errorf("a new key after a restart = %+v; want job 3", acc)
	}
}

// A crash after the entry is saved: before the counter moved on, and after.
// On restart the counter is past the entry's number, the key answers its job
// as unknown, and the next key gets the next number — no number has two keys.
func TestOrderQueue_CrashBetweenIndexAndCounter(t *testing.T) {
	for _, point := range []string{crashOrderAfterIndexReserved, crashOrderAfterCounterAdvanced} {
		t.Run(point, func(t *testing.T) {
			h := newOrderHarness(t)
			q := h.queue(false)
			q.crashPoint = func(p string) {
				if p == point {
					panic("killed at " + p)
				}
			}
			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("the crash hook did not fire")
					}
				}()
				_, _ = q.Accept(keyedOrder("rel-a"))
			}()
			if e := h.entry("rel-a"); e.Number != 1 || e.State != orderReserved {
				t.Fatalf("entry at the crash = %+v; want 1 reserved", e)
			}

			q2 := h.queue(false) // the restart
			if n := h.counter(); n != 1 {
				t.Fatalf("counter after recovery = %d; want 1 (past the entry)", n)
			}
			if acc := mustAccept(t, q2, keyedOrder("rel-a")); acc.JobID != "1" || acc.Status != JobStatusUnknown || !acc.Replay {
				t.Errorf("the crashed key = %+v; want job 1, unknown, replay", acc)
			}
			if acc := mustAccept(t, q2, keyedOrder("rel-b")); acc.JobID != "2" || acc.Replay {
				t.Errorf("the next key = %+v; want job 2", acc)
			}
		})
	}
}

// The counter could not be moved past a number the index already gave a key:
// the order still goes ahead under it (the index owns it), and nothing new is
// issued until recovery has moved the counter on.
func TestOrderQueue_CounterThatCannotAdvanceIsRecoveredBeforeTheNextNumber(t *testing.T) {
	h := newOrderHarness(t)
	q := h.queue(false)
	counter := filepath.Join(h.sendDir, "lastOrderNumber.json")
	broken := false
	q.crashPoint = func(p string) {
		if p == crashOrderAfterIndexReserved && !broken {
			broken = true
			// The counter turns unwritable between the index save and its advance.
			if err := os.Mkdir(counter, 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	acc := mustAccept(t, q, keyedOrder("rel-a"))
	if acc.JobID != "1" || acc.Status != JobStatusQueued || q.Pending() != 1 {
		t.Fatalf("Accept with a stuck counter = %+v, queued %d; want job 1 queued", acc, q.Pending())
	}
	if len(h.log.errors) == 0 {
		t.Error("the stuck counter was not logged")
	}

	// Still stuck: a new key is refused rather than given 1 again.
	if _, err := q.Accept(keyedOrder("rel-b")); err == nil {
		t.Fatal("a new key was accepted while the counter still offers a number the index owns")
	}
	if err := os.Remove(counter); err != nil { // the disk is repaired (counter missing = 0)
		t.Fatal(err)
	}
	if acc := mustAccept(t, q, keyedOrder("rel-b")); acc.JobID != "2" {
		t.Fatalf("the next key after repair = %+v; want job 2, never 1 again", acc)
	}
	if n := h.counter(); n != 2 {
		t.Errorf("counter = %d; want 2", n)
	}
}

// A counter moved back (restored from an old backup, hand-edited) is moved
// past every number the index owns, with a warning, before any is issued.
func TestOrderQueue_CounterMovedBackIsMovedForward(t *testing.T) {
	h := newOrderHarness(t)
	q := h.queue(false)
	for i := 0; i < 3; i++ {
		mustAccept(t, q, keyedOrder("rel-"+strconv.Itoa(i)))
	}
	if err := os.WriteFile(filepath.Join(h.sendDir, "lastOrderNumber.json"), []byte(`{"lastOrderNumber": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	q2 := h.queue(false)
	if !h.log.warned("lastOrderNumber") {
		t.Error("moving the counter forward was not logged")
	}
	if acc := mustAccept(t, q2, keyedOrder("rel-new")); acc.JobID != "4" {
		t.Errorf("a new key = %+v; want job 4", acc)
	}
}

// A repeat whose body differs from the first is answered with the first
// order (the key decides) and logged, so a person can see a key reused.
func TestOrderQueue_RepeatWithADifferentBodyIsLogged(t *testing.T) {
	h := newOrderHarness(t)
	q := h.queue(false)
	mustAccept(t, q, keyedOrder("rel-a"))
	if h.log.warned("different") {
		t.Fatal("warned before any repeat")
	}
	mustAccept(t, q, keyedOrder("rel-a"))
	if h.log.warned("different") {
		t.Error("an identical repeat was reported as different")
	}
	changed := keyedOrder("rel-a")
	changed.Total = 99
	if acc := mustAccept(t, q, changed); acc.JobID != "1" || !acc.Replay {
		t.Fatalf("changed repeat = %+v; want job 1, replay", acc)
	}
	if !h.log.warned("different") {
		t.Error("a repeat with a different body was not logged")
	}
}

// Without the key index (a queue no data directory was given) Submit behaves
// as it always did: every call is a new order.
func TestOrderQueue_WithoutTheIndexEveryCallIsNew(t *testing.T) {
	q, dir, _ := drainTestQueue(t)
	if err := lockImport(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer unlockImport()
	a, _ := q.Submit(keyedOrder("same"))
	b, _ := q.Submit(keyedOrder("same"))
	if a == b || lastOrderNumber(t, dir) != 2 {
		t.Errorf("jobs %s and %s, counter %d; want two orders", a, b, lastOrderNumber(t, dir))
	}
}

// The counter moved back while the connector runs (a restore over a live
// install): the number it offers is already owned, so the key is refused with
// nothing reserved, and the next attempt recovers first and gets a fresh number.
func TestOrderQueue_CounterMovedBackWhileRunningNeverReissuesANumber(t *testing.T) {
	h := newOrderHarness(t)
	q := h.queue(false)
	mustAccept(t, q, keyedOrder("rel-a"))
	mustAccept(t, q, keyedOrder("rel-b"))
	if err := os.WriteFile(filepath.Join(h.sendDir, "lastOrderNumber.json"), []byte(`{"lastOrderNumber": 0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if acc, err := q.Accept(keyedOrder("rel-c")); err == nil {
		t.Fatalf("Accept = %+v; want a refusal — order 1 belongs to rel-a", acc)
	}
	if _, err := os.Stat(h.entryPath("rel-c")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused key has an index entry: %v", err)
	}
	if acc := mustAccept(t, q, keyedOrder("rel-c")); acc.JobID != "3" || acc.Replay {
		t.Fatalf("the retry = %+v; want job 3", acc)
	}
	if acc := mustAccept(t, q, keyedOrder("rel-a")); acc.JobID != "1" || !acc.Replay {
		t.Errorf("rel-a = %+v; still job 1", acc)
	}
}
