package hasavshevet

import (
	"context"
	"sync"
)

// importLock serialises everything that writes Hashavshevet import files and
// runs its importer: the order worker (IMOVEIN + has.exe / digi.bat) and the
// receipt writer (KUPAIN + the receipt BAT). Hashavshevet imports one batch at
// a time, and the two folders may share an importer, so an order job and a
// receipt never run at once.
//
// It is process-wide on purpose — one importer per machine — so no wiring can
// leave the two paths holding different locks. It is a one-slot channel rather
// than a sync.Mutex so a receipt request can stop waiting when its caller has
// gone, before it has reserved anything.
var importLock = make(chan struct{}, 1)

// lockImport takes the import lock, or returns ctx's error if ctx ends first.
func lockImport(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err // select would pick at random between a free lock and a done ctx
	}
	select {
	case importLock <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// unlockImport releases the lock taken by lockImport.
func unlockImport() { <-importLock }

// HoldImports takes the import lock and returns the function that releases
// it, or ctx's error. While it is held no order job and no receipt runs. It
// exists for tests outside this package that need the importer idle (the
// daemon's shutdown test holds queued orders behind it); production code takes
// the lock only through the order worker and the receipt writer.
func HoldImports(ctx context.Context) (release func(), err error) {
	if err := lockImport(ctx); err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(unlockImport) }, nil
}
