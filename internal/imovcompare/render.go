package imovcompare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/api/handlers"
	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
)

// Rendered is the file the connector published for one order.
type Rendered struct {
	FileName string // IMOVEIN_<n>.doc, TMP_IMOVEIN_<n>.doc for a return
	Doc      []byte
	Log      []string // the connector's log lines for the order
}

// renderTimeout bounds one order: the worker writes two small files.
const renderTimeout = 30 * time.Second

// Render posts body — a POST /api/sendOrder body, exactly as erp-manager
// sends it — to the connector's own route, queue and writer, configured as
// BFL's write node is for the cutover (model (a): sendOrderPickupDir,
// sendOrderFormat legacy), in a fresh temporary folder, with the order
// counter set so that this order takes number. It returns the one file
// published to the pickup folder and removes the folder.
//
// There is no database: a body without "account" takes account (the
// Accounts row the box would read, as a sendOrder "account" object); with
// neither, the order fails. The currency rate is the connector's no-database
// default, 1.
func Render(ctx context.Context, body []byte, number int64, account json.RawMessage) (Rendered, error) {
	if number < 1 || number > config.MaxDocumentNumber {
		return Rendered{}, fmt.Errorf("order number %d: must be 1..%d (MyID, nine digits)", number, config.MaxDocumentNumber)
	}
	body, err := withAccount(body, account)
	if err != nil {
		return Rendered{}, err
	}
	root, err := os.MkdirTemp("", "imovein-compare-")
	if err != nil {
		return Rendered{}, err
	}
	defer os.RemoveAll(root)
	own, pickup, data := filepath.Join(root, "orders"), filepath.Join(root, "pickup"), filepath.Join(root, "data")
	for _, d := range []string{own, pickup, data} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return Rendered{}, err
		}
	}
	cfg := config.Config{
		ERP: config.ERPHasavshevet, SendOrderDir: own, SendOrderPickupDir: pickup,
		SendOrderFormat: config.OrderFormatLegacy,
	}
	if err := cfg.ValidateOrders(); err != nil {
		return Rendered{}, fmt.Errorf("the harness config is refused: %w", err)
	}
	counter := filepath.Join(own, "lastOrderNumber.json")
	if err := os.WriteFile(counter, []byte(fmt.Sprintf(`{"lastOrderNumber": %d}`, number-1)), 0o600); err != nil {
		return Rendered{}, err
	}

	log := &memLog{}
	queue := hasavshevet.NewOrderQueue(hasavshevet.NewSender(nil, cfg, hasavshevet.NewOrderNumberStore(counter), log), log)
	queue.UseKeyIndex(data)
	if err := queue.Recover(); err != nil {
		return Rendered{}, fmt.Errorf("order index: %w", err)
	}
	qctx, cancel := context.WithCancel(ctx)
	defer cancel()
	queue.Start(qctx)
	defer func() {
		queue.Stop()
		<-queue.Done()
	}()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/sendOrder", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	handlers.NewSendOrderHandler(queue, cfg.OrdersConfigured()).ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		return Rendered{Log: log.lines()}, fmt.Errorf("the route answered %d: %s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	var accepted struct {
		JobID string `json:"jobId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil {
		return Rendered{Log: log.lines()}, fmt.Errorf("202 body: %w", err)
	}
	if accepted.JobID != strconv.FormatInt(number, 10) {
		return Rendered{Log: log.lines()}, fmt.Errorf("the order took number %s, not %d", accepted.JobID, number)
	}

	deadline := time.Now().Add(renderTimeout)
	for {
		st, ok := queue.Status(accepted.JobID)
		if ok && st.Status == hasavshevet.JobStatusDone {
			break
		}
		if ok && st.Status == hasavshevet.JobStatusFailed {
			return Rendered{Log: log.lines()}, fmt.Errorf("the order failed: %v", st.Err)
		}
		if time.Now().After(deadline) {
			return Rendered{Log: log.lines()}, errors.New("the order was not written in time")
		}
		select {
		case <-ctx.Done():
			return Rendered{Log: log.lines()}, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}

	entries, err := os.ReadDir(pickup)
	if err != nil {
		return Rendered{}, err
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return Rendered{Log: log.lines()}, fmt.Errorf("the pickup folder holds %v, want exactly one file", names)
	}
	doc, err := os.ReadFile(filepath.Join(pickup, entries[0].Name()))
	if err != nil {
		return Rendered{}, err
	}
	return Rendered{FileName: entries[0].Name(), Doc: doc, Log: log.lines()}, nil
}

// withAccount adds "account" to a body that has none.
func withAccount(body []byte, account json.RawMessage) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("the body is not a JSON object: %w", err)
	}
	if a, ok := m["account"]; (ok && string(a) != "null") || len(account) == 0 {
		return body, nil
	}
	m["account"] = account
	return json.Marshal(m)
}

// memLog keeps the connector's log lines for the report.
type memLog struct {
	mu sync.Mutex
	l  []string
}

func (m *memLog) add(level, msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.l = append(m.l, level+" "+msg)
}
func (m *memLog) Info(msg string)    { m.add("INFO", msg) }
func (m *memLog) Warn(msg string)    { m.add("WARN", msg) }
func (m *memLog) Success(msg string) { m.add("OK", msg) }
func (m *memLog) Error(msg string, err error) {
	m.add("ERROR", fmt.Sprintf("%s: %v", msg, err))
}
func (m *memLog) Close() error { return nil }
func (m *memLog) lines() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.l...)
}
