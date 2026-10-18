package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/api/handlers"
	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
)

type quietLogger struct {
	mu     sync.Mutex
	errors []string
}

func (*quietLogger) Info(string) {}
func (l *quietLogger) Error(msg string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errors = append(l.errors, fmt.Sprintf("%s: %v", msg, err))
}
func (*quietLogger) Warn(string)    {}
func (*quietLogger) Success(string) {}
func (*quietLogger) Close() error   { return nil }

func newOrderQueue(t *testing.T) (*hasavshevet.OrderQueue, context.CancelFunc) {
	t.Helper()
	dir := t.TempDir()
	sender := hasavshevet.NewSender(nil, config.Config{SendOrderDir: dir},
		hasavshevet.NewOrderNumberStore(filepath.Join(dir, "lastOrderNumber.json")), &quietLogger{})
	queue := hasavshevet.NewOrderQueue(sender, &quietLogger{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	queue.Start(ctx)
	return queue, cancel
}

func serve(t *testing.T, h http.Handler) (*http.Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	return srv, "http://" + ln.Addr().String()
}

// stopAsync runs app.Stop and reports when it returns.
func stopAsync(app *serverApp) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		app.Stop(ctx)
		close(done)
	}()
	return done
}

func returned(ch <-chan struct{}, within time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(within):
		return false
	}
}

// Stop must not lose orders the backend was told were accepted (202). The
// worker is held behind the import lock so all three are still queued when
// Stop starts; the old Stop cancelled the worker first and they were lost.
func TestStop_FinishesQueuedOrders(t *testing.T) {
	queue, queueCancel := newOrderQueue(t)
	srv, url := serve(t, http.NotFoundHandler())
	app := &serverApp{logSvc: &quietLogger{}, srv: srv, orderQueue: queue, queueCancel: queueCancel}

	release, err := hasavshevet.HoldImports(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var ids []string
	for i := 0; i < 3; i++ {
		id, err := queue.Submit(hasavshevet.OrderRequest{
			DocumentType: "ORDER", UserExtID: "1234", DueDate: "2026-10-11", CreatedDate: "2026-10-11",
			HistoryID: "H-" + strconv.Itoa(i), Currency: "ILS", Total: 50,
			Account: &hasavshevet.AccountDetails{AccountKey: "1234", FullName: "Test"},
			Details: []hasavshevet.OrderLineItem{{Title: "Widget", SKU: "1001", Quantity: 1, SinglePrice: 50, TotalPrice: 50}},
		})
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		ids = append(ids, id)
	}

	stopped := stopAsync(app)
	if returned(stopped, 200*time.Millisecond) {
		// The old Stop gets here: it cancelled the worker, which gave up
		// waiting for the lock with all three orders queued.
		for _, id := range ids {
			st, _ := queue.Status(id)
			t.Errorf("Stop returned while order %s was %s", id, st.Status)
		}
		return
	}
	release()
	if !returned(stopped, 10*time.Second) {
		t.Fatal("Stop did not return after the orders could run")
	}

	for _, id := range ids {
		if st, _ := queue.Status(id); st == nil || st.Status != hasavshevet.JobStatusDone {
			t.Errorf("order %s after Stop = %+v; want done", id, st)
		}
	}
	if _, err := http.Get(url); err == nil {
		t.Error("the HTTP server still answers after Stop")
	}
}

// blockingReceiptWriter holds a receipt "in the importer" until released.
type blockingReceiptWriter struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockingReceiptWriter) Write(_ context.Context, req hasavshevet.ReceiptRequest) (*hasavshevet.ReceiptResult, error) {
	close(b.entered)
	<-b.release
	return &hasavshevet.ReceiptResult{ReceiptNumber: 1000, IdempotencyKey: req.IdempotencyKey, Written: true, Import: hasavshevet.ImportOK}, nil
}

const inFlightReceipt = `{"idempotencyKey":"3f2c9a1e-7b4d-4c8e-9a51-2d6f0b8e4c17","accountKey":"40217","settledDate":"2026-10-08",
"amountMinor":694700,"installments":1,"cardIssuer":"2","cardBrand":"2","last4":"4580","confirmationCode":"0123456",
"providerUid":"","remarks":"26004295","agentKey":"","name":"Levi","address":"","phone":"","email":""}`

// A receipt being written when the service is stopped finishes, and its
// caller gets the number: Stop waits for it before anything else.
func TestStop_LetsAnInFlightReceiptFinish(t *testing.T) {
	queue, queueCancel := newOrderQueue(t)
	w := &blockingReceiptWriter{entered: make(chan struct{}), release: make(chan struct{})}
	srv, url := serve(t, handlers.NewSendReceiptHandler(w, true, &quietLogger{}))
	app := &serverApp{logSvc: &quietLogger{}, srv: srv, orderQueue: queue, queueCancel: queueCancel}

	type answer struct {
		status int
		body   string
		err    error
	}
	got := make(chan answer, 1)
	go func() {
		resp, err := http.Post(url, "application/json", strings.NewReader(inFlightReceipt))
		if err != nil {
			got <- answer{err: err}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		got <- answer{status: resp.StatusCode, body: string(b)}
	}()
	select {
	case <-w.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the receipt never reached the writer")
	}

	stopped := stopAsync(app)
	if returned(stopped, 200*time.Millisecond) {
		t.Fatal("Stop returned with a receipt still being written")
	}
	close(w.release)

	a := <-got
	if a.err != nil || a.status != http.StatusOK || !strings.Contains(a.body, `"receiptNumber":"1000"`) {
		t.Fatalf("in-flight receipt answer = %d %q %v; want 200 with its number", a.status, a.body, a.err)
	}
	if !returned(stopped, 10*time.Second) {
		t.Fatal("Stop did not return after the receipt finished")
	}
}

// receiptCfg is a configured receipt install under a temp directory.
func receiptCfg(t *testing.T) (config.Config, string, string) {
	t.Helper()
	root := t.TempDir()
	cfg := config.Config{ERP: config.ERPHasavshevet, SendOrderDir: filepath.Join(root, "orders"),
		SendReceiptDir: "  " + filepath.Join(root, "herp") + " "}
	return cfg, filepath.Join(root, "herp"), filepath.Join(root, "data")
}

// plantReserved leaves the index as a crash between reserving receipt n and
// writing its file would.
func plantReserved(t *testing.T, dataDir, key string, n int64) {
	t.Helper()
	req := hasavshevet.ReceiptRequest{
		IdempotencyKey: key, AccountKey: "40217", Name: "Levi",
		SettledDate: "2026-10-08", AmountMinor: 694700, Installments: 1,
		CardIssuer: "2", Last4: "4580", ConfirmationCode: "0123456", ProviderUID: "81122394",
		Remarks: "26004295",
	}
	b, err := json.Marshal(map[string]any{
		"key": key, "number": n, "state": "reserved", "request": req, "files": nil,
		"import": "not_run", "reservedAt": "2026-10-11T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dataDir, "receipts", "index", key+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The daemon recovers a half-written receipt at startup, before serving.
func TestBuildReceiptWriter_RecoversAtStartup(t *testing.T) {
	cfg, herp, dataDir := receiptCfg(t)
	if err := hasavshevet.NewReceiptNumberStore(hasavshevet.ReceiptNumberStorePath(dataDir)).Seed(1000); err != nil {
		t.Fatal(err)
	}
	plantReserved(t, dataDir, "00000000-0000-4000-8000-000000000001", 1000)

	log := &quietLogger{}
	w, err := buildReceiptWriter(cfg, dataDir, log)
	if err != nil || w == nil {
		t.Fatalf("buildReceiptWriter = %v, %v", w, err)
	}
	t.Cleanup(func() { _ = w.Close() }) // before TempDir's: Windows cannot delete a held lock file
	// The trimmed folder is the one written to.
	if _, err := os.Stat(filepath.Join(herp, "KUPAIN_1000.doc")); err != nil {
		t.Errorf("the reserved receipt was not finished at startup: %v", err)
	}
	if n, err := hasavshevet.NewReceiptNumberStore(hasavshevet.ReceiptNumberStorePath(dataDir)).Peek(); err != nil || n != 1001 {
		t.Errorf("store after startup = %d, %v; want 1001", n, err)
	}
	if len(log.errors) != 0 {
		t.Errorf("errors logged: %v", log.errors)
	}
}

// A failed recovery is logged and does not stop the daemon: orders and
// queries keep working, and the route answers errors until it succeeds.
func TestBuildReceiptWriter_FailedRecoveryIsNotFatal(t *testing.T) {
	cfg, _, dataDir := receiptCfg(t)
	idx := filepath.Join(dataDir, "receipts", "index", "00000000-0000-4000-8000-000000000001.json")
	if err := os.MkdirAll(filepath.Dir(idx), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idx, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	log := &quietLogger{}
	w, err := buildReceiptWriter(cfg, dataDir, log)
	if err != nil || w == nil {
		t.Fatalf("buildReceiptWriter = %v, %v; want a writer and no error", w, err)
	}
	t.Cleanup(func() { _ = w.Close() }) // before TempDir's: Windows cannot delete a held lock file
	if len(log.errors) != 1 || !strings.Contains(log.errors[0], "recovery") {
		t.Errorf("errors logged = %v; want the recovery failure", log.errors)
	}
}

func TestBuildReceiptWriter_NotConfiguredAndInvalid(t *testing.T) {
	if w, err := buildReceiptWriter(config.Config{ERP: config.ERPHasavshevet}, t.TempDir(), &quietLogger{}); w != nil || err != nil {
		t.Errorf("unconfigured = %v, %v; want nil, nil", w, err)
	}
	cfg, _, dataDir := receiptCfg(t)
	cfg.SendReceiptDir = "herp" // relative
	if _, err := buildReceiptWriter(cfg, dataDir, &quietLogger{}); err == nil {
		t.Error("a relative sendReceiptDir was accepted")
	}
}

// Stop releases the receipt data-directory lock, so the next connector to
// start (an upgrade, a restart) takes it at once rather than at process exit.
func TestStop_ReleasesTheReceiptDataLock(t *testing.T) {
	cfg, _, dataDir := receiptCfg(t)
	if err := hasavshevet.NewReceiptNumberStore(hasavshevet.ReceiptNumberStorePath(dataDir)).Seed(1000); err != nil {
		t.Fatal(err)
	}
	w, err := buildReceiptWriter(cfg, dataDir, &quietLogger{})
	if err != nil || w == nil {
		t.Fatalf("buildReceiptWriter = %v, %v", w, err)
	}
	app := &serverApp{logSvc: &quietLogger{}, receiptWriter: w}
	app.Stop(context.Background())

	next, err := hasavshevet.NewReceiptWriter(hasavshevet.ReceiptWriterConfigFrom(cfg, dataDir), &quietLogger{})
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Recover(); err != nil {
		t.Fatalf("after Stop the data directory is still held: %v", err)
	}
	_ = next.Close()
}

// …and never hangs on it: an order import holding the import lock past the
// stop budget leaves the lock to the OS at exit.
func TestStop_DoesNotHangOnAHeldImportLock(t *testing.T) {
	cfg, _, dataDir := receiptCfg(t)
	w, err := buildReceiptWriter(cfg, dataDir, &quietLogger{})
	if err != nil || w == nil {
		t.Fatalf("buildReceiptWriter = %v, %v", w, err)
	}
	// Runs after the deferred release below (cleanups follow defers) and
	// before TempDir's: Windows cannot delete a lock file Stop left held.
	t.Cleanup(func() { _ = w.Close() })
	release, err := hasavshevet.HoldImports(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	app := &serverApp{logSvc: &quietLogger{}, receiptWriter: w}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	stopped := make(chan struct{})
	go func() { app.Stop(ctx); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop hung on the held import lock")
	}
}
