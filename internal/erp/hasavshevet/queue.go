package hasavshevet

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/logger"
)

const defaultQueueSize = 64

// JobStatus represents the lifecycle state of an enqueued order job.
type JobStatus string

const (
	JobStatusQueued  JobStatus = "queued"
	JobStatusRunning JobStatus = "running"
	JobStatusDone    JobStatus = "done"
	JobStatusFailed  JobStatus = "failed"
	// JobStatusUnknown is a key index entry still reserved after a restart:
	// the order was accepted, and whether its files were written (and
	// imported) was never recorded. It is never written again; a person — or
	// a lookup in Hashavshevet — decides.
	JobStatusUnknown JobStatus = "unknown"
)

// Crash points for tests: where a process kill is simulated in Accept.
const (
	crashOrderAfterIndexReserved   = "order-index-reserved"   // the index owns the number; the counter has not moved on
	crashOrderAfterCounterAdvanced = "order-counter-advanced" // the counter has moved on; the job is not queued yet
)

// JobResult holds the outcome of a processed order job.
type JobResult struct {
	ID           string
	Status       JobStatus
	OrderNumber  int64
	WrittenFiles []string
	Err          error
}

type orderJob struct {
	id          string
	orderNumber int64
	req         OrderRequest
	// entry is the key index entry that owns orderNumber; nil without a key
	// index. Only the worker touches it once the job is queued.
	entry *orderIndexEntry
}

// OrderQueue is a single-worker async queue for Hasavshevet send-order jobs.
//
// Using a single worker guarantees that only one goroutine writes to
// IMOVEIN.doc/.prm and executes has.exe at a time, preventing file collisions.
// Jobs are processed in FIFO order.
type OrderQueue struct {
	ch     chan orderJob
	sender *Sender
	log    logger.LoggerService
	done   chan struct{} // closed when the worker exits

	mu   sync.RWMutex
	jobs map[string]*JobResult
	// byNumber maps every order number the key index holds to its entry's
	// file stem (guarded by mu): how Status answers a job after a restart, and
	// how a re-queue checks that its number is its own. sharedNumbers holds the
	// numbers two readable entries both claim (recovery logs them).
	byNumber      map[int64]string
	sharedNumbers map[int64]bool

	// closeMu guards closed and every send on ch, so Submit never sends on a
	// closed channel and Stop never closes it twice. It also serialises every
	// Accept, so two requests with one historyId cannot both miss the index.
	closeMu sync.Mutex
	closed  bool

	// index is the historyId → order number index (UseKeyIndex); nil without
	// one, and then every Submit is a new order.
	index *orderIndex
	// recovered is guarded by closeMu. False until Recover succeeds, and set
	// back to false when the counter could not be moved past a number the
	// index owns, so no new number is issued until recovery has repaired it.
	recovered bool
	// scanned is guarded by closeMu: a recovery has read the index into
	// byNumber, even if it then failed on the counter.
	scanned bool
	// highest is the highest order number known to be issued (guarded by
	// closeMu): readable index entries and sendOrderDir/history/<n> at
	// recovery, then every number reserved since. The counter must offer more.
	highest int64

	crashPoint func(point string) // test seam
}

// ErrQueueClosed is Submit's answer once the queue is draining or stopped.
var ErrQueueClosed = errors.New("order queue is closed (the connector is stopping)")

// ErrQueueFull is Submit's answer when defaultQueueSize orders are waiting.
var ErrQueueFull = errors.New("order queue full")

// Acceptance is Accept's answer: the job that holds the order. For a new order
// Status is queued; for a repeat of a known historyId (Replay) it is that
// order's current state — queued, running, done, failed, or unknown — and
// queued again when the repeat re-queued a retryable order.
type Acceptance struct {
	JobID  string
	Status JobStatus
	Replay bool
}

// NewOrderQueue creates a new queue. Call Start to begin processing.
func NewOrderQueue(sender *Sender, log logger.LoggerService) *OrderQueue {
	return &OrderQueue{
		ch:            make(chan orderJob, defaultQueueSize),
		sender:        sender,
		log:           log,
		jobs:          make(map[string]*JobResult),
		byNumber:      make(map[int64]string),
		sharedNumbers: make(map[int64]bool),
		done:          make(chan struct{}),
	}
}

// UseKeyIndex makes historyId the order's idempotency key: one order number
// per historyId, persisted under OrderIndexDir(dataDir), so a retry, a
// redelivery or a resend answers the first order instead of writing a second
// Hashavshevet document. Call it before Recover, Start and the first Submit.
// The queue needs a sender with an order number store.
func (q *OrderQueue) UseKeyIndex(dataDir string) {
	q.closeMu.Lock()
	defer q.closeMu.Unlock()
	q.index = &orderIndex{dir: OrderIndexDir(dataDir)}
	q.recovered = false
}

// Recover reads the key index and moves the order counter past the highest
// number issued — the highest readable entry, or the highest
// sendOrderDir/history/<n> if higher (orders written before the index, or
// whose entry is damaged) — since a crash between saving an entry and moving
// the counter leaves the counter behind. It logs every entry it cannot read
// (and skips it: only that key is refused) and every order whose outcome is
// unknown. It writes no order and never changes an entry. Accept runs it
// before issuing a new number when it has not succeeded yet; a repeat of a
// known key needs no recovery.
func (q *OrderQueue) Recover() error {
	q.closeMu.Lock()
	defer q.closeMu.Unlock()
	if q.index == nil {
		return nil
	}
	return q.recoverLocked()
}

func (q *OrderQueue) recoverLocked() error {
	store := q.numberStore()
	if store == nil {
		return errors.New("order index: the queue has no order number store")
	}
	if err := privateDirs(filepath.Dir(q.index.dir), q.index.dir); err != nil {
		return fmt.Errorf("prepare the order index: %w", err)
	}
	entries, problems, err := q.index.all()
	if err != nil {
		return fmt.Errorf("recover the order index: %w", err)
	}
	for _, p := range problems {
		q.log.Warn(fmt.Sprintf("ORDER INDEX ENTRY SKIPPED: %v. Its historyId is answered 500 ORDER_INDEX_UNREADABLE until a person repairs or moves the file (docs/troubleshooting.md); every other order goes on.", p))
	}
	byNumber := make(map[int64]string, len(entries))
	shared := make(map[int64]bool)
	var highestEntry *orderIndexEntry
	for _, e := range entries {
		if stem, dup := byNumber[e.Number]; dup && stem != orderIndexStem(e.Key) {
			shared[e.Number] = true
		}
		byNumber[e.Number] = orderIndexStem(e.Key)
		highestEntry = e // sorted by number
	}
	sendDir := q.sender.cfg.SendOrderDir

	// The map first: a re-queue needs it and no counter, so it is in place
	// even when the counter step below fails.
	q.mu.Lock()
	q.byNumber = byNumber
	q.sharedNumbers = shared
	var unknown []*orderIndexEntry
	for _, e := range entries {
		if _, inMemory := q.jobs[strconv.FormatInt(e.Number, 10)]; e.State == orderReserved && !inMemory {
			unknown = append(unknown, e)
		}
	}
	q.mu.Unlock()
	q.scanned = true
	for _, e := range unknown {
		n := strconv.FormatInt(e.Number, 10)
		q.log.Warn(fmt.Sprintf("ORDER OUTCOME UNKNOWN: order %d (historyId=%q, reserved %s) was accepted but the connector stopped before recording whether it was written. "+
			"It will NOT be written again on its own; a repeat of the key answers status \"unknown\". If %s does not exist the connector never ran the importer for it: "+
			"set \"state\" to \"retryable\" in %s and the next repeat writes it under order %d. If it exists, check Hashavshevet (Asmahta %d) first (docs/troubleshooting.md).",
			e.Number, e.Key, e.ReservedAt, filepath.Join(sendDir, "history", n, "IMOVEIN_"+n+".doc"),
			q.index.pathFor(orderIndexStem(e.Key)), e.Number, e.Number))
	}

	highestHistory, err := highestHistoryNumber(sendDir)
	if err != nil {
		return fmt.Errorf("recover the order index: %w", err)
	}
	highest := highestHistory
	if highestEntry != nil && highestEntry.Number > highest {
		highest = highestEntry.Number
	}

	if highest > 0 {
		next, err := store.Peek()
		if err != nil {
			return fmt.Errorf("recover the order index: %w", err)
		}
		if next <= highest {
			switch {
			case highestEntry != nil && highestEntry.Number == highest && highestEntry.State == orderReserved && next == highest:
				q.log.Warn(fmt.Sprintf("order %d was reserved for historyId=%q but lastOrderNumber.json was not moved past it (the connector stopped, or the counter could not be written, in between); moving it past %d",
					highest, highestEntry.Key, highest))
			case highest == highestHistory:
				// Not a crash: the counter was moved back (restored, hand-edited),
				// or orders were written before the index existed.
				q.log.Warn(fmt.Sprintf("lastOrderNumber.json offered %d but %s already holds order %d; moving the counter past %d",
					next, filepath.Join(sendDir, "history"), highest, highest))
			default:
				q.log.Warn(fmt.Sprintf("lastOrderNumber.json offered %d but the order index already holds orders up to %d; moving the counter past %d",
					next, highest, highest))
			}
			if err := store.advancePast(highest); err != nil {
				return fmt.Errorf("recover the order index: %w", err)
			}
		}
	}
	q.highest = highest
	q.recovered = true
	return nil
}

// Start launches the single background worker goroutine.
// The goroutine exits when ctx is cancelled or Stop is called.
func (q *OrderQueue) Start(ctx context.Context) {
	go q.run(ctx)
}

func (q *OrderQueue) run(ctx context.Context) {
	defer close(q.done)
	defer q.abandonQueued(ctx) // before done: Stop waits for it
	for {
		select {
		case <-ctx.Done():
			return
		case job, ok := <-q.ch:
			if !ok {
				return
			}
			// One import at a time across orders and receipts (importmu.go).
			if err := lockImport(ctx); err != nil {
				q.abandon(job, err) // ctx cancelled while a receipt held the lock
				return
			}
			q.setStatus(job.id, JobStatusRunning, job.orderNumber, nil, nil)
			result, err := q.sender.processOrderWithNumber(ctx, job.req, job.orderNumber)
			unlockImport()
			// The index first, the in-memory status second: a poll that reads
			// done/failed is already backed by disk. Crash window: a process
			// that dies after the files are written (and the importer ran) but
			// before this save leaves the entry reserved — after a restart the
			// key answers "unknown" and is never written again.
			q.recordOutcome(job, result, err)
			if err != nil {
				q.log.Error(fmt.Sprintf("order job %s failed", job.id), err)
				q.setStatus(job.id, JobStatusFailed, job.orderNumber, nil, err)
			} else {
				q.log.Success(fmt.Sprintf("order job %s done orderNumber=%d files=%v", job.id, result.OrderNumber, result.WrittenFiles))
				q.setStatus(job.id, JobStatusDone, result.OrderNumber, result.WrittenFiles, nil)
			}
		}
	}
}

// abandon records a job the worker gives up before starting it — the worker
// is stopping — as not written: its key stays retryable, not unknown.
func (q *OrderQueue) abandon(job orderJob, cause error) {
	err := notWritten(fmt.Errorf("the connector stopped before writing this order: %w", cause))
	q.recordOutcome(job, nil, err)
	q.setStatus(job.id, JobStatusFailed, job.orderNumber, nil, err)
	q.log.Warn(fmt.Sprintf("order job %s not started: the connector is stopping; a repeat of its historyId writes it under the same number", job.id))
}

// abandonQueued abandons every job still in the channel when the worker
// exits (its context ended: the drain timed out). On a normal exit the
// channel is closed and empty, and this does nothing.
func (q *OrderQueue) abandonQueued(ctx context.Context) {
	for {
		select {
		case job, ok := <-q.ch:
			if !ok {
				return
			}
			q.abandon(job, ctx.Err())
		default:
			return
		}
	}
}

// Done is closed when the worker has exited — after Drain finished the queue,
// or after its context ended and the unstarted jobs were recorded.
func (q *OrderQueue) Done() <-chan struct{} { return q.done }

// Submit is Accept for callers that need only the job ID.
func (q *OrderQueue) Submit(req OrderRequest) (string, error) {
	acc, err := q.Accept(req)
	return acc.JobID, err
}

// Accept enqueues an order request, or — with the key index on — answers the
// order its historyId already has.
//
// A new order: the job ID is the reserved order number as a decimal string
// (random and opaque only when the queue has no number store, as in some
// tests). Returns ErrQueueClosed once the queue is stopping, or ErrQueueFull;
// neither refusal reserves a number.
//
// With the key index (UseKeyIndex) a historyId seen before is answered with
// its first job ID and current state, Replay true: no number is reserved,
// nothing is queued, no file is written — in every state but one. A retryable
// order (it failed before any file was written) is queued again under its
// SAME number: the entry is saved reserved first, so racing repeats see it
// queued. A repeat needs no counter and no recovery. An entry that cannot be
// read refuses with ErrOrderIndexUnreadable and is never given a second
// number. Otherwise, in this order: the counter is read (Peek), the entry that
// owns the number is saved (reserved), then the counter is moved past it, then
// the job is queued.
//
// The whole of Accept runs under closeMu: the closed and capacity checks, the
// index lookup, the reservation and the send. Checked before taking closeMu, a
// Stop landing between the check and the send burned the number just
// reserved, and two requests with one key could both miss the index. Only
// Accept sends on ch, so a free slot seen under closeMu is still free at the
// send.
// ValidateLayout is the sender's ValidateLayout: whether the configured IMOVEIN
// layout (sendOrderFormat) can hold req as written. The route calls it before
// Accept, so a request it refuses is a 400 and never a number given away. nil
// for a queue without a sender.
func (q *OrderQueue) ValidateLayout(req OrderRequest) error {
	if q == nil || q.sender == nil {
		return nil
	}
	return q.sender.ValidateLayout(req)
}

func (q *OrderQueue) Accept(req OrderRequest) (Acceptance, error) {
	q.closeMu.Lock()
	defer q.closeMu.Unlock()
	if q.closed {
		return Acceptance{}, ErrQueueClosed
	}
	if q.index == nil {
		return q.acceptUnkeyedLocked(req)
	}
	acc, err := q.acceptKeyedLocked(req)
	if err != nil && !errors.Is(err, ErrQueueFull) {
		q.log.Error(fmt.Sprintf("sendOrder historyId=%q not accepted", req.HistoryID), err)
	}
	return acc, err
}

func (q *OrderQueue) acceptUnkeyedLocked(req OrderRequest) (Acceptance, error) {
	if len(q.ch) >= cap(q.ch) {
		return Acceptance{}, fmt.Errorf("%w (capacity %d)", ErrQueueFull, defaultQueueSize)
	}
	jobID, orderNumber, err := q.reserveJobIdentity()
	if err != nil {
		return Acceptance{}, err
	}
	return q.enqueueLocked(orderJob{id: jobID, orderNumber: orderNumber, req: req})
}

func (q *OrderQueue) acceptKeyedLocked(req OrderRequest) (Acceptance, error) {
	store := q.numberStore()
	if store == nil {
		return Acceptance{}, errors.New("order index: the queue has no order number store")
	}
	key := req.HistoryID
	if key == "" {
		return Acceptance{}, errors.New("historyId is required: it is the order's idempotency key")
	}

	e, found, err := q.index.load(key)
	if err != nil {
		return Acceptance{}, err
	}
	if found {
		if e.State == orderRetryable && !q.jobActive(e.Number) {
			return q.requeueLocked(e, req)
		}
		return q.replay(e, req), nil
	}

	if !q.recovered {
		if err := q.recoverLocked(); err != nil {
			return Acceptance{}, err
		}
	}
	if len(q.ch) >= cap(q.ch) {
		return Acceptance{}, fmt.Errorf("%w (capacity %d)", ErrQueueFull, defaultQueueSize)
	}
	n, err := store.Peek()
	if err != nil {
		return Acceptance{}, fmt.Errorf("reserve order number: %w", err)
	}
	if n <= q.highest {
		// Recovery put the counter past every number known to be issued, so
		// the file was moved back while the connector ran. Refuse, and have
		// the next Accept recover (move it forward) before issuing anything.
		q.recovered = false
		return Acceptance{}, fmt.Errorf("lastOrderNumber.json offers order %d, but orders up to %d were already issued; nothing reserved", n, q.highest)
	}

	e = &orderIndexEntry{
		Key:         key,
		Number:      n,
		State:       orderReserved,
		RequestHash: orderRequestHash(req),
		ReservedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	if err := q.index.save(e); err != nil {
		return Acceptance{}, err // nothing reserved: the counter has not moved
	}
	q.highest = n
	q.mu.Lock()
	q.byNumber[n] = orderIndexStem(key)
	q.mu.Unlock()
	q.crash(crashOrderAfterIndexReserved)
	if err := store.advancePast(n); err != nil {
		// The index owns n, so this order goes ahead under it — refusing would
		// leave the key reserved and its order never written. But the counter
		// still offers n: issue nothing more until recovery has moved it on.
		q.recovered = false
		q.log.Error(fmt.Sprintf("order %d reserved for historyId=%q, but lastOrderNumber.json could not be moved past it; no new order is accepted until it can", n, key), err)
	}
	q.crash(crashOrderAfterCounterAdvanced)

	acc, err := q.enqueueLocked(orderJob{id: strconv.FormatInt(n, 10), orderNumber: n, req: req, entry: e})
	if err != nil { // unreachable: see enqueueLocked
		q.recordOutcome(orderJob{entry: e}, nil, notWritten(fmt.Errorf("never queued: %w", err)))
		return acc, err
	}
	// The log is where a person maps a historyId to its number and file.
	q.log.Info(fmt.Sprintf("sendOrder historyId=%q: order %d reserved and queued (index %s)", key, n, q.index.pathFor(orderIndexStem(key))))
	return acc, nil
}

// requeueLocked queues a retryable order again under its own number: nothing
// of it was written, so writing it now is its first write. The entry is saved
// reserved BEFORE the job is sent, so a racing repeat sees it queued and never
// queues it twice. No counter is involved. Caller holds closeMu.
func (q *OrderQueue) requeueLocked(e *orderIndexEntry, req OrderRequest) (Acceptance, error) {
	// The guard below needs the index map. Recovery fills it before its
	// counter step, so a counter it cannot read does not stop a re-queue.
	if !q.recovered {
		if err := q.recoverLocked(); err != nil && !q.scanned {
			return Acceptance{}, err
		}
	}
	if reason := q.requeueConflict(e); reason != "" {
		q.log.Error(fmt.Sprintf("ORDER NOT QUEUED AGAIN: order %d (historyId=%q) is retryable, but %s. Writing it would make a second document with Asmahta %d; answering it as failed. "+
			"A person must check the entry %s (docs/troubleshooting.md § The order index).",
			e.Number, e.Key, reason, e.Number, q.index.pathFor(orderIndexStem(e.Key))), nil)
		return Acceptance{JobID: strconv.FormatInt(e.Number, 10), Status: JobStatusFailed, Replay: true}, nil
	}
	if len(q.ch) >= cap(q.ch) {
		return Acceptance{}, fmt.Errorf("%w (capacity %d)", ErrQueueFull, defaultQueueSize)
	}
	lastErr := e.Error
	e.State = orderReserved
	e.Error = ""
	e.FinishedAt = ""
	e.Retries++
	e.RequestHash = orderRequestHash(req)
	if err := q.index.save(e); err != nil {
		return Acceptance{}, err // still retryable on disk; nothing queued
	}
	q.mu.Lock()
	q.byNumber[e.Number] = orderIndexStem(e.Key)
	q.mu.Unlock()
	acc, err := q.enqueueLocked(orderJob{id: strconv.FormatInt(e.Number, 10), orderNumber: e.Number, req: req, entry: e})
	if err != nil { // unreachable: see enqueueLocked
		q.recordOutcome(orderJob{entry: e}, nil, notWritten(fmt.Errorf("never queued: %w", err)))
		return acc, err
	}
	q.log.Info(fmt.Sprintf("sendOrder historyId=%q repeated: order %d failed before anything was written (%s); queued again under the same number (retry %d)",
		e.Key, e.Number, lastErr, e.Retries))
	acc.Replay = true
	return acc, nil
}

// requeueConflict says why a retryable entry's number is not safely its own
// to write, or "" when it is: a history copy of the order already exists (a
// hand edit gone wrong, a number reissued), or another entry holds the number.
func (q *OrderQueue) requeueConflict(e *orderIndexEntry) string {
	n := strconv.FormatInt(e.Number, 10)
	doc := filepath.Join(q.sender.cfg.SendOrderDir, "history", n, "IMOVEIN_"+n+".doc")
	if _, err := os.Lstat(doc); err == nil {
		return doc + " already exists"
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Sprintf("%s cannot be checked (%v)", doc, err)
	}
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.sharedNumbers[e.Number] {
		return "two index entries hold order " + n
	}
	if stem, ok := q.byNumber[e.Number]; ok && stem != orderIndexStem(e.Key) {
		return "another historyId's entry holds order " + n
	}
	return ""
}

// jobActive reports whether order n is queued or running in this process.
func (q *OrderQueue) jobActive(n int64) bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	r, ok := q.jobs[strconv.FormatInt(n, 10)]
	return ok && (r.Status == JobStatusQueued || r.Status == JobStatusRunning)
}

// enqueueLocked records the job as queued and sends it to the worker. Caller
// holds closeMu and has checked capacity.
func (q *OrderQueue) enqueueLocked(job orderJob) (Acceptance, error) {
	q.mu.Lock()
	q.jobs[job.id] = &JobResult{
		ID:          job.id,
		Status:      JobStatusQueued,
		OrderNumber: job.orderNumber,
	}
	q.mu.Unlock()

	select {
	case q.ch <- job:
		return Acceptance{JobID: job.id, Status: JobStatusQueued}, nil
	default: // unreachable while only Accept sends, and only under closeMu
		q.forget(job.id)
		return Acceptance{}, fmt.Errorf("%w (capacity %d)", ErrQueueFull, defaultQueueSize)
	}
}

// replay answers a key the index already holds. It writes nothing.
func (q *OrderQueue) replay(e *orderIndexEntry, req OrderRequest) Acceptance {
	jobID := strconv.FormatInt(e.Number, 10)
	status := entryStatus(e)
	q.mu.RLock()
	if r, ok := q.jobs[jobID]; ok {
		status = r.Status // queued or running now, or finished in this process
	}
	q.mu.RUnlock()

	if e.RequestHash != "" && e.RequestHash != orderRequestHash(req) {
		q.log.Warn(fmt.Sprintf("sendOrder historyId=%q repeated with a different body; answering order %d as first accepted (%s)", e.Key, e.Number, status))
	}
	if status == JobStatusUnknown {
		q.log.Warn(fmt.Sprintf("sendOrder historyId=%q repeated: order %d's outcome is UNKNOWN (reserved, never recorded as written or failed); answering it, not writing it again", e.Key, e.Number))
	} else {
		q.log.Info(fmt.Sprintf("sendOrder historyId=%q repeated: answering order %d (%s); nothing written", e.Key, e.Number, status))
	}
	return Acceptance{JobID: jobID, Status: status, Replay: true}
}

// entryStatus is the job status an entry stands for when no job in memory
// says more. A retryable order reads as failed: it did fail, and a repeat of
// its key queues it again.
func entryStatus(e *orderIndexEntry) JobStatus {
	switch e.State {
	case orderWritten:
		return JobStatusDone
	case orderFailed, orderRetryable:
		return JobStatusFailed
	default:
		return JobStatusUnknown
	}
}

// recordOutcome saves the worker's result on the job's index entry: written
// with its files; retryable with the error when it failed before any file was
// written (ErrOrderNotWritten) — a repeat queues it again under its number;
// failed with the error otherwise — never queued again. A save that fails is
// logged: the entry stays reserved, so after a restart the key answers unknown
// (still never written twice).
func (q *OrderQueue) recordOutcome(job orderJob, result *OrderResult, procErr error) {
	e := job.entry
	if e == nil || q.index == nil {
		return
	}
	outcome := "written"
	switch {
	case errors.Is(procErr, ErrOrderNotWritten):
		outcome = "failed before writing anything (retryable)"
		e.State = orderRetryable
		e.Error = orderErrorText(procErr)
	case procErr != nil:
		outcome = "failed"
		e.State = orderFailed
		e.Error = orderErrorText(procErr)
	default:
		e.State = orderWritten
		e.Files = append([]string(nil), result.WrittenFiles...)
	}
	e.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	if err := q.index.save(e); err != nil {
		q.log.Error(fmt.Sprintf("order %d (historyId=%q) %s, but the order index could not record it; after a restart a repeat of the key answers unknown",
			e.Number, e.Key, outcome), err)
	}
}

// highestHistoryNumber is the highest order number with a folder under
// sendOrderDir/history (0 when there is none): orders the connector began to
// write, whatever the index says.
func highestHistoryNumber(sendOrderDir string) (int64, error) {
	if strings.TrimSpace(sendOrderDir) == "" {
		return 0, nil
	}
	dir := filepath.Join(sendOrderDir, "history")
	des, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("list %s: %w", dir, err)
	}
	var highest int64
	for _, de := range des {
		n, err := strconv.ParseInt(de.Name(), 10, 64)
		if err != nil || !de.IsDir() || strconv.FormatInt(n, 10) != de.Name() {
			continue
		}
		if n > highest {
			highest = n
		}
	}
	return highest, nil
}

func (q *OrderQueue) numberStore() *OrderNumberStore {
	if q.sender == nil {
		return nil
	}
	return q.sender.numberStore
}

func (q *OrderQueue) crash(point string) {
	if q.crashPoint != nil {
		q.crashPoint(point)
	}
}

// orderRequestHash fingerprints a request, to notice a key repeated with a
// different body. The entry keeps the hash, not the order.
func orderRequestHash(req OrderRequest) string {
	b, err := json.Marshal(req)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (q *OrderQueue) forget(jobID string) {
	q.mu.Lock()
	delete(q.jobs, jobID)
	q.mu.Unlock()
}

// Pending is the number of orders accepted but not yet started.
func (q *OrderQueue) Pending() int { return len(q.ch) }

// Unfinished counts the orders accepted (202) and not written yet, by state:
// queued — which includes one the worker has taken off the channel and is
// still waiting to import (lockImport), so Pending would no longer count it —
// and running. A restart is safe only when both are 0 (GET /api/sendOrder):
// the queue lives in memory.
func (q *OrderQueue) Unfinished() (queued, running int) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	for _, r := range q.jobs {
		switch r.Status {
		case JobStatusQueued:
			queued++
		case JobStatusRunning:
			running++
		}
	}
	return queued, running
}

// reserveJobIdentity reserves the next order number when available and uses it
// as the externally visible job ID (for backward compatibility with clients
// that expect sendOrder's jobId to carry the Hasavshevet order number).
//
// When queue sender/number store is unavailable (e.g. unit tests), it falls
// back to a random opaque job ID.
func (q *OrderQueue) reserveJobIdentity() (string, int64, error) {
	if q.sender == nil || q.sender.numberStore == nil {
		return newJobID(), 0, nil
	}

	orderNumber, err := q.sender.numberStore.Next()
	if err != nil {
		return "", 0, fmt.Errorf("reserve order number: %w", err)
	}

	return strconv.FormatInt(orderNumber, 10), orderNumber, nil
}

// Status returns the current result for a job ID, or false if not found.
// With the key index, a job this process does not have in memory (it was
// accepted before a restart) is answered from its index entry: done, failed,
// or unknown for an entry still reserved.
func (q *OrderQueue) Status(jobID string) (*JobResult, bool) {
	q.mu.RLock()
	r, ok := q.jobs[jobID]
	n, err := strconv.ParseInt(jobID, 10, 64)
	stem, indexed := "", false
	if !ok && err == nil && strconv.FormatInt(n, 10) == jobID {
		stem, indexed = q.byNumber[n]
	}
	q.mu.RUnlock()
	if ok || !indexed || q.index == nil {
		return r, ok
	}

	e, found, err := q.index.loadStem(stem)
	if err != nil {
		q.log.Error(fmt.Sprintf("order status %s: the index entry cannot be read; answering unknown", jobID), err)
		return &JobResult{ID: jobID, Status: JobStatusUnknown, OrderNumber: n}, true
	}
	if !found || e.Number != n {
		return nil, false
	}
	res := &JobResult{ID: jobID, Status: entryStatus(e), OrderNumber: n, WrittenFiles: e.Files}
	if e.State == orderFailed || e.State == orderRetryable {
		res.Err = errors.New(e.Error)
	}
	return res, true
}

// Stop closes the queue: Submit answers ErrQueueClosed from now on, and the
// worker exits once it has processed every job already queued. Safe to call
// more than once.
func (q *OrderQueue) Stop() {
	q.closeMu.Lock()
	defer q.closeMu.Unlock()
	if !q.closed {
		q.closed = true
		close(q.ch)
	}
}

// Drain stops the queue and waits until the worker has finished every order
// already queued — orders the caller was told were accepted (202) — or until
// ctx ends. Cancel the worker's context only after Drain: cancelling first is
// what lost queued orders on shutdown before.
func (q *OrderQueue) Drain(ctx context.Context) error {
	q.Stop()
	select {
	case <-q.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (q *OrderQueue) setStatus(id string, status JobStatus, orderNumber int64, files []string, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.jobs[id] = &JobResult{
		ID:           id,
		Status:       status,
		OrderNumber:  orderNumber,
		WrittenFiles: files,
		Err:          err,
	}
}

func newJobID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
