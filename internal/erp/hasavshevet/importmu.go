package hasavshevet

import "context"

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
