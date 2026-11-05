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
)

// block plants a directory where the worker must write a file, so opening
// it fails: IMOVEIN.doc → before the first write; IMOVEIN.prm → after it.
func (h *orderHarness) block(name string) (unblock func()) {
	h.t.Helper()
	p := filepath.Join(h.sendDir, name)
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) { // a file an earlier order wrote
		h.t.Fatal(err)
	}
	if err := os.Mkdir(p, 0o700); err != nil {
		h.t.Fatal(err)
	}
	return func() {
		if err := os.Remove(p); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *orderHarness) historyDoc(n int64) string {
	s := strconv.FormatInt(n, 10)
	return filepath.Join(h.sendDir, "history", s, "IMOVEIN_"+s+".doc")
}

func (h *orderHarness) setCounter(content string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.sendDir, "lastOrderNumber.json"), []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// A failure before the first file write (here: IMOVEIN.doc held, as by a
// Windows sharing violation) leaves nothing on disk. The key is retryable: a
// repeat queues the order again under the SAME number, and it is written.
func TestOrderQueue_FailureBeforeWritingIsRetriedUnderTheSameNumber(t *testing.T) {
	h := newOrderHarness(t)
	q := h.queue(true)
	unblock := h.block("IMOVEIN.doc")
	acc := mustAccept(t, q, keyedOrder("rel-a"))
	waitForStatus(t, q, acc.JobID, JobStatusFailed)
	if e := h.entry("rel-a"); e.State != orderRetryable || e.Number != 1 || e.Error == "" {
		t.Fatalf("entry after a failure before writing = %+v; want retryable, 1, the error", e)
	}

	unblock()
	again := mustAccept(t, q, keyedOrder("rel-a"))
	if again.JobID != "1" || again.Status != JobStatusQueued || !again.Replay {
		t.Fatalf("repeat = %+v; want job 1 queued again, replay", again)
	}
	waitForStatus(t, q, "1", JobStatusDone)
	if e := h.entry("rel-a"); e.State != orderWritten || e.Number != 1 || e.Retries != 1 || e.Error != "" {
		t.Errorf("entry after the retry = %+v; want written, 1, one retry, no error", e)
	}
	if _, err := os.Stat(h.historyDoc(1)); err != nil {
		t.Errorf("order 1 was not written on the retry: %v", err)
	}
	if n := h.counter(); n != 1 {
		t.Errorf("counter = %d; a retry must not take a new number", n)
	}
	if done := mustAccept(t, q, keyedOrder("rel-a")); done.JobID != "1" || done.Status != JobStatusDone {
		t.Errorf("repeat after the retry = %+v; want job 1 done", done)
	}
}

// A failure once a file may exist (IMOVEIN.doc written, IMOVEIN.prm not) is
// final: a repeat answers failed and queues nothing.
func TestOrderQueue_FailureAfterAFileIsWrittenIsFinal(t *testing.T) {
	h := newOrderHarness(t)
	q := h.queue(true)
	unblock := h.block("IMOVEIN.prm")
	acc := mustAccept(t, q, keyedOrder("rel-a"))
	waitForStatus(t, q, acc.JobID, JobStatusFailed)
	if e := h.entry("rel-a"); e.State != orderFailed || e.Error == "" {
		t.Fatalf("entry = %+v; want failed with the error", e)
	}
	unblock()
	for i := 0; i < 2; i++ {
		again := mustAccept(t, q, keyedOrder("rel-a"))
		if again.JobID != "1" || again.Status != JobStatusFailed || !again.Replay {
			t.Fatalf("repeat = %+v; want job 1 failed, replay", again)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(h.historyDoc(1)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a final failure was written again: %v", err)
	}
	if queued, running := q.Unfinished(); queued+running != 0 {
		t.Errorf("unfinished %d/%d; want nothing queued", queued, running)
	}
}

// The retryable state is on disk: a restart between the failure and the
// repeat changes nothing.
func TestOrderQueue_FailureBeforeWritingThenRestartIsRetried(t *testing.T) {
	h := newOrderHarness(t)
	q := h.queue(true)
	unblock := h.block("IMOVEIN.doc")
	waitForStatus(t, q, mustAccept(t, q, keyedOrder("rel-a")).JobID, JobStatusFailed)
	h.stop(q)
	unblock()

	q2 := h.queue(true)
	if r, ok := q2.Status("1"); !ok || r.Status != JobStatusFailed || r.Err == nil {
		t.Errorf("status after restart = %+v, %v; want failed (a repeat will retry it)", r, ok)
	}
	again := mustAccept(t, q2, keyedOrder("rel-a"))
	if again.JobID != "1" || again.Status != JobStatusQueued || !again.Replay {
		t.Fatalf("repeat after restart = %+v; want job 1 queued again", again)
	}
	waitForStatus(t, q2, "1", JobStatusDone)
	if n := h.counter(); n != 1 {
		t.Errorf("counter = %d; want 1", n)
	}
}

// Repeats racing for a retryable key queue it once: the entry is reserved
// again before the job is sent, so the others see it queued.
func TestOrderQueue_ConcurrentRepeatsOfARetryableKeyQueueItOnce(t *testing.T) {
	h := newOrderHarness(t)
	q := h.queue(true)
	unblock := h.block("IMOVEIN.doc")
	waitForStatus(t, q, mustAccept(t, q, keyedOrder("rel-a")).JobID, JobStatusFailed)
	h.stop(q)
	unblock()

	q2 := h.queue(false) // not started: whatever is queued stays in the channel
	const n = 24
	var wg sync.WaitGroup
	accs := make([]Acceptance, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			accs[i], errs[i] = q2.Accept(keyedOrder("rel-a"))
		}(i)
	}
	wg.Wait()
	for i := range accs {
		if errs[i] != nil || accs[i].JobID != "1" || !accs[i].Replay || accs[i].Status != JobStatusQueued {
			t.Fatalf("repeat %d = %+v, %v; want job 1 queued, replay", i, accs[i], errs[i])
		}
	}
	if p := q2.Pending(); p != 1 {
		t.Errorf("queue holds %d jobs for order 1; want exactly one", p)
	}
	if e := h.entry("rel-a"); e.State != orderReserved || e.Retries != 1 {
		t.Errorf("entry = %+v; want reserved again, one retry", e)
	}
}

// A retry needs a queue slot; a full queue refuses it and leaves the key
// retryable.
func TestOrderQueue_RetryOnAFullQueueIsRefused(t *testing.T) {
	h := newOrderHarness(t)
	q := h.queue(true)
	unblock := h.block("IMOVEIN.doc")
	waitForStatus(t, q, mustAccept(t, q, keyedOrder("rel-a")).JobID, JobStatusFailed)
	h.stop(q)
	unblock()

	q2 := h.queue(false)
	for i := 0; i < defaultQueueSize; i++ {
		mustAccept(t, q2, keyedOrder("fill-"+strconv.Itoa(i)))
	}
	if _, err := q2.Accept(keyedOrder("rel-a")); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("retry on a full queue = %v; want ErrQueueFull", err)
	}
	if e := h.entry("rel-a"); e.State != orderRetryable {
		t.Errorf("entry = %+v; want still retryable", e)
	}
}

// Orders queued when the worker is stopped (the drain timed out) were never
// started: nothing of them is on disk, so they are retryable, not unknown.
func TestOrderQueue_WorkerStoppedBeforeWritingLeavesQueuedOrdersRetryable(t *testing.T) {
	h := newOrderHarness(t)
	q := h.newQueue()
	if err := q.Recover(); err != nil {
		t.Fatal(err)
	}
	if err := lockImport(context.Background()); err != nil { // a receipt holds the importer
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			unlockImport()
		}
	}()
	cancel := h.start(q)
	mustAccept(t, q, keyedOrder("rel-a"))
	mustAccept(t, q, keyedOrder("rel-b"))
	cancel()
	select {
	case <-q.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not exit on its cancel")
	}
	unlockImport()
	locked = false
	for _, key := range []string{"rel-a", "rel-b"} {
		if e := h.entry(key); e.State != orderRetryable {
			t.Errorf("entry %s after the worker stopped = %+v; want retryable", key, e)
		}
	}

	q2 := h.queue(true)
	for i, key := range []string{"rel-a", "rel-b"} {
		acc := mustAccept(t, q2, keyedOrder(key))
		if acc.JobID != strconv.Itoa(i+1) || acc.Status != JobStatusQueued {
			t.Fatalf("repeat of %s after restart = %+v; want job %d queued", key, acc, i+1)
		}
		waitForStatus(t, q2, acc.JobID, JobStatusDone)
	}
}

// One damaged entry no longer blocks every order after a restart: recovery
// skips it (naming the file), moves the counter past the highest number it
// can still see — readable entries and sendOrderDir/history — and only the
// damaged key itself is refused.
func TestOrderQueue_RecoverSkipsAnUnreadableEntry(t *testing.T) {
	h := newOrderHarness(t)
	q := h.queue(true)
	for _, key := range []string{"rel-1", "rel-2", "rel-3"} {
		waitForStatus(t, q, mustAccept(t, q, keyedOrder(key)).JobID, JobStatusDone)
	}
	h.stop(q)
	if err := os.WriteFile(h.entryPath("rel-3"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.setCounter(`{"lastOrderNumber": 0}`) // and the counter was restored backwards

	q2 := h.queue(false) // Recover succeeds
	if !h.log.warned(h.entryPath("rel-3")) {
		t.Error("recovery did not name the damaged file")
	}
	if n := h.counter(); n != 3 {
		t.Errorf("counter after recovery = %d; want 3 (order 3's history folder)", n)
	}
	if _, err := q2.Accept(keyedOrder("rel-3")); !errors.Is(err, ErrOrderIndexUnreadable) {
		t.Errorf("the damaged key = %v; want ErrOrderIndexUnreadable", err)
	}
	if acc := mustAccept(t, q2, keyedOrder("rel-new")); acc.JobID != "4" {
		t.Errorf("a new key = %+v; want job 4 — 3 was issued before", acc)
	}
	if acc := mustAccept(t, q2, keyedOrder("rel-1")); acc.JobID != "1" || acc.Status != JobStatusDone {
		t.Errorf("a readable key = %+v; want job 1 done", acc)
	}
}

// Orders written before the index existed (or whose entries are lost) still
// hold their numbers: the counter is moved past the highest history/<n>.
func TestOrderQueue_CounterBehindTheHistoryIsMovedPastIt(t *testing.T) {
	h := newOrderHarness(t)
	for _, name := range []string{"5", "12", "abc", "007"} {
		if err := os.MkdirAll(filepath.Join(h.sendDir, "history", name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(h.sendDir, "history", "99"), nil, 0o600); err != nil { // a file, not an order
		t.Fatal(err)
	}
	h.setCounter(`{"lastOrderNumber": 3}`)
	q := h.queue(false)
	if n := h.counter(); n != 12 {
		t.Errorf("counter = %d; want 12", n)
	}
	if !h.log.warned("history") {
		t.Error("moving the counter forward was not logged")
	}
	if acc := mustAccept(t, q, keyedOrder("rel-a")); acc.JobID != "13" {
		t.Errorf("a new key = %+v; want job 13", acc)
	}
}

// While the connector runs, a counter moved back below the highest known
// number is refused even when the number it offers is in no index entry (an
// order from before the index), and the next attempt recovers first.
func TestOrderQueue_CounterMovedBelowTheHighestWhileRunning(t *testing.T) {
	h := newOrderHarness(t)
	for i := 1; i <= 5; i++ {
		if err := os.MkdirAll(filepath.Join(h.sendDir, "history", strconv.Itoa(i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	q := h.queue(false)
	if acc := mustAccept(t, q, keyedOrder("rel-a")); acc.JobID != "6" {
		t.Fatalf("first key = %+v; want job 6", acc)
	}
	h.setCounter(`{"lastOrderNumber": 2}`)
	if acc, err := q.Accept(keyedOrder("rel-b")); err == nil {
		t.Fatalf("Accept = %+v; want a refusal — 3 was issued before the index", acc)
	}
	if acc := mustAccept(t, q, keyedOrder("rel-b")); acc.JobID != "7" {
		t.Errorf("the retry = %+v; want job 7", acc)
	}
}

// A known key needs no counter: while recovery cannot run (the counter is
// unreadable) repeats are still answered; only new keys are refused.
func TestOrderQueue_ReplaysNeedNoCounter(t *testing.T) {
	h := newOrderHarness(t)
	q := h.queue(false)
	mustAccept(t, q, keyedOrder("rel-a"))
	h.setCounter(`{`)

	if _, err := q.Accept(keyedOrder("rel-b")); !errors.Is(err, ErrOrderCounterUnreadable) {
		t.Errorf("a new key with an unreadable counter = %v; want ErrOrderCounterUnreadable", err)
	}
	q2 := h.newQueue() // the restart: recovery fails on the counter
	if err := q2.Recover(); !errors.Is(err, ErrOrderCounterUnreadable) {
		t.Fatalf("Recover = %v; want ErrOrderCounterUnreadable", err)
	}
	if acc := mustAccept(t, q2, keyedOrder("rel-a")); acc.JobID != "1" || !acc.Replay {
		t.Errorf("a known key without a counter = %+v; want job 1, replay", acc)
	}
	if _, err := q2.Accept(keyedOrder("rel-c")); !errors.Is(err, ErrOrderCounterUnreadable) {
		t.Errorf("a new key = %v; want ErrOrderCounterUnreadable", err)
	}
	if got := string(readFile(t, filepath.Join(h.sendDir, "lastOrderNumber.json"))); got != "{" {
		t.Errorf("the counter was rewritten to %q", got)
	}
}

// The documented way back for "unknown": with no history copy, a person sets
// the entry's state to retryable and the next repeat writes it under its
// number.
func TestOrderQueue_UnknownSetToRetryableByHandIsWrittenUnderItsNumber(t *testing.T) {
	h := newOrderHarness(t)
	q := h.queue(false)
	mustAccept(t, q, keyedOrder("rel-a")) // accepted, never written: unknown after a restart
	b := readFile(t, h.entryPath("rel-a"))
	edited := strings.Replace(string(b), `"state": "reserved"`, `"state": "retryable"`, 1)
	if edited == string(b) {
		t.Fatalf("entry %s has no reserved state to edit", b)
	}
	if err := os.WriteFile(h.entryPath("rel-a"), []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	q2 := h.queue(true)
	acc := mustAccept(t, q2, keyedOrder("rel-a"))
	if acc.JobID != "1" || acc.Status != JobStatusQueued {
		t.Fatalf("repeat = %+v; want job 1 queued", acc)
	}
	waitForStatus(t, q2, "1", JobStatusDone)
	if _, err := os.Stat(h.historyDoc(1)); err != nil {
		t.Errorf("order 1 not written: %v", err)
	}
}

// An entry that reads retryable while its job is still queued or running in
// this process (the moment between the worker's save and its status update,
// or a hand edit) is never queued a second time.
func TestOrderQueue_RetryableEntryWithALiveJobIsNotQueuedTwice(t *testing.T) {
	h := newOrderHarness(t)
	q := h.queue(false) // not started: the job stays queued
	mustAccept(t, q, keyedOrder("rel-a"))
	b := readFile(t, h.entryPath("rel-a"))
	edited := strings.Replace(string(b), `"state": "reserved"`, `"state": "retryable"`, 1)
	if err := os.WriteFile(h.entryPath("rel-a"), []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	acc := mustAccept(t, q, keyedOrder("rel-a"))
	if acc.JobID != "1" || acc.Status != JobStatusQueued || !acc.Replay {
		t.Fatalf("repeat = %+v; want job 1 queued, replay", acc)
	}
	if p := q.Pending(); p != 1 {
		t.Errorf("queue holds %d jobs; the live job was queued again", p)
	}
}

// plantEntry writes an index entry by hand, as an operator's edit would.
func (h *orderHarness) plantEntry(key string, n int64, state orderState) {
	h.t.Helper()
	b := fmt.Sprintf(`{"key":%q,"number":%d,"state":%q,"reservedAt":"2026-11-05T10:00:00Z"}`, key, n, state)
	if err := os.MkdirAll(OrderIndexDir(h.dataDir), 0o700); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(h.entryPath(key), []byte(b), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *orderHarness) errored(substr string) bool {
	h.log.mu.Lock()
	defer h.log.mu.Unlock()
	for _, e := range h.log.errors {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}

// A retryable entry whose number already has a history copy (a mistaken hand
// edit, a number reissued) is never queued again: that would be a second
// document with Asmahta n. Refused loudly, answered as failed.
func TestOrderQueue_RetryRefusedWhenTheHistoryCopyExists(t *testing.T) {
	h := newOrderHarness(t)
	h.plantEntry("rel-a", 1, orderRetryable)
	if err := os.MkdirAll(filepath.Dir(h.historyDoc(1)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.historyDoc(1), []byte("written"), 0o644); err != nil {
		t.Fatal(err)
	}
	q := h.queue(false)
	acc := mustAccept(t, q, keyedOrder("rel-a"))
	if acc.JobID != "1" || acc.Status != JobStatusFailed || !acc.Replay {
		t.Fatalf("repeat = %+v; want job 1 answered failed, replay", acc)
	}
	if q.Pending() != 0 {
		t.Error("the order was queued again over an existing history copy")
	}
	if !h.errored("IMOVEIN_1.doc already exists") {
		t.Errorf("the refusal was not logged loudly: %v", h.log.errors)
	}
	if e := h.entry("rel-a"); e.State != orderRetryable || e.Retries != 0 {
		t.Errorf("entry = %+v; a refusal must not change it", e)
	}
}

// A retryable entry for a number another key holds is never queued again.
// In process: the other key reserved the number (its entry is in byNumber).
func TestOrderQueue_RetryRefusedWhenAnotherKeyHoldsTheNumber(t *testing.T) {
	h := newOrderHarness(t)
	q := h.queue(true)
	unblock := h.block("IMOVEIN.prm") // rel-a fails final, with no history copy
	waitForStatus(t, q, mustAccept(t, q, keyedOrder("rel-a")).JobID, JobStatusFailed)
	unblock()
	h.plantEntry("rel-b", 1, orderRetryable) // a mistaken hand edit: rel-a's number

	acc := mustAccept(t, q, keyedOrder("rel-b"))
	if acc.JobID != "1" || acc.Status != JobStatusFailed || !acc.Replay {
		t.Fatalf("repeat = %+v; want job 1 answered failed, replay", acc)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(h.historyDoc(1)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("order 1 was written for a second key: %v", err)
	}
	if !h.errored("rel-b") {
		t.Errorf("the refusal was not logged loudly: %v", h.log.errors)
	}
}

// After a restart: two entries hold one number (recovery logs it). Neither is
// queued again, whichever the index map happened to keep.
func TestOrderQueue_RetryRefusedWhenTwoEntriesHoldTheNumber(t *testing.T) {
	for _, other := range []orderState{orderWritten, orderRetryable} {
		t.Run(string(other), func(t *testing.T) {
			h := newOrderHarness(t)
			h.plantEntry("rel-a", 1, other)
			h.plantEntry("rel-b", 1, orderRetryable)
			q := h.queue(false)
			if !h.log.warned("both hold order 1") {
				t.Error("recovery did not report the shared number")
			}
			for _, key := range []string{"rel-a", "rel-b"} {
				acc := mustAccept(t, q, keyedOrder(key))
				if acc.JobID != "1" || acc.Replay != true || acc.Status == JobStatusQueued {
					t.Errorf("repeat of %s = %+v; want job 1 answered, not queued", key, acc)
				}
			}
			if q.Pending() != 0 {
				t.Errorf("queued %d; a shared number must not be written again", q.Pending())
			}
		})
	}
}

// The guard needs the index map; a retry while recovery cannot finish (the
// counter is unreadable) still scans the index first — and still needs no
// counter.
func TestOrderQueue_RetryWithAnUnreadableCounterStillChecksTheIndex(t *testing.T) {
	h := newOrderHarness(t)
	h.plantEntry("rel-a", 1, orderWritten)
	h.plantEntry("rel-b", 1, orderRetryable)
	h.plantEntry("rel-c", 2, orderRetryable)
	h.setCounter(`{`)
	q := h.newQueue()
	if err := q.Recover(); !errors.Is(err, ErrOrderCounterUnreadable) {
		t.Fatalf("Recover = %v; want ErrOrderCounterUnreadable", err)
	}
	if acc := mustAccept(t, q, keyedOrder("rel-b")); acc.Status != JobStatusFailed || q.Pending() != 0 {
		t.Errorf("rel-b = %+v, queued %d; want refused (order 1 is shared)", acc, q.Pending())
	}
	if acc := mustAccept(t, q, keyedOrder("rel-c")); acc.JobID != "2" || acc.Status != JobStatusQueued || q.Pending() != 1 {
		t.Errorf("rel-c = %+v, queued %d; want job 2 queued again without a counter", acc, q.Pending())
	}
}

// A queue whose recovery never ran (it failed before reading the index, or
// was never called) reads the index before it re-queues anything.
func TestOrderQueue_RetryOnAQueueNeverRecoveredScansTheIndexFirst(t *testing.T) {
	h := newOrderHarness(t)
	h.plantEntry("rel-a", 1, orderWritten)
	h.plantEntry("rel-b", 1, orderRetryable)
	q := h.newQueue() // no Recover
	if acc := mustAccept(t, q, keyedOrder("rel-b")); acc.Status != JobStatusFailed || q.Pending() != 0 {
		t.Errorf("rel-b = %+v, queued %d; want refused (order 1 is shared)", acc, q.Pending())
	}
}
