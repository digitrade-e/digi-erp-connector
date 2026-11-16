package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
)

// Plan 087 (DIG-1563): POST /api/v2/sendOrder with documentRule, and the read
// routes GET /api/counters and GET /api/documents/capabilities.

type v2Box struct {
	cfg     config.Config
	queue   *hasavshevet.OrderQueue
	set     *hasavshevet.CounterSet
	dataDir string
}

func newV2Box(t *testing.T) *v2Box {
	t.Helper()
	root := t.TempDir()
	mk := func(name string) string {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	b := &v2Box{dataDir: filepath.Join(root, "data")}
	b.cfg = config.Config{
		ERP: config.ERPHasavshevet, SendOrderDir: mk("orders"),
		Documents: config.DocumentsConfig{
			Counters: map[string]config.CounterConfig{
				"b2b":           {Kind: config.DocumentKindOrders, Min: 1_000_000, Max: 1_999_999, Store: config.CounterStoreLastOrderNumber},
				"b2b_test":      {Kind: config.DocumentKindOrders, Min: 9_000_000, Max: 9_000_000, Test: true},
				"receipts_test": {Kind: config.DocumentKindReceipts, Min: 9_100_000, Max: 9_199_999, Test: true},
				"old":           {Kind: config.DocumentKindOrders, Min: 8_000_000, Max: 8_999_999, Retired: true},
			},
			Folders: map[string]config.FolderConfig{
				"herp": {Kind: config.DocumentKindOrders, Path: mk("herp"), NamePattern: "{prefix}IMOVEIN_{n}.doc",
					TempPrefix: "TMP_", Layout: config.OrderFormatLegacy},
				"herp_test": {Kind: config.DocumentKindOrders, Path: mk("herp_test"), NamePattern: "IMOVEIN_{n}.doc",
					Layout: config.OrderFormatLegacy, Test: true},
				"herp_receipts": {Kind: config.DocumentKindReceipts, Path: mk("kupain"), NamePattern: "KUPAIN_{n}.doc"},
			},
		},
	}
	if err := b.cfg.ValidateOrders(); err != nil {
		t.Fatal(err)
	}
	b.set = hasavshevet.NewCounterSet(b.cfg, b.dataDir)
	for k, last := range map[string]int64{"b2b": 1_000_122, "b2b_test": 8_999_999, "receipts_test": 9_100_004, "old": 8_000_100} {
		c, _ := b.set.Get(k)
		if err := c.RaiseTo(last); err != nil {
			t.Fatal(err)
		}
	}
	b.queue = hasavshevet.NewOrderQueue(hasavshevet.NewSender(nil, b.cfg, b.set.OrderNumberStore(), &noopLogger{}), &noopLogger{})
	b.queue.UseCounters(b.set)
	b.queue.UseKeyIndex(b.dataDir)
	if err := b.queue.Recover(); err != nil {
		t.Fatal(err)
	}
	return b
}

func postJSON(t *testing.T, h http.HandlerFunc, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	h(w, req)
	return w
}

// orderWithRule is a valid order body with the documentRule text spliced in
// raw ("" = no documentRule key at all).
func orderWithRule(t *testing.T, historyID, rule string) string {
	t.Helper()
	body := validOrderBody()
	body["historyId"] = historyID
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if rule == "" {
		return string(b)
	}
	return string(bytes.TrimSuffix(b, []byte("}"))) + `,"documentRule":` + rule + `}`
}

func TestSendOrderV2_RuleIsAppliedAndEchoed(t *testing.T) {
	b := newV2Box(t)
	h := NewSendOrderV2Handler(b.queue, b.cfg)
	w := postJSON(t, h, "/api/v2/sendOrder", orderWithRule(t, "cart-1", `{"v":1,"temporary":true,"folder":"herp","counter":"b2b"}`))
	if w.Code != http.StatusAccepted {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	m := decodeBody(t, w)
	want := map[string]any{"status": "queued", "jobId": "1000123", "counter": "b2b", "number": float64(1_000_123),
		"fileName": "TMP_IMOVEIN_1000123.doc", "folder": "herp"}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %v (%s)", k, m[k], v, w.Body)
		}
	}
	if _, ok := m["meta"]; !ok {
		t.Error("today's meta is gone")
	}

	// The same rule again: a replay, echoing the frozen rule.
	w = postJSON(t, h, "/api/v2/sendOrder", orderWithRule(t, "cart-1", `{"v":1,"temporary":true,"folder":"herp","counter":"b2b"}`))
	if m = decodeBody(t, w); w.Code != http.StatusAccepted || m["replay"] != true || m["fileName"] != "TMP_IMOVEIN_1000123.doc" {
		t.Fatalf("replay: %d %s", w.Code, w.Body)
	}
	// Another rule on the same historyId: 409, nothing written.
	w = postJSON(t, h, "/api/v2/sendOrder", orderWithRule(t, "cart-1", `{"v":1,"temporary":false,"folder":"herp","counter":"b2b"}`))
	if w.Code != http.StatusConflict || decodeBody(t, w)["code"] != "DOCUMENT_RULE_CONFLICT" {
		t.Fatalf("another rule: %d %s", w.Code, w.Body)
	}
	// And a repeat on /api/sendOrder (no rule) is another rule too.
	w = postJSON(t, NewSendOrderHandler(b.queue, true), "/api/sendOrder", orderWithRule(t, "cart-1", ""))
	if w.Code != http.StatusConflict {
		t.Fatalf("v1 repeat of a ruled order: %d %s", w.Code, w.Body)
	}
}

// No rule on v2 = exactly v1's answer: no echo, today's counter.
func TestSendOrderV2_NoRuleIsTodaysAnswer(t *testing.T) {
	b := newV2Box(t)
	for name, rule := range map[string]string{"absent": "", "null": "null"} {
		w := postJSON(t, NewSendOrderV2Handler(b.queue, b.cfg), "/api/v2/sendOrder", orderWithRule(t, "plain-"+name, rule))
		m := decodeBody(t, w)
		if w.Code != http.StatusAccepted {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body)
		}
		for _, k := range []string{"counter", "number", "fileName", "folder"} {
			if _, ok := m[k]; ok {
				t.Errorf("%s: %s echoed for an order without a rule", name, k)
			}
		}
	}
}

// /api/sendOrder (v1) ignores documentRule, as every older connector did.
func TestSendOrderV1_IgnoresDocumentRule(t *testing.T) {
	b := newV2Box(t)
	w := postJSON(t, NewSendOrderHandler(b.queue, true), "/api/sendOrder", orderWithRule(t, "v1", `{"v":2,"folder":"nope"}`))
	if w.Code != http.StatusAccepted || decodeBody(t, w)["jobId"] != "1000123" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if _, ok := decodeBody(t, w)["counter"]; ok {
		t.Error("v1 echoed a rule")
	}
}

func TestSendOrderV2_Refusals(t *testing.T) {
	tests := []struct {
		name, rule string
		status     int
		field      string
	}{
		{"not an object", `"herp"`, 400, "documentRule"},
		{"array", `[1]`, 400, "documentRule"},
		{"unknown key", `{"v":1,"folder":"herp","counter":"b2b","fileName":"x.doc"}`, 400, "documentRule/fileName"},
		{"path key", `{"v":1,"folder":"herp","counter":"b2b","path":"C:\\x"}`, 400, "documentRule/path"},
		{"wrong case", `{"v":1,"Folder":"herp","counter":"b2b"}`, 400, "documentRule/Folder"},
		{"duplicate", `{"v":1,"folder":"herp","folder":"herp_test","counter":"b2b"}`, 400, "documentRule/folder"},
		{"v missing", `{"folder":"herp","counter":"b2b"}`, 400, "documentRule/v"},
		{"v 2", `{"v":2,"folder":"herp","counter":"b2b"}`, 400, "documentRule/v"},
		{"v as a string", `{"v":"1","folder":"herp","counter":"b2b"}`, 400, "documentRule/v"},
		{"v 1.0", `{"v":1.0,"folder":"herp","counter":"b2b"}`, 400, "documentRule/v"},
		{"folder missing", `{"v":1,"counter":"b2b"}`, 400, "documentRule/folder"},
		{"folder a number", `{"v":1,"folder":5,"counter":"b2b"}`, 400, "documentRule/folder"},
		{"counter empty", `{"v":1,"folder":"herp","counter":""}`, 400, "documentRule/counter"},
		{"temporary a string", `{"v":1,"folder":"herp","counter":"b2b","temporary":"false"}`, 400, "documentRule/temporary"},
		{"unknown folder", `{"v":1,"folder":"nope","counter":"b2b"}`, 400, "documentRule/folder"},
		{"folder as a path", `{"v":1,"folder":"C:\\xampp\\htdocs\\herp","counter":"b2b"}`, 400, "documentRule/folder"},
		{"receipts folder", `{"v":1,"folder":"herp_receipts","counter":"b2b"}`, 400, "documentRule/folder"},
		{"unknown counter", `{"v":1,"folder":"herp","counter":"nope"}`, 400, "documentRule/counter"},
		{"receipts counter", `{"v":1,"folder":"herp_test","counter":"receipts_test"}`, 400, "documentRule/counter"},
		{"test counter into production", `{"v":1,"folder":"herp","counter":"b2b_test"}`, 400, "documentRule/folder"},
		{"temporary without a prefix", `{"v":1,"folder":"herp_test","counter":"b2b_test","temporary":true}`, 400, "documentRule/temporary"},
		{"retired counter", `{"v":1,"folder":"herp","counter":"old"}`, 400, "documentRule/counter"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := newV2Box(t)
			w := postJSON(t, NewSendOrderV2Handler(b.queue, b.cfg), "/api/v2/sendOrder", orderWithRule(t, "r-"+tc.name, tc.rule))
			m := decodeBody(t, w)
			details, _ := m["details"].(map[string]any)
			if w.Code != tc.status || details["field"] != tc.field {
				t.Fatalf("%d %s; want %d field %s", w.Code, w.Body, tc.status, tc.field)
			}
			if strings.Contains(w.Body.String(), `C:\\xampp`) {
				t.Error("a value was echoed")
			}
			// Nothing reserved: the next order still gets b2b's next number.
			if c, _ := b.set.Get("b2b"); c != nil {
				if last, _ := c.Last(); last != 1_000_122 {
					t.Errorf("b2b moved to %d", last)
				}
			}
			if _, err := os.Stat(orderIndexFile(filepath.Dir(b.dataDir), "r-"+tc.name)); err == nil {
				t.Error("an index entry was written")
			}
		})
	}
}

func TestSendOrderV2_CounterExhaustedIs503(t *testing.T) {
	b := newV2Box(t)
	h := NewSendOrderV2Handler(b.queue, b.cfg)
	rule := `{"v":1,"folder":"herp_test","counter":"b2b_test"}`
	if w := postJSON(t, h, "/api/v2/sendOrder", orderWithRule(t, "t1", rule)); w.Code != http.StatusAccepted {
		t.Fatalf("the range's one number: %d %s", w.Code, w.Body)
	}
	w := postJSON(t, h, "/api/v2/sendOrder", orderWithRule(t, "t2", rule))
	if w.Code != http.StatusServiceUnavailable || decodeBody(t, w)["code"] != "COUNTER_EXHAUSTED" {
		t.Fatalf("past the range: %d %s", w.Code, w.Body)
	}
}

func TestCountersRoute(t *testing.T) {
	b := newV2Box(t)
	if _, err := b.queue.Accept(hasavshevet.OrderRequest{}); err == nil {
		t.Fatal("an empty order was accepted")
	}
	h := NewCountersHandler(func() []hasavshevet.CounterStatus {
		return hasavshevet.ReportCounters(b.set, b.queue, nil, b.dataDir, b.cfg.SendOrderDir)
	})
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodGet, "/api/counters", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var got []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("%v %s", err, w.Body)
	}
	byKey := map[string]map[string]any{}
	for _, c := range got {
		byKey[c["key"].(string)] = c
		for _, k := range []string{"key", "kind", "min", "max", "last", "highestIssued", "remaining", "test"} {
			if _, ok := c[k]; !ok {
				t.Errorf("%v: no %s", c["key"], k)
			}
		}
	}
	b2b := byKey["b2b"]
	if b2b["kind"] != "orders" || b2b["last"] != float64(1_000_122) || b2b["remaining"] != float64(1_999_999-1_000_122) || b2b["test"] != false {
		t.Errorf("b2b %v", b2b)
	}
	if r := byKey["receipts_test"]; r["kind"] != "receipts" || r["last"] != float64(9_100_004) || r["test"] != true {
		t.Errorf("receipts_test %v", r)
	}
	if byKey["old"]["retired"] != true {
		t.Errorf("old %v", byKey["old"])
	}
	if len(got) != 4 {
		t.Errorf("%d counters", len(got))
	}
}

func TestCountersRoute_HighestIssuedRunsAheadOfTheFile(t *testing.T) {
	b := newV2Box(t)
	if _, err := b.queue.Accept(hasavshevet.OrderRequest{DocumentType: "ORDER", UserExtID: "1", DueDate: "d", CreatedDate: "d", HistoryID: "h1",
		Details: []hasavshevet.OrderLineItem{{Title: "x", SKU: "1", Quantity: 1}}}); err != nil {
		t.Fatal(err)
	}
	// The counter file is moved back by hand while the connector runs.
	c, _ := b.set.Get("b2b")
	if err := os.WriteFile(c.Path, []byte(`{"lastOrderNumber": 1000100}`), 0o600); err != nil {
		t.Fatal(err)
	}
	report := hasavshevet.ReportCounters(b.set, b.queue, nil, b.dataDir, b.cfg.SendOrderDir)
	for _, r := range report {
		if r.Key == "b2b" && (r.Last != 1_000_100 || r.HighestIssued != 1_000_123 || r.Remaining != 1_999_999-1_000_123) {
			t.Errorf("b2b %+v", r)
		}
	}
}

func TestDocumentCapabilitiesRoute(t *testing.T) {
	b := newV2Box(t)
	w := httptest.NewRecorder()
	NewDocumentCapabilitiesHandler(b.cfg, b.set)(w, httptest.NewRequest(http.MethodGet, "/api/documents/capabilities", nil))
	want := `{"contract":1,"folders":[{"key":"herp","kind":"orders","test":false},{"key":"herp_receipts","kind":"receipts","test":false},{"key":"herp_test","kind":"orders","test":true}],` +
		`"counters":[{"key":"b2b","kind":"orders","test":false},{"key":"b2b_test","kind":"orders","test":true},{"key":"receipts_test","kind":"receipts","test":true}],"placeholders":["prefix","n"]}`
	if got := strings.TrimSpace(w.Body.String()); w.Code != 200 || got != want {
		t.Errorf("%d\n got %s\nwant %s", w.Code, got, want)
	}

	// Today's box: no folder, the implicit counters.
	legacy := config.Config{ERP: config.ERPHasavshevet, SendOrderDir: t.TempDir()}
	w = httptest.NewRecorder()
	NewDocumentCapabilitiesHandler(legacy, hasavshevet.NewCounterSet(legacy, t.TempDir()))(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := strings.TrimSpace(w.Body.String()); got != `{"contract":1,"folders":[],"counters":[{"key":"orders","kind":"orders","test":false}],"placeholders":["prefix","n"]}` {
		t.Errorf("legacy: %s", got)
	}
}

// Review M1: behind the link the daemon refuses a typed folder others can
// write to — 503 FOLDER_WRITABLE, nothing reserved — and so does a receipt.
func TestSendOrderV2_RefusedFolderIs503(t *testing.T) {
	b := newV2Box(t)
	b.queue.RefuseFolders(map[string]string{"herp": `it can be written by BUILTIN\Users`})
	w := postJSON(t, NewSendOrderV2Handler(b.queue, b.cfg), "/api/v2/sendOrder", orderWithRule(t, "f1", `{"v":1,"folder":"herp","counter":"b2b"}`))
	if w.Code != http.StatusServiceUnavailable || decodeBody(t, w)["code"] != "FOLDER_WRITABLE" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "Users") {
		t.Error("the answer names the box's accounts (the log does)")
	}
	if c, _ := b.set.Get("b2b"); c != nil {
		if last, _ := c.Last(); last != 1_000_122 {
			t.Errorf("b2b moved to %d", last)
		}
	}
	rw := httptest.NewRecorder()
	writeReceiptError(rw, nil, "k", fmt.Errorf("%w: the receipt folder", hasavshevet.ErrFolderWritable))
	if rw.Code != http.StatusServiceUnavailable || decodeBody(t, rw)["code"] != "FOLDER_WRITABLE" {
		t.Fatalf("receipt: %d %s", rw.Code, rw.Body)
	}
}

// Review m3: a repeat of a known historyId is answered against the rule
// frozen at its reservation, even when its folder or counter key no longer
// exists on the box — a 400 would read as "order refused" for an order that
// was written. Only a new order is checked against the box's keys.
func TestSendOrderV2_RepeatAfterItsKeysAreGone(t *testing.T) {
	b := newV2Box(t)
	h := NewSendOrderV2Handler(b.queue, b.cfg)
	for _, tc := range []struct{ key, rule string }{
		{"cart-herp", `{"v":1,"folder":"herp","counter":"b2b"}`},
		{"cart-test", `{"v":1,"folder":"herp_test","counter":"b2b_test"}`},
	} {
		if w := postJSON(t, h, "/api/v2/sendOrder", orderWithRule(t, tc.key, tc.rule)); w.Code != http.StatusAccepted {
			t.Fatalf("%s: %d %s", tc.key, w.Code, w.Body)
		}
	}

	// The documents command removed herp (nothing in flight for it), a restore
	// dropped b2b_test; the service restarted on that config.
	cfg := b.cfg
	cfg.Documents = b.cfg.Documents.Clone()
	delete(cfg.Documents.Folders, "herp")
	delete(cfg.Documents.Counters, "b2b_test")
	if err := cfg.ValidateOrders(); err != nil {
		t.Fatal(err)
	}
	set := hasavshevet.NewCounterSet(cfg, b.dataDir)
	q := hasavshevet.NewOrderQueue(hasavshevet.NewSender(nil, cfg, set.OrderNumberStore(), &noopLogger{}), &noopLogger{})
	q.UseCounters(set)
	q.UseKeyIndex(b.dataDir)
	if err := q.Recover(); err != nil {
		t.Fatal(err)
	}
	h = NewSendOrderV2Handler(q, cfg)

	w := postJSON(t, h, "/api/v2/sendOrder", orderWithRule(t, "cart-herp", `{"v":1,"folder":"herp","counter":"b2b"}`))
	if m := decodeBody(t, w); w.Code != http.StatusAccepted || m["replay"] != true || m["fileName"] != "IMOVEIN_1000123.doc" || m["folder"] != "herp" {
		t.Fatalf("repeat after its folder is gone: %d %s", w.Code, w.Body)
	}
	w = postJSON(t, h, "/api/v2/sendOrder", orderWithRule(t, "cart-test", `{"v":1,"folder":"herp_test","counter":"b2b_test"}`))
	if m := decodeBody(t, w); w.Code != http.StatusAccepted || m["replay"] != true || m["counter"] != "b2b_test" {
		t.Fatalf("repeat after its counter is gone: %d %s", w.Code, w.Body)
	}
	// Another rule on a known key is still 409, even naming keys the box lacks.
	w = postJSON(t, h, "/api/v2/sendOrder", orderWithRule(t, "cart-herp", `{"v":1,"folder":"nope","counter":"b2b"}`))
	if w.Code != http.StatusConflict {
		t.Fatalf("another rule: %d %s", w.Code, w.Body)
	}
	// A new order naming the folder that is gone: 400, nothing reserved.
	w = postJSON(t, h, "/api/v2/sendOrder", orderWithRule(t, "cart-new", `{"v":1,"folder":"herp","counter":"b2b"}`))
	if details, _ := decodeBody(t, w)["details"].(map[string]any); w.Code != http.StatusBadRequest || details["field"] != "documentRule/folder" {
		t.Fatalf("new order into a folder that is gone: %d %s", w.Code, w.Body)
	}
	if c, _ := set.Get("b2b"); c != nil {
		if last, _ := c.Last(); last != 1_000_123 {
			t.Errorf("b2b moved to %d", last)
		}
	}
}
