package hasavshevet

import (
	"context"
	"time"
)

// ImportLock serialises everything that writes Hashavshevet import files or
// runs an importer: the order worker (IMOVEIN + has.exe/digi.bat) and receipt
// writes (KUPAIN + the receipt BAT). One process, one lock, so an order import
// and a receipt never overlap on the ERP node.
//
// It is a one-slot channel rather than a sync.Mutex because receipts must be
// able to give up: a caller waiting on a request deadline cannot block forever
// behind a slow order import.
type ImportLock struct {
	ch chan struct{}
}

// NewImportLock returns an unlocked lock.
func NewImportLock() *ImportLock {
	return &ImportLock{ch: make(chan struct{}, 1)}
}

// Acquire blocks until the lock is held or ctx ends.
func (l *ImportLock) Acquire(ctx context.Context) error {
	select {
	case l.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// AcquireWithin waits at most d for the lock. It reports false when the wait
// ran out; a ctx that ends first returns its error.
func (l *ImportLock) AcquireWithin(ctx context.Context, d time.Duration) (bool, error) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case l.ch <- struct{}{}:
		return true, nil
	case <-timer.C:
		return false, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// Release frees the lock. Releasing an unheld lock is a programming error and
// panics rather than silently letting two importers run.
func (l *ImportLock) Release() {
	select {
	case <-l.ch:
	default:
		panic("hasavshevet: ImportLock released while not held")
	}
}
