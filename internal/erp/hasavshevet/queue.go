package hasavshevet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"

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

	// closeMu guards closed and every send on ch, so Submit never sends on a
	// closed channel and Stop never closes it twice.
	closeMu sync.Mutex
	closed  bool
}

// ErrQueueClosed is Submit's answer once the queue is draining or stopped.
var ErrQueueClosed = errors.New("order queue is closed (the connector is stopping)")

// NewOrderQueue creates a new queue. Call Start to begin processing.
func NewOrderQueue(sender *Sender, log logger.LoggerService) *OrderQueue {
	return &OrderQueue{
		ch:     make(chan orderJob, defaultQueueSize),
		sender: sender,
		log:    log,
		jobs:   make(map[string]*JobResult),
		done:   make(chan struct{}),
	}
}

// Start launches the single background worker goroutine.
// The goroutine exits when ctx is cancelled or Stop is called.
func (q *OrderQueue) Start(ctx context.Context) {
	go q.run(ctx)
}

func (q *OrderQueue) run(ctx context.Context) {
	defer close(q.done)
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
				return // ctx cancelled while a receipt held the lock
			}
			q.setStatus(job.id, JobStatusRunning, job.orderNumber, nil, nil)
			result, err := q.sender.processOrderWithNumber(ctx, job.req, job.orderNumber)
			unlockImport()
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

// Submit enqueues an order request and returns a job ID.
// In normal runtime this ID is the reserved lastOrderNumber as a decimal string.
// Returns ErrQueueClosed once the queue is stopping, or an error if it is full;
// neither refusal reserves an order number.
//
// The whole of Submit runs under closeMu: the closed and capacity checks, the
// reservation and the send. Checked before taking closeMu, a Stop landing
// between the check and the send burned the number just reserved. Only Submit
// sends on ch, so a free slot seen under closeMu is still free at the send.
func (q *OrderQueue) Submit(req OrderRequest) (string, error) {
	q.closeMu.Lock()
	defer q.closeMu.Unlock()
	if q.closed {
		return "", ErrQueueClosed
	}
	if len(q.ch) >= cap(q.ch) {
		return "", fmt.Errorf("order queue full (capacity %d)", defaultQueueSize)
	}
	jobID, orderNumber, err := q.reserveJobIdentity()
	if err != nil {
		return "", err
	}

	job := orderJob{id: jobID, orderNumber: orderNumber, req: req}

	q.mu.Lock()
	q.jobs[jobID] = &JobResult{
		ID:          jobID,
		Status:      JobStatusQueued,
		OrderNumber: orderNumber,
	}
	q.mu.Unlock()

	select {
	case q.ch <- job:
		return jobID, nil
	default: // unreachable while only Submit sends, and only under closeMu
		q.forget(jobID)
		return "", fmt.Errorf("order queue full (capacity %d)", defaultQueueSize)
	}
}

func (q *OrderQueue) forget(jobID string) {
	q.mu.Lock()
	delete(q.jobs, jobID)
	q.mu.Unlock()
}

// Pending is the number of orders accepted but not yet started.
func (q *OrderQueue) Pending() int { return len(q.ch) }

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
func (q *OrderQueue) Status(jobID string) (*JobResult, bool) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	r, ok := q.jobs[jobID]
	return r, ok
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
