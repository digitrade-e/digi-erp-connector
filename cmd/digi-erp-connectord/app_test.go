package main

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
)

type quietLogger struct{}

func (quietLogger) Info(string)         {}
func (quietLogger) Error(string, error) {}
func (quietLogger) Warn(string)         {}
func (quietLogger) Success(string)      {}
func (quietLogger) Close() error        { return nil }

// Stop must not lose orders the backend was told were accepted (202). It used
// to cancel the queue first, so the worker quit with orders still queued.
func TestStop_FinishesQueuedOrders(t *testing.T) {
	dir := t.TempDir()
	sender := hasavshevet.NewSender(nil, config.Config{SendOrderDir: dir},
		hasavshevet.NewOrderNumberStore(filepath.Join(dir, "lastOrderNumber.json")), quietLogger{})
	queue := hasavshevet.NewOrderQueue(sender, quietLogger{})
	queueCtx, queueCancel := context.WithCancel(context.Background())
	queue.Start(queueCtx)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.NotFoundHandler()}
	go func() { _ = srv.Serve(ln) }()

	app := &serverApp{logSvc: quietLogger{}, srv: srv, orderQueue: queue, queueCancel: queueCancel}

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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	app.Stop(ctx)

	for _, id := range ids {
		if st, _ := queue.Status(id); st == nil || st.Status != hasavshevet.JobStatusDone {
			t.Errorf("order %s after Stop = %+v; want done", id, st)
		}
	}
	if _, err := http.Get("http://" + ln.Addr().String()); err == nil {
		t.Error("the HTTP server still answers after Stop")
	}
}
