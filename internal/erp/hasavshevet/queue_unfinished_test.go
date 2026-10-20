package hasavshevet

import (
	"context"
	"testing"
	"time"
)

// Unfinished follows a job through its states, so a caller deciding whether
// a restart would lose an order sees it until it is written.
func TestOrderQueue_UnfinishedCountsByState(t *testing.T) {
	q := NewOrderQueue(nil, noopLogger{}) // not started: the states are set by hand
	assertUnfinished(t, q, 0, 0)

	id, err := q.Submit(drainTestOrder(1))
	if err != nil {
		t.Fatal(err)
	}
	assertUnfinished(t, q, 1, 0)

	q.setStatus(id, JobStatusRunning, 0, nil, nil)
	assertUnfinished(t, q, 0, 1)

	q.setStatus(id, JobStatusDone, 0, nil, nil)
	assertUnfinished(t, q, 0, 0)
}

// The worker takes a job off the channel and then waits for the import lock
// (a receipt may hold it). Pending no longer counts that job; Unfinished
// still does — it has not been written.
func TestOrderQueue_UnfinishedCountsAJobWaitingForTheImportLock(t *testing.T) {
	q, _, _ := drainTestQueue(t)
	if err := lockImport(context.Background()); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			unlockImport()
		}
	}()

	if _, err := q.Submit(drainTestOrder(1)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for q.Pending() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the worker never took the job off the channel")
		}
		time.Sleep(5 * time.Millisecond)
	}
	assertUnfinished(t, q, 1, 0)

	unlockImport()
	locked = false
	for {
		queued, running := q.Unfinished()
		if queued == 0 && running == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the order was never written: queued=%d running=%d", queued, running)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func assertUnfinished(t *testing.T, q *OrderQueue, wantQueued, wantRunning int) {
	t.Helper()
	if queued, running := q.Unfinished(); queued != wantQueued || running != wantRunning {
		t.Fatalf("Unfinished = (%d, %d), want (%d, %d)", queued, running, wantQueued, wantRunning)
	}
}
