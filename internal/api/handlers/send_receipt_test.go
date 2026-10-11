package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
)

// wire.json is the backend's golden sendReceipt payload (backend
// api/tests/Fixtures/receipt/wire.json, DIG-928), copied byte for byte. If the
// backend changes it, re-copy it and update the hash — the two sides must
// decode the same bytes.
const (
	wireFixture       = "testdata/receipt/wire.json"
	wireFixtureSHA256 = "5afaee0926093c31cf3da5f738fd11cff77a0e7422399e9bdbcdaec76f76b665"
	wireKey           = "3f2c9a1e-7b4d-4c8e-9a51-2d6f0b8e4c17"
)

// wireGolden is the fixture as read at package init, for table cases that
// edit its text; wireBytes re-reads and re-checks it.
var wireGolden, _ = os.ReadFile(wireFixture)

func wireBytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(wireFixture)
	if err != nil {
		t.Fatalf("read %s: %v", wireFixture, err)
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != wireFixtureSHA256 {
		t.Fatalf("%s sha256 = %s; want %s — it is no longer the backend's golden payload", wireFixture, got, wireFixtureSHA256)
	}
	return b
}

// wireMap is the golden payload as a map, for cases that change one field.
func wireMap(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(wireBytes(t), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

type receiptInstall struct {
	sendDir, dataDir string
	writer           *hasavshevet.ReceiptWriter
}

// newReceiptInstall is a connector install with a real writer; seed 0 leaves
// the number store unseeded.
func newReceiptInstall(t *testing.T, seed int64) *receiptInstall {
	t.Helper()
	root := t.TempDir()
	in := &receiptInstall{sendDir: filepath.Join(root, "herp"), dataDir: filepath.Join(root, "data")}
	if err := os.MkdirAll(in.sendDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if seed > 0 {
		if err := hasavshevet.NewReceiptNumberStore(hasavshevet.ReceiptNumberStorePath(in.dataDir)).Seed(seed); err != nil {
			t.Fatal(err)
		}
	}
	w, err := hasavshevet.NewReceiptWriter(hasavshevet.ReceiptWriterConfig{SendReceiptDir: in.sendDir, DataDir: in.dataDir}, &noopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Recover(); err != nil {
		t.Fatal(err)
	}
	in.writer = w
	return in
}

func (in *receiptInstall) nextNumber(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(hasavshevet.ReceiptNumberStorePath(in.dataDir))
	if err != nil {
		return "unseeded"
	}
	var f struct {
		Next json.Number `json:"nextReceiptNumber"`
	}
	_ = json.Unmarshal(b, &f)
	return f.Next.String()
}

func postReceipt(t *testing.T, h http.Handler, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/sendReceipt", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type errorBody struct {
	Error   string         `json:"error"`
	Code    string         `json:"code"`
	Details map[string]any `json:"details"`
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) errorBody {
	t.Helper()
	var e errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("error body is not the envelope: %v (%s)", err, rec.Body.String())
	}
	return e
}

// The contract test: the backend's golden bytes decode, write a receipt, and
// are answered with exactly the §2 response.
func TestSendReceipt_GoldenWirePayloadWritesAndAnswers200(t *testing.T) {
	in := newReceiptInstall(t, 1000)
	h := NewSendReceiptHandler(in.writer, true, &noopLogger{})

	rec := postReceipt(t, h, wireBytes(t))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body %s)", rec.Code, rec.Body.String())
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"receiptNumber":  `"1000"`,
		"written":        `true`,
		"idempotencyKey": `"` + wireKey + `"`,
		"import":         `"not_run"`,
		"warnings":       `[]`,
	}
	if len(raw) != len(want) {
		t.Errorf("response keys = %v; want exactly %v", keysOf(raw), keysOf(want))
	}
	for k, v := range want {
		if got := string(raw[k]); got != v {
			t.Errorf("%s = %s; want %s", k, got, v)
		}
	}
	if strings.Contains(rec.Body.String(), in.dataDir) || strings.Contains(rec.Body.String(), in.sendDir) {
		t.Errorf("the response leaks a file path: %s", rec.Body.String())
	}

	doc, err := os.ReadFile(filepath.Join(in.sendDir, "KUPAIN_1000.doc"))
	if err != nil {
		t.Fatalf("KUPAIN_1000.doc not written: %v", err)
	}
	if len(doc) != 1260 {
		t.Errorf("KUPAIN_1000.doc is %d bytes; want 1259 + LF", len(doc))
	}

	// A repeat is the same receipt.
	again := postReceipt(t, h, wireBytes(t))
	if again.Code != http.StatusOK || !bytes.Equal(again.Body.Bytes(), rec.Body.Bytes()) {
		t.Errorf("repeat = %d %s; want the first answer again", again.Code, again.Body.String())
	}
	if n := in.nextNumber(t); n != "1001" {
		t.Errorf("next number = %s; want 1001", n)
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Request faults are 400 VALIDATION_ERROR naming the field, and burn no number.
func TestSendReceipt_RequestFaultsAre400NamingTheField(t *testing.T) {
	set := func(path string, v any) func(map[string]any) {
		return func(m map[string]any) {
			parts := strings.Split(path, "/")
			cur := m
			for _, p := range parts[:len(parts)-1] {
				if p == "details" {
					cur = m["details"].([]any)[0].(map[string]any)
					continue
				}
				cur = cur[p].(map[string]any)
			}
			cur[parts[len(parts)-1]] = v
		}
	}
	del := func(path string) func(map[string]any) {
		return func(m map[string]any) {
			parts := strings.Split(path, "/")
			cur := m
			for _, p := range parts[:len(parts)-1] {
				if p == "details" {
					cur = m["details"].([]any)[0].(map[string]any)
					continue
				}
				cur = cur[p].(map[string]any)
			}
			delete(cur, parts[len(parts)-1])
		}
	}

	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		raw    string // used instead of the mutated golden when set
		field  string
	}{
		{"unknown top-level field", set("surprise", 1), "", "surprise"},
		{"unknown account field", set("account/surprise", 1), "", "account/surprise"},
		{"unknown detail field", set("details/surprise", 1), "", "details/surprise"},
		// encoding/json would match these case-insensitively and fill the field.
		{"key in another case", nil, strings.Replace(string(wireGolden), `"amountMinor": 694700`, `"AmountMinor": 694700`, 1), "AmountMinor"},
		{"nested key in another case", nil, strings.Replace(string(wireGolden), `"syncKey"`, `"SYNCKEY"`, 1), "account/SYNCKEY"},
		// encoding/json would keep the last of two keys.
		{"duplicate top-level key", nil, strings.Replace(string(wireGolden), `"currency": "ILS",`, `"currency": "ILS", "currency": "USD",`, 1), "currency"},
		{"duplicate nested key", nil, strings.Replace(string(wireGolden), `"city": "חיפה",`, `"city": "חיפה", "city": "תל אביב",`, 1), "account/city"},
		{"duplicate key in a detail", nil, strings.Replace(string(wireGolden), `"reference": "19057",`, `"reference": "19057", "reference": "X",`, 1), "details/reference"},
		{"body over 1 MiB", nil, `{"remarkSuffix":"` + strings.Repeat("a", 1<<20) + `"}`, "body"},
		{"missing idempotencyKey", del("idempotencyKey"), "", "idempotencyKey"},
		{"missing account", del("account"), "", "account"},
		{"missing account/syncKey", del("account/syncKey"), "", "account/syncKey"},
		{"missing account/name", del("account/name"), "", "account/name"},
		{"null account/name", set("account/name", nil), "", "account/name"},
		{"missing settledAt", del("settledAt"), "", "settledAt"},
		{"missing amountMinor", del("amountMinor"), "", "amountMinor"},
		{"missing currency", del("currency"), "", "currency"},
		{"missing installments", del("installments"), "", "installments"},
		{"missing last4", del("last4"), "", "last4"},
		{"missing confirmationCode", del("confirmationCode"), "", "confirmationCode"},
		{"missing providerTransactionId", del("providerTransactionId"), "", "providerTransactionId"},
		{"blank providerTransactionId", set("providerTransactionId", "  "), "", "providerTransactionId"},
		{"providerTransactionId over 64 bytes", set("providerTransactionId", strings.Repeat("9", 65)), "", "providerTransactionId"},
		{"missing details", del("details"), "", "details"},
		{"empty details", set("details", []any{}), "", "details"},
		{"missing details/reference", del("details/reference"), "", "details/reference"},
		{"missing details/amountMinor", del("details/amountMinor"), "", "details/amountMinor"},
		{"amountMinor as a string", set("amountMinor", "694700"), "", "amountMinor"},
		{"amountMinor fractional", set("amountMinor", 6947.5), "", "amountMinor"},
		{"account/syncKey as a number", set("account/syncKey", 40217), "", "account/syncKey"},
		{"details/amountMinor as a string", set("details/amountMinor", "x"), "", "details/amountMinor"},
		{"account as a string", set("account", "x"), "", "account"},
		{"currency USD", set("currency", "USD"), "", "currency"},
		{"currency lower-case", set("currency", "ils"), "", "currency"},
		{"last4 not digits", set("last4", "12a4"), "", "last4"},
		{"last4 too short", set("last4", "123"), "", "last4"},
		{"idempotencyKey not a UUID", set("idempotencyKey", "not-a-uuid"), "", "idempotencyKey"},
		{"idempotencyKey a path", set("idempotencyKey", "../../../../etc/passwd"), "", "idempotencyKey"},
		{"settledAt not a date", set("settledAt", "08/10/2026"), "", "settledAt"},
		{"amountMinor zero", set("amountMinor", 0), "", "amountMinor"},
		{"installments 100", set("installments", 100), "", "installments"},
		{"account/name too wide", set("account/name", strings.Repeat("א", 51)), "", "account/name"},
		{"malformed JSON", nil, `{"idempotencyKey":`, "body"},
		{"trailing data", nil, `{} {}`, "body"},
		{"not an object", nil, `[]`, "body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := newReceiptInstall(t, 1000)
			h := NewSendReceiptHandler(in.writer, true, &noopLogger{})
			body := []byte(tc.raw)
			if tc.raw == "" {
				m := wireMap(t)
				tc.mutate(m)
				body = mustJSON(t, m)
			}

			rec := postReceipt(t, h, body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400 (body %s)", rec.Code, rec.Body.String())
			}
			e := decodeError(t, rec)
			if e.Code != "VALIDATION_ERROR" {
				t.Errorf("code = %q; want VALIDATION_ERROR", e.Code)
			}
			if got, _ := e.Details["field"].(string); got != tc.field {
				t.Errorf("details.field = %q; want %q (error %q)", got, tc.field, e.Error)
			}
			if n := in.nextNumber(t); n != "1000" {
				t.Errorf("a refused request moved the store to %s", n)
			}
			if docs, _ := filepath.Glob(filepath.Join(in.sendDir, "KUPAIN_*")); len(docs) != 0 {
				t.Errorf("a refused request wrote %v", docs)
			}
		})
	}
}

// Nullable fields accept null and absence alike.
func TestSendReceipt_NullableFieldsMayBeNullOrAbsent(t *testing.T) {
	for _, field := range []string{"email", "firstPaymentMinor", "cardBrand", "cardIssuer", "authorizationRef", "remarkSuffix"} {
		for _, absent := range []bool{false, true} {
			in := newReceiptInstall(t, 1)
			m := wireMap(t)
			if absent {
				delete(m, field)
			} else {
				m[field] = nil
			}
			rec := postReceipt(t, NewSendReceiptHandler(in.writer, true, &noopLogger{}), mustJSON(t, m))
			if rec.Code != http.StatusOK {
				t.Errorf("%s absent=%v: status = %d (%s); want 200", field, absent, rec.Code, rec.Body.String())
			}
		}
	}
	in := newReceiptInstall(t, 1)
	m := wireMap(t)
	acc := m["account"].(map[string]any)
	for _, f := range []string{"address", "city", "phone", "agentSyncKey"} {
		acc[f] = nil
	}
	if rec := postReceipt(t, NewSendReceiptHandler(in.writer, true, &noopLogger{}), mustJSON(t, m)); rec.Code != http.StatusOK {
		t.Errorf("null account extras: status = %d (%s); want 200", rec.Code, rec.Body.String())
	}
}

// Without sendReceiptDir the route answers 503 before reading anything.
func TestSendReceipt_NotConfiguredIs503AndTouchesNothing(t *testing.T) {
	h := NewSendReceiptHandler(nil, false, &noopLogger{})
	for _, body := range [][]byte{wireBytes(t), []byte(`garbage`)} {
		rec := postReceipt(t, h, body)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d; want 503", rec.Code)
		}
		if e := decodeError(t, rec); e.Code != "RECEIPTS_NOT_CONFIGURED" {
			t.Errorf("code = %q; want RECEIPTS_NOT_CONFIGURED", e.Code)
		}
	}
}

func TestSendReceipt_UnseededStoreIs503(t *testing.T) {
	in := newReceiptInstall(t, 0)
	rec := postReceipt(t, NewSendReceiptHandler(in.writer, true, &noopLogger{}), wireBytes(t))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503 (%s)", rec.Code, rec.Body.String())
	}
	e := decodeError(t, rec)
	if e.Code != "RECEIPT_NUMBER_STORE_UNAVAILABLE" {
		t.Errorf("code = %q; want RECEIPT_NUMBER_STORE_UNAVAILABLE", e.Code)
	}
	if strings.Contains(e.Error, in.dataDir) {
		t.Errorf("the error leaks a path: %q", e.Error)
	}
}

// fakeReceiptWriter answers what the test tells it to.
type fakeReceiptWriter struct {
	res    *hasavshevet.ReceiptResult
	err    error
	called int
	gotReq hasavshevet.ReceiptRequest
}

func (f *fakeReceiptWriter) Write(_ context.Context, req hasavshevet.ReceiptRequest) (*hasavshevet.ReceiptResult, error) {
	f.called++
	f.gotReq = req
	return f.res, f.err
}

// Faults that are not the request's are 5xx — never 400, which the backend
// would treat as a payload it must not resend.
func TestSendReceipt_InternalFaultsAre5xx(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"card-name config", &hasavshevet.CardNamesConfigError{Table: "cardIssuer", Name: "x", Reason: "too wide"}, 500, "RECEIPT_CONFIG_INVALID"},
		{"store unavailable", fmt.Errorf("wrapped: %w", hasavshevet.ErrStoreUnavailable), 503, "RECEIPT_NUMBER_STORE_UNAVAILABLE"},
		{"index unreadable", fmt.Errorf("%w: /secret/data/receipts/index/x.json", hasavshevet.ErrReceiptIndexUnreadable), 500, "RECEIPT_INDEX_UNREADABLE"},
		{"stored request no longer builds", fmt.Errorf("%w: receipt 9 (key k): last4: must be exactly 4 digits", hasavshevet.ErrStoredReceiptUnbuildable), 500, "RECEIPT_STORED_REQUEST_INVALID"},
		{"lock wait ran out", context.DeadlineExceeded, 503, "RECEIPT_WRITER_BUSY"},
		{"disk", errors.New("open /secret/herp/KUPAIN_1.doc: permission denied"), 500, "RECEIPT_WRITE_FAILED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeReceiptWriter{err: tc.err}
			rec := postReceipt(t, NewSendReceiptHandler(f, true, &noopLogger{}), wireBytes(t))
			if rec.Code != tc.status {
				t.Fatalf("status = %d; want %d (%s)", rec.Code, tc.status, rec.Body.String())
			}
			e := decodeError(t, rec)
			if e.Code != tc.code {
				t.Errorf("code = %q; want %q", e.Code, tc.code)
			}
			if strings.Contains(rec.Body.String(), "/secret") {
				t.Errorf("the error leaks a path: %s", rec.Body.String())
			}
		})
	}
}

// The writer's result goes out as-is — import "started" and warnings included —
// and its file paths never do.
func TestSendReceipt_PassesImportAndWarningsThroughButNeverFiles(t *testing.T) {
	f := &fakeReceiptWriter{res: &hasavshevet.ReceiptResult{
		ReceiptNumber:  42,
		IdempotencyKey: wireKey,
		Written:        true,
		Import:         hasavshevet.ImportStarted,
		Warnings:       []string{"no card name for issuer \"6\" / brand \"2\"; card name (F17) left blank"},
		Files:          []string{`C:\ProgramData\digi-erp-connector\history\receipts\42\KUPAIN_42.doc`},
	}}
	rec := postReceipt(t, NewSendReceiptHandler(f, true, &noopLogger{}), wireBytes(t))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		ReceiptNumber string   `json:"receiptNumber"`
		Import        string   `json:"import"`
		Warnings      []string `json:"warnings"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.ReceiptNumber != "42" || body.Import != "started" || len(body.Warnings) != 1 {
		t.Errorf("body = %+v", body)
	}
	if strings.Contains(rec.Body.String(), "ProgramData") || strings.Contains(rec.Body.String(), "KUPAIN") {
		t.Errorf("the response carries file paths: %s", rec.Body.String())
	}
}

// The decoded request reaches the writer field for field.
func TestSendReceipt_MapsTheWireToTheWriter(t *testing.T) {
	f := &fakeReceiptWriter{res: &hasavshevet.ReceiptResult{ReceiptNumber: 1, IdempotencyKey: wireKey, Written: true, Import: hasavshevet.ImportNotRun}}
	if rec := postReceipt(t, NewSendReceiptHandler(f, true, &noopLogger{}), wireBytes(t)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	got := f.gotReq
	if got.IdempotencyKey != wireKey || got.Account.SyncKey != "40217" || got.Account.Name != `בנייני לוי בע"מ` ||
		got.Account.Address != "הרצל 1" || got.Account.City != "חיפה" || got.Account.Phone != "04-8123456" ||
		got.Account.AgentSyncKey != "7" || got.Email != "dana@levi-building.example" || got.SettledAt != "2026-10-08" ||
		got.AmountMinor != 694700 || got.Currency != "ILS" || got.Installments != 1 || got.FirstPaymentMinor != nil ||
		got.CardBrand != "2" || got.CardIssuer != "2" || got.Last4 != "4580" || got.ConfirmationCode != "0123456" ||
		got.AuthorizationRef != "26100809142708822928912" || got.ProviderTransactionID != "81122394" ||
		got.RemarkSuffix != "2824019" {
		t.Errorf("writer got %+v", got)
	}
	if len(got.Details) != 3 || got.Details[0] != (hasavshevet.ReceiptDetail{Reference: "26004295", AmountMinor: 710700}) ||
		got.Details[1] != (hasavshevet.ReceiptDetail{Reference: "19057", AmountMinor: -17500}) ||
		got.Details[2] != (hasavshevet.ReceiptDetail{Reference: "OI-77", AmountMinor: 1500}) {
		t.Errorf("details = %+v", got.Details)
	}
}

// End to end: a reserved receipt whose stored request no longer builds is a
// 500 for whoever comes next — never a 400 blaming a request that is fine.
func TestSendReceipt_UnbuildableStoredReceiptIs500(t *testing.T) {
	in := newReceiptInstall(t, 1000)
	stored := `{"key":"00000000-0000-4000-8000-000000000001","number":1000,"state":"reserved",` +
		`"request":{"idempotencyKey":"00000000-0000-4000-8000-000000000001","account":{"syncKey":"1","name":"n","address":"","city":"","phone":"","agentSyncKey":""},` +
		`"email":"","settledAt":"2026-10-08","amountMinor":100,"currency":"ILS","installments":1,"firstPaymentMinor":null,` +
		`"cardBrand":"","cardIssuer":"","last4":"12a4","confirmationCode":"1","authorizationRef":"","providerTransactionId":"1",` +
		`"details":[{"reference":"r","amountMinor":100}],"remarkSuffix":""},"files":null,"import":"not_run","reservedAt":"2026-10-11T00:00:00Z"}`
	idx := filepath.Join(in.dataDir, "receipts", "index", "00000000-0000-4000-8000-000000000001.json")
	if err := os.MkdirAll(filepath.Dir(idx), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idx, []byte(stored), 0o600); err != nil {
		t.Fatal(err)
	}

	// A restart: a fresh writer, whose first Write recovers the planted entry.
	w, err := hasavshevet.NewReceiptWriter(hasavshevet.ReceiptWriterConfig{SendReceiptDir: in.sendDir, DataDir: in.dataDir}, &noopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	rec := postReceipt(t, NewSendReceiptHandler(w, true, &noopLogger{}), wireBytes(t))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d (%s); want 500", rec.Code, rec.Body.String())
	}
	if e := decodeError(t, rec); e.Code != "RECEIPT_STORED_REQUEST_INVALID" {
		t.Errorf("code = %q; want RECEIPT_STORED_REQUEST_INVALID", e.Code)
	}
}
