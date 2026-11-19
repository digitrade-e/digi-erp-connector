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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
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
	// Import is the order's import check (import_check.go); nil when no
	// importer ran or none was recorded. A report only: Status is unchanged.
	Import *ImportCheck
}

type orderJob struct {
	id          string
	orderNumber int64
	req         OrderRequest
	// entry is the key index entry that owns orderNumber; nil without a key
	// index. Only the worker touches it once the job is queued.
	entry *orderIndexEntry
	// dest is where an order with a documentRule goes (plan 087): the rule
	// frozen in its entry at reservation, never the repeat's. nil = today's
	// destination.
	dest *orderDestination
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
	// importsDir keeps each order's import check (OrderImportsDir; set by
	// UseKeyIndex); "" = kept in memory only.
	importsDir string
	// counters are the order counters (plan 087, UseCounters); nil = the
	// sender's order counter under the implicit key, today's behaviour.
	counters *CounterSet
	// recovered is guarded by closeMu, per counter key. False until a
	// recovery moved that counter past every number issued in its range, and
	// set back to false when it could not, so no new number is issued from it
	// until recovery has repaired it. Each counter recovers on its own: one
	// that cannot be read never stops another.
	recovered map[string]bool
	// scanned is guarded by closeMu: a recovery has read the index into
	// byNumber, even if it then failed on a counter.
	scanned bool
	// highest is, per counter key, the highest number known to be issued in
	// its range (guarded by closeMu): readable index entries and
	// sendOrderDir/history/<n> at recovery, then every number reserved since.
	// The counter must offer more.
	highest map[string]int64
	// guard (guarded by closeMu) says whether a typed folder may be written
	// into: behind the link, not while accounts besides the administrators
	// can write to it (review M1, R3; UseFolderGuard). nil: every folder.
	guard FolderAllower
	// importerGuard (guarded by closeMu) says whether the order importer may
	// run (review T1; UseImporterGuard). nil: always.
	importerGuard FolderAllower
	// gen counts what moves a counter here — reservations, recoveries — for
	// the read route's cache (review m13; Generation).
	gen atomic.Uint64

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
	// Rule is the documentRule the order's number was reserved under (plan
	// 087): the counter, the number, the file name and the folder — the
	// frozen one on a replay; nil for an order without a rule.
	Rule *AppliedRule
}

// ErrDocumentRuleConflict is a repeat of a historyId whose documentRule is not
// the one frozen at reservation: 409, nothing written.
var ErrDocumentRuleConflict = errors.New("documentRule differs from the one this historyId was accepted with")

// ErrDocumentRuleInvalid is a documentRule that names what this box does not
// have (the route checks first; this is the queue's own guard): 400.
var ErrDocumentRuleInvalid = errors.New("documentRule is not valid on this box")

// NewOrderQueue creates a new queue. Call Start to begin processing.
func NewOrderQueue(sender *Sender, log logger.LoggerService) *OrderQueue {
	return &OrderQueue{
		ch:            make(chan orderJob, defaultQueueSize),
		sender:        sender,
		log:           log,
		jobs:          make(map[string]*JobResult),
		byNumber:      make(map[int64]string),
		sharedNumbers: make(map[int64]bool),
		recovered:     make(map[string]bool),
		highest:       make(map[string]int64),
		done:          make(chan struct{}),
	}
}

// UseCounters gives the queue the box's counters (plan 087): an order with a
// documentRule is numbered by the counter it names, one without by the
// default order counter. Call it before UseKeyIndex, Recover, Start and the
// first Submit. Without it the queue has one counter, the sender's.
func (q *OrderQueue) UseCounters(set *CounterSet) {
	q.closeMu.Lock()
	defer q.closeMu.Unlock()
	q.counters = set
	clear(q.recovered)
}

// Generation changes whenever this queue reserves a number or recovers a
// counter: GET /api/counters' cache answers again only while it is the same
// (review m13).
func (q *OrderQueue) Generation() uint64 {
	if q == nil {
		return 0
	}
	return q.gen.Load()
}

// UseFolderGuard makes the queue ask g before every new order, and every
// re-queue, into a typed folder: a refusal is ErrFolderWritable, nothing
// reserved, nothing written; a repeat of an order already written is still
// answered. The daemon gives it its FolderGuard (review M1, R3).
func (q *OrderQueue) UseFolderGuard(g FolderAllower) {
	q.closeMu.Lock()
	defer q.closeMu.Unlock()
	q.guard = g
}

// UseImporterGuard makes the queue ask g (KeyOrderImporter) before a new
// order without a rule is reserved, before it is re-queued, and the worker
// before its files are written: behind the link an importer others can
// change is not run (review T1).
func (q *OrderQueue) UseImporterGuard(g FolderAllower) {
	q.closeMu.Lock()
	defer q.closeMu.Unlock()
	q.importerGuard = g
	if q.sender != nil {
		q.sender.importerCheck = func() error { return g.Allowed(KeyOrderImporter) }
	}
}

// importerRefused is the importer guard's refusal for an order without a
// rule, or nil. Caller holds closeMu.
func (q *OrderQueue) importerRefused(req OrderRequest) error {
	if q.importerGuard == nil || req.Rule != nil || q.sender == nil || q.sender.cfg.OrderImporter() == "" {
		return nil
	}
	return q.importerGuard.Allowed(KeyOrderImporter)
}

// RefuseFolders is UseFolderGuard with fixed refusals (key → why).
func (q *OrderQueue) RefuseFolders(why map[string]string) { q.UseFolderGuard(staticRefusals(why)) }

// folderRefused is the guard's refusal for an order into folder, or nil.
// Caller holds closeMu; the guard never waits on a read.
func (q *OrderQueue) folderRefused(folder string) error {
	if q.guard == nil {
		return nil
	}
	return q.guard.Allowed(folder)
}

// orderCounters is the queue's counter set.
func (q *OrderQueue) orderCounters() *CounterSet {
	if q.counters != nil {
		return q.counters
	}
	return implicitOrderCounters(q.numberStore())
}

// Counters is the queue's counter set, for the read routes.
func (q *OrderQueue) Counters() *CounterSet {
	if q == nil {
		return nil
	}
	q.closeMu.Lock()
	defer q.closeMu.Unlock()
	return q.orderCounters()
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
	q.importsDir = OrderImportsDir(dataDir)
	clear(q.recovered)
}

// Recover reads the key index and moves each order counter past the highest
// number issued in its range — the highest readable entry, or the highest
// sendOrderDir/history/<n> if higher (orders written before the index, or
// whose entry is damaged) — since a crash between saving an entry and moving
// the counter leaves the counter behind. It logs every entry it cannot read
// (and skips it: only that key is refused) and every order whose outcome is
// unknown. It writes no order and never changes an entry. Accept runs it for
// a counter before issuing a new number from it when that counter has not
// recovered yet; a repeat of a known key needs no recovery. A counter that
// cannot be recovered is in the error and refuses its own orders; the others
// recover and serve (plan 087).
func (q *OrderQueue) Recover() error {
	q.closeMu.Lock()
	defer q.closeMu.Unlock()
	if q.index == nil {
		return nil
	}
	return q.recoverLocked()
}

func (q *OrderQueue) recoverLocked() error {
	cs := q.orderCounters().OfKind(config.DocumentKindOrders)
	if len(cs) == 0 {
		return errors.New("order index: the queue has no order number store")
	}
	return q.recoverCountersLocked(cs)
}

// recoverCountersLocked reads the index into byNumber, then recovers each of
// cs over the numbers in its range. Caller holds closeMu.
func (q *OrderQueue) recoverCountersLocked(cs []*Counter) error {
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
	for _, e := range entries {
		if stem, dup := byNumber[e.Number]; dup && stem != orderIndexStem(e.Key) {
			shared[e.Number] = true
		}
		byNumber[e.Number] = orderIndexStem(e.Key)
	}
	sendDir := q.sender.cfg.SendOrderDir

	// The map first: a re-queue needs it and no counter, so it is in place
	// even when a counter step below fails.
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

	history, err := historyNumbers(filepath.Join(sendDir, "history"))
	if strings.TrimSpace(sendDir) == "" {
		history, err = nil, nil
	}
	if err != nil {
		return fmt.Errorf("recover the order index: %w", err)
	}
	if all := q.orderCounters().OfKind(config.DocumentKindOrders); len(all) > 1 || (len(all) == 1 && !all[0].Implicit) {
		q.logOutOfRange(all, entries, history)
	}
	defer q.gen.Add(1) // a counter may have moved
	var errs []error
	for _, c := range cs {
		if err := q.recoverCounterLocked(c, entries, history); err != nil {
			q.recovered[c.Key] = false
			errs = append(errs, fmt.Errorf("recover the order counter %s: %w", c.Key, err))
			continue
		}
		q.recovered[c.Key] = true
	}
	return errors.Join(errs...)
}

// recoverCounterLocked moves counter c past the highest number issued in its
// range. Caller holds closeMu.
func (q *OrderQueue) recoverCounterLocked(c *Counter, entries []*orderIndexEntry, history []int64) error {
	var highestEntry *orderIndexEntry
	for _, e := range entries { // sorted by number
		if c.InRange(e.Number) {
			highestEntry = e
		}
	}
	var highestHistory int64
	for _, n := range history {
		if c.InRange(n) && n > highestHistory {
			highestHistory = n
		}
	}
	highest := highestHistory
	if highestEntry != nil && highestEntry.Number > highest {
		highest = highestEntry.Number
	}
	store := q.counterName(c)
	if highest > 0 {
		next, err := c.Peek()
		if err != nil {
			return err
		}
		if next <= highest {
			switch {
			case highestEntry != nil && highestEntry.Number == highest && highestEntry.State == orderReserved && next == highest:
				q.log.Warn(fmt.Sprintf("order %d was reserved for historyId=%q but %s was not moved past it (the connector stopped, or the counter could not be written, in between); moving it past %d",
					highest, highestEntry.Key, store, highest))
			case highest == highestHistory:
				// Not a crash: the counter was moved back (restored, hand-edited),
				// or orders were written before the index existed.
				q.log.Warn(fmt.Sprintf("%s offered %d but %s already holds order %d; moving the counter past %d",
					store, next, filepath.Join(q.sender.cfg.SendOrderDir, "history"), highest, highest))
			default:
				q.log.Warn(fmt.Sprintf("%s offered %d but the order index already holds orders up to %d; moving the counter past %d",
					store, next, highest, highest))
			}
			if err := c.advancePast(highest); err != nil {
				return err
			}
		}
	}
	q.highest[c.Key] = highest
	return nil
}

// counterName is how the log names a counter: lastOrderNumber.json for the
// implicit one (today's text), "counter <key> (<file>)" otherwise.
func (q *OrderQueue) counterName(c *Counter) string {
	if c.Implicit {
		return "lastOrderNumber.json"
	}
	return fmt.Sprintf("counter %s (%s)", c.Key, filepath.Base(c.Path))
}

// logOutOfRange warns about issued order numbers no counter's range holds:
// they fence nothing, and a counter created over them later recovers past
// them from the index and the history.
func (q *OrderQueue) logOutOfRange(cs []*Counter, entries []*orderIndexEntry, history []int64) {
	seen := map[int64]bool{}
	var out []int64
	for _, e := range entries {
		if counterFor(cs, e.Number) == nil && !seen[e.Number] {
			seen[e.Number] = true
			out = append(out, e.Number)
		}
	}
	for _, n := range history {
		if counterFor(cs, n) == nil && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	if len(out) > 20 {
		out = out[len(out)-20:]
	}
	q.log.Warn(fmt.Sprintf("orders issued outside every counter's range (the highest, at most 20): %v — they fence no counter; no range on this box may be widened over them without the counters command checking the index", out))
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
			result, err := q.sender.processOrder(ctx, job.req, job.orderNumber, job.dest)
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
				q.recordImportCheck(result)
				q.log.Success(fmt.Sprintf("order job %s done orderNumber=%d files=%v", job.id, result.OrderNumber, result.WrittenFiles))
				q.setDone(job.id, result)
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
	var fe *FieldError
	if err != nil && !errors.Is(err, ErrQueueFull) && !errors.As(err, &fe) { // a 400 is the request's, not the box's
		q.log.Error(fmt.Sprintf("sendOrder historyId=%q not accepted", req.HistoryID), err)
	}
	return acc, err
}

func (q *OrderQueue) acceptUnkeyedLocked(req OrderRequest) (Acceptance, error) {
	if req.Rule != nil {
		return Acceptance{}, errors.New("an order with a documentRule needs the order index (historyId is its key)")
	}
	if err := q.validateNew(req); err != nil {
		return Acceptance{}, err
	}
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
	key := req.HistoryID
	if key == "" {
		return Acceptance{}, errors.New("historyId is required: it is the order's idempotency key")
	}

	// The index first (review m3): a repeat is answered against what was
	// frozen at its reservation — the rule's keys need not exist on the box
	// any more (a folder removed with nothing in flight, a restore, the
	// implicit "orders" key adopted under another name): it replays, re-queues
	// or answers 409, never a 400 that reads as "order refused".
	e, found, err := q.index.load(key)
	if err != nil {
		return Acceptance{}, err
	}
	if found {
		// The rule was frozen at reservation (plan 087): a repeat that names
		// another one is refused, never written under the first number into
		// another folder or from another counter.
		if !e.Rule.same(req.Rule) {
			return Acceptance{JobID: strconv.FormatInt(e.Number, 10)}, fmt.Errorf("%w: historyId %q holds order %d with %s; the repeat sends %s",
				ErrDocumentRuleConflict, key, e.Number, e.Rule.describe(), describeRule(req.Rule))
		}
		if e.State == orderRetryable && !q.jobActive(e.Number) {
			if e.Rule == nil {
				if err := q.importerRefused(req); err != nil {
					return Acceptance{JobID: strconv.FormatInt(e.Number, 10)}, err
				}
			}
			if e.Rule != nil {
				if err := q.folderRefused(e.Rule.Folder); err != nil {
					return Acceptance{JobID: strconv.FormatInt(e.Number, 10)}, err
				}
				// Review R2: the frozen test flags still match the folder.
				if f, ok := q.sender.cfg.Documents.Folders[e.Rule.Folder]; ok {
					if why := testFlagChanged(f, e.Rule.CounterTest, e.Rule.FolderTest); why != "" {
						q.log.Warn(fmt.Sprintf("sendOrder historyId=%q repeated: order %d is retryable but NOT queued again: %s (FOLDER_TEST_CHANGED); it stays retryable", key, e.Number, why))
						return Acceptance{JobID: strconv.FormatInt(e.Number, 10)}, fmt.Errorf("%w: order %d into folder %s: %s", ErrFolderTestChanged, e.Number, e.Rule.Folder, why)
					}
				}
			}
			return q.requeueLocked(e, req)
		}
		return q.replay(e, req), nil
	}

	// A new reservation: only now are the rule and the layout checked
	// against the box, and the counter read.
	if err := q.validateNew(req); err != nil {
		return Acceptance{}, err
	}
	c, err := q.counterForRequest(req)
	if err != nil {
		return Acceptance{}, err
	}
	if c.Retired {
		return Acceptance{}, fmt.Errorf("%w: counter %s issues no new number", ErrCounterRetired, c.Key)
	}
	if req.Rule != nil {
		if err := q.folderRefused(req.Rule.Folder); err != nil {
			return Acceptance{}, err
		}
	}
	if err := q.importerRefused(req); err != nil {
		return Acceptance{}, err
	}
	if !q.recovered[c.Key] {
		if err := q.recoverCountersLocked([]*Counter{c}); err != nil {
			return Acceptance{}, err
		}
	}
	if len(q.ch) >= cap(q.ch) {
		return Acceptance{}, fmt.Errorf("%w (capacity %d)", ErrQueueFull, defaultQueueSize)
	}
	n, err := c.next()
	if err != nil {
		return Acceptance{}, fmt.Errorf("reserve order number: %w", err)
	}
	if n <= q.highest[c.Key] {
		// Recovery put the counter past every number known to be issued, so
		// the file was moved back while the connector ran. Refuse, and have
		// the next Accept recover (move it forward) before issuing anything.
		q.recovered[c.Key] = false
		return Acceptance{}, fmt.Errorf("%s offers order %d, but orders up to %d were already issued; nothing reserved", q.counterName(c), n, q.highest[c.Key])
	}

	var dest *orderDestination
	e = &orderIndexEntry{
		Key:         key,
		Number:      n,
		State:       orderReserved,
		RequestHash: orderRequestHash(req),
		ReservedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	if req.Rule != nil {
		f, ok := q.sender.cfg.Documents.Folders[req.Rule.Folder]
		if !ok { // the handler resolved it against the same config
			return Acceptance{}, fmt.Errorf("%w: folder %q is not configured", ErrDocumentRuleInvalid, req.Rule.Folder)
		}
		e.Rule = &storedRule{Counter: c.Key, Folder: req.Rule.Folder, Temporary: req.Rule.Temporary,
			FileName: config.DocumentFileName(f.NamePattern, f.TempPrefix, req.Rule.Temporary, n), CounterTest: c.Test, FolderTest: f.Test}
		dest = e.Rule.destination()
	}
	if err := q.index.save(e); err != nil {
		return Acceptance{}, err // nothing reserved: the counter has not moved
	}
	q.highest[c.Key] = n
	q.mu.Lock()
	q.byNumber[n] = orderIndexStem(key)
	q.mu.Unlock()
	q.crash(crashOrderAfterIndexReserved)
	err = c.advancePast(n)
	// Review R5: the cache's generation moves once the store has moved — a
	// report read in between, under the old generation, would serve the old
	// last number for the cache's whole lifetime.
	q.gen.Add(1)
	if err != nil {
		// The index owns n, so this order goes ahead under it — refusing would
		// leave the key reserved and its order never written. But the counter
		// still offers n: issue nothing more until recovery has moved it on.
		q.recovered[c.Key] = false
		q.log.Error(fmt.Sprintf("order %d reserved for historyId=%q, but %s could not be moved past it; no new order is accepted from it until it can", n, key, q.counterName(c)), err)
	}
	q.crash(crashOrderAfterCounterAdvanced)

	acc, err := q.enqueueLocked(orderJob{id: strconv.FormatInt(n, 10), orderNumber: n, req: req, entry: e, dest: dest})
	if err != nil { // unreachable: see enqueueLocked
		q.recordOutcome(orderJob{entry: e}, nil, notWritten(fmt.Errorf("never queued: %w", err)))
		return acc, err
	}
	// The log is where a person maps a historyId to its number and file.
	if e.Rule != nil {
		q.log.Info(fmt.Sprintf("sendOrder historyId=%q: order %d reserved from counter %s for folder %s as %s, and queued (index %s)",
			key, n, e.Rule.Counter, e.Rule.Folder, e.Rule.FileName, q.index.pathFor(orderIndexStem(key))))
	} else {
		q.log.Info(fmt.Sprintf("sendOrder historyId=%q: order %d reserved and queued (index %s)", key, n, q.index.pathFor(orderIndexStem(key))))
	}
	acc.Rule = e.Rule.applied(n)
	return acc, nil
}

// validateNew checks a new order against the box before anything is
// reserved (review m3: a repeat never is): its documentRule's keys
// (ResolveRule) and the layout its destination writes (ValidateLayout). Every
// refusal is a *FieldError — the route's 400 VALIDATION_ERROR with
// details.field.
func (q *OrderQueue) validateNew(req OrderRequest) error {
	if req.Rule != nil {
		if q.sender == nil {
			return fmt.Errorf("%w: the queue has no sender to resolve it against", ErrDocumentRuleInvalid)
		}
		if err := ResolveRule(q.sender.cfg, *req.Rule); err != nil {
			return err
		}
	}
	if err := q.ValidateLayout(req); err != nil {
		var fe *FieldError
		if !errors.As(err, &fe) {
			return &FieldError{Field: "body", Reason: err.Error()}
		}
		return err
	}
	return nil
}

// counterForRequest is the counter that numbers req: the one its rule names,
// or the default order counter.
func (q *OrderQueue) counterForRequest(req OrderRequest) (*Counter, error) {
	set := q.orderCounters()
	if req.Rule == nil {
		c := set.DefaultOrder()
		if c == nil {
			return nil, errors.New("order index: the queue has no order number store")
		}
		return c, nil
	}
	c, ok := set.Get(req.Rule.Counter)
	if !ok || c.Kind != config.DocumentKindOrders {
		return nil, fmt.Errorf("%w: counter %q is not an order counter on this box", ErrDocumentRuleInvalid, req.Rule.Counter)
	}
	return c, nil
}

// requeueLocked queues a retryable order again under its own number: nothing
// of it was written, so writing it now is its first write. The entry is saved
// reserved BEFORE the job is sent, so a racing repeat sees it queued and never
// queues it twice. No counter is involved. Caller holds closeMu.
func (q *OrderQueue) requeueLocked(e *orderIndexEntry, req OrderRequest) (Acceptance, error) {
	// The guard below needs the index map. Recovery fills it before its
	// counter step, so a counter it cannot read does not stop a re-queue.
	if !q.scanned {
		if err := q.recoverLocked(); err != nil && !q.scanned {
			return Acceptance{}, err
		}
	}
	if reason := q.requeueConflict(e); reason != "" {
		q.log.Error(fmt.Sprintf("ORDER NOT QUEUED AGAIN: order %d (historyId=%q) is retryable, but %s. Writing it would make a second document with Asmahta %d; answering it as failed. "+
			"A person must check the entry %s (docs/troubleshooting.md § The order index).",
			e.Number, e.Key, reason, e.Number, q.index.pathFor(orderIndexStem(e.Key))), nil)
		return Acceptance{JobID: strconv.FormatInt(e.Number, 10), Status: JobStatusFailed, Replay: true, Rule: e.Rule.applied(e.Number)}, nil
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
	acc, err := q.enqueueLocked(orderJob{id: strconv.FormatInt(e.Number, 10), orderNumber: e.Number, req: req, entry: e, dest: e.Rule.destination()})
	if err != nil { // unreachable: see enqueueLocked
		q.recordOutcome(orderJob{entry: e}, nil, notWritten(fmt.Errorf("never queued: %w", err)))
		return acc, err
	}
	q.log.Info(fmt.Sprintf("sendOrder historyId=%q repeated: order %d failed before anything was written (%s); queued again under the same number (retry %d)",
		e.Key, e.Number, lastErr, e.Retries))
	acc.Replay = true
	acc.Rule = e.Rule.applied(e.Number)
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
	return Acceptance{JobID: jobID, Status: status, Replay: true, Rule: e.Rule.applied(e.Number)}
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
	if e.State == orderWritten && q.importsDir != "" {
		res.Import = loadImportCheck(q.importsDir, n)
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

// setDone records a written order with its import check in one step, so a
// poll never reads done without the check the worker already has.
func (q *OrderQueue) setDone(id string, result *OrderResult) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.jobs[id] = &JobResult{
		ID:           id,
		Status:       JobStatusDone,
		OrderNumber:  result.OrderNumber,
		WrittenFiles: result.WrittenFiles,
		Import:       result.Import,
	}
}

// recordImportCheck keeps a written order's import check beside the index
// (OrderImportsDir), so a status answered after a restart still has it. A
// save that fails is logged and costs only the report.
func (q *OrderQueue) recordImportCheck(result *OrderResult) {
	if result == nil || result.Import == nil || q.importsDir == "" {
		return
	}
	if err := saveImportCheck(q.importsDir, result.OrderNumber, result.Import); err != nil {
		q.log.Warn(fmt.Sprintf("order %d: import check not saved (%v); it is answered until the connector restarts", result.OrderNumber, err))
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

// highestIssued is the highest order number issued in c's range that this
// queue knows: the index's numbers read at recovery and every reservation
// since, and sendOrderDir/history/<n>. known is false before a recovery read
// the index (the caller scans it then).
func (q *OrderQueue) highestIssued(c *Counter) (highest int64, known bool, err error) {
	q.closeMu.Lock()
	known = q.scanned
	q.closeMu.Unlock()
	if !known {
		return 0, false, nil
	}
	q.mu.RLock()
	for n := range q.byNumber {
		if c.InRange(n) && n > highest {
			highest = n
		}
	}
	q.mu.RUnlock()
	if dir := strings.TrimSpace(q.sender.cfg.SendOrderDir); dir != "" {
		h, err := highestHistoryInRange(filepath.Join(dir, "history"), c.Min, c.Max)
		if err != nil {
			return highest, true, err
		}
		highest = max(highest, h)
	}
	return highest, true, nil
}
