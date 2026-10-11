package hasavshevet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

func drainTestQueue(t *testing.T) (*OrderQueue, string, context.CancelFunc) {
	t.Helper()
	dir := t.TempDir()
	sender := NewSender(nil, config.Config{SendOrderDir: dir},
		NewOrderNumberStore(filepath.Join(dir, "lastOrderNumber.json")), noopLogger{})
	q := NewOrderQueue(sender, noopLogger{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	q.Start(ctx)
	return q, dir, cancel
}

func drainTestOrder(i int) OrderRequest {
	return OrderRequest{
		DocumentType: "ORDER", UserExtID: "1234", DueDate: "2026-10-11", CreatedDate: "2026-10-11",
		HistoryID: "H-" + strconv.Itoa(i), Currency: "ILS", Total: 50,
		Account: &AccountDetails{AccountKey: "1234", FullName: "Test"},
		Details: []OrderLineItem{{Title: "Widget", SKU: "1001", Quantity: 1, SinglePrice: 50, TotalPrice: 50}},
	}
}

// Stopping the daemon must not lose orders already accepted with 202: Drain
// lets the worker finish every queued job before it returns.
func TestOrderQueue_DrainFinishesEveryQueuedOrder(t *testing.T) {
	q, dir, _ := drainTestQueue(t)

	// Hold the worker back so the three orders are all still queued.
	if err := lockImport(context.Background()); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := 0; i < 3; i++ {
		id, err := q.Submit(drainTestOrder(i))
		if err != nil {
			unlockImport()
			t.Fatalf("Submit: %v", err)
		}
		ids = append(ids, id)
	}

	drained := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		drained <- q.Drain(ctx)
	}()
	time.Sleep(50 * time.Millisecond) // Drain is now waiting on queued work
	unlockImport()

	if err := <-drained; err != nil {
		t.Fatalf("Drain: %v", err)
	}
	for _, id := range ids {
		st, _ := q.Status(id)
		if st == nil || st.Status != JobStatusDone {
			t.Errorf("order %s after Drain = %+v; want done", id, st)
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "history", id)); err != nil {
			t.Errorf("order %s has no history: %v", id, err)
		}
	}
}

// After Drain nothing is accepted — and Submit says so instead of panicking on
// the closed channel.
func TestOrderQueue_SubmitAfterDrainIsRefused(t *testing.T) {
	q, _, _ := drainTestQueue(t)
	if err := q.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if _, err := q.Submit(drainTestOrder(1)); !errors.Is(err, ErrQueueClosed) {
		t.Fatalf("Submit after Drain = %v; want ErrQueueClosed", err)
	}
	// Idempotent.
	if err := q.Drain(context.Background()); err != nil {
		t.Errorf("second Drain: %v", err)
	}
	q.Stop()
}

// Drain gives up when its context ends and says how much was left.
func TestOrderQueue_DrainHonoursItsDeadline(t *testing.T) {
	q, _, _ := drainTestQueue(t)
	if err := lockImport(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer unlockImport()
	if _, err := q.Submit(drainTestOrder(1)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := q.Drain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain = %v; want the deadline", err)
	}
}
