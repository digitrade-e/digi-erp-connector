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

// wire.json and wire-blanks.json are the backend's REAL serializer output:
// DIG-928's Model/Outbound/Receipt, jsonSerialize()d and encoded by Symfony
// HttpClient exactly as the outbound path sends it (testdata/receipt/generate.php,
// run in a throwaway backend-php container). They are the contract: the
// connector must take these bytes as they are. If the backend's Receipt
// changes, regenerate them and update the hashes — never hand-edit them.
const (
	wireFixture             = "testdata/receipt/wire.json"
	wireFixtureSHA256       = "9fe5d4d43c17d6f52f59b84c993b3e3ccfe9fe7683b05c9ff06ab3bc53c6e510"
	wireKey                 = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	wireBlanksFixture       = "testdata/receipt/wire-blanks.json"
	wireBlanksFixtureSHA256 = "fc62a904b67cba00862081c018db91bf867983e8ef78ad10c375dbb76cc069bc"
)

// receiptWireKeys is every key Receipt::jsonSerialize() emits, in its order,
// with the JSON type it emits it as.
var receiptWireKeys = []struct {
	key  string
	kind string // "string" or "int"
}{
	{"idempotencyKey", "string"},
	{"accountKey", "string"},
	{"settledDate", "string"},
	{"amountMinor", "int"},
	{"installments", "int"},
	{"cardIssuer", "string"},
	{"cardBrand", "string"},
	{"last4", "string"},
	{"confirmationCode", "string"},
	{"providerUid", "string"},
	{"remarks", "string"},
	{"agentKey", "string"},
	{"name", "string"},
	{"address", "string"},
	{"phone", "string"},
	{"email", "string"},
}

// wireGolden is the fixture as read at package init, for table cases that
// edit its text; wireBytes re-reads and re-checks it.
var wireGolden, _ = os.ReadFile(wireFixture)

func fixtureBytes(t *testing.T, path, sha string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != sha {
		t.Fatalf("%s sha256 = %s; want %s — it is no longer the backend's serializer output", path, got, sha)
	}
	return b
}

func wireBytes(t *testing.T) []byte { return fixtureBytes(t, wireFixture, wireFixtureSHA256) }

// wireMap is the golden payload as a map, for cases that change one field.
// Numbers stay json.Number so a re-encoded map keeps 693200 an integer.
func wireMap(t *testing.T) map[string]any {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(wireBytes(t)))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil {
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
	t.Cleanup(func() { _ = w.Close() })
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

// The contract test: the backend's own bytes decode, write a receipt, and are
// answered with exactly the §4.6a response.
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

// Everything the backend may leave blank ("" — it never sends null), blank:
// still a receipt, with F17 left blank and said so.
func TestSendReceipt_BlankOptionalFieldsAreAReceipt(t *testing.T) {
	in := newReceiptInstall(t, 7)
	rec := postReceipt(t, NewSendReceiptHandler(in.writer, true, &noopLogger{}),
		fixtureBytes(t, wireBlanksFixture, wireBlanksFixtureSHA256))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		ReceiptNumber string   `json:"receiptNumber"`
		Warnings      []string `json:"warnings"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.ReceiptNumber != "7" || len(body.Warnings) != 1 || !strings.Contains(body.Warnings[0], "F17") {
		t.Errorf("body = %s; want receipt 7 with one F17 warning", rec.Body.String())
	}
}

// The decoder takes EXACTLY the keys Receipt::jsonSerialize() emits — every
// one, as the type it emits it as.
func TestSendReceipt_DecoderMatchesTheBackendKeySet(t *testing.T) {
	m := wireMap(t)
	if len(m) != len(receiptWireKeys) {
		t.Fatalf("fixture has %d keys; the contract table has %d", len(m), len(receiptWireKeys))
	}
	for _, k := range receiptWireKeys {
		v, ok := m[k.key]
		if !ok {
			t.Fatalf("fixture lacks %q", k.key)
		}
		switch k.kind {
		case "int":
			if _, isNum := v.(json.Number); !isNum {
				t.Errorf("fixture %s = %#v; the backend sends an int", k.key, v)
			}
		case "string":
			if _, isStr := v.(string); !isStr {
				t.Errorf("fixture %s = %#v; the backend sends a string", k.key, v)
			}
		}
	}

	for _, k := range receiptWireKeys {
		k := k
		wrong := any(json.Number("1"))
		if k.kind == "int" {
			wrong = "1"
		}
		for _, tc := range []struct {
			name   string
			mutate func(map[string]any)
		}{
			{"missing", func(m map[string]any) { delete(m, k.key) }},
			{"null", func(m map[string]any) { m[k.key] = nil }},
			{"wrong JSON type", func(m map[string]any) { m[k.key] = wrong }},
		} {
			t.Run(k.key+"/"+tc.name, func(t *testing.T) {
				in := newReceiptInstall(t, 1000)
				m := wireMap(t)
				tc.mutate(m)
				expect400(t, in, mustJSON(t, m), k.key)
			})
		}
	}
}

func expect400(t *testing.T, in *receiptInstall, body []byte, field string) {
	t.Helper()
	rec := postReceipt(t, NewSendReceiptHandler(in.writer, true, &noopLogger{}), body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 (body %s)", rec.Code, rec.Body.String())
	}
	e := decodeError(t, rec)
	if e.Code != "VALIDATION_ERROR" {
		t.Errorf("code = %q; want VALIDATION_ERROR", e.Code)
	}
	if got, _ := e.Details["field"].(string); got != field {
		t.Errorf("details.field = %q; want %q (error %q)", got, field, e.Error)
	}
	if n := in.nextNumber(t); n != "1000" {
		t.Errorf("a refused request moved the store to %s", n)
	}
	if docs, _ := filepath.Glob(filepath.Join(in.sendDir, "KUPAIN_*")); len(docs) != 0 {
		t.Errorf("a refused request wrote %v", docs)
	}
}

// Request faults are 400 VALIDATION_ERROR naming the field, and burn no number.
// The backend stops a receipt on a 400 and retries anything else, so a 400
// must mean "this content can never be written".
func TestSendReceipt_RequestFaultsAre400NamingTheField(t *testing.T) {
	set := func(key string, v any) func(map[string]any) {
		return func(m map[string]any) { m[key] = v }
	}
	golden := string(wireGolden)

	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		raw    string // used instead of the mutated golden when set
		field  string
	}{
		{"unknown field", set("surprise", json.Number("1")), "", "surprise"},
		// The 10-11 nested wire is gone; its keys are unknown now.
		{"old nested account", set("account", map[string]any{"syncKey": "1"}), "", "account"},
		{"old currency", set("currency", "ILS"), "", "currency"},
		{"old details", set("details", []any{}), "", "details"},
		{"old firstPaymentMinor", set("firstPaymentMinor", nil), "", "firstPaymentMinor"},
		// encoding/json would match this case-insensitively and fill the field.
		{"key in another case", nil, strings.Replace(golden, `"amountMinor":693200`, `"AmountMinor":693200`, 1), "AmountMinor"},
		{"key in another case, string", nil, strings.Replace(golden, `"last4"`, `"LAST4"`, 1), "LAST4"},
		// encoding/json would keep the last of two keys.
		{"duplicate key", nil, strings.Replace(golden, `"amountMinor":693200,`, `"amountMinor":693200,"amountMinor":1,`, 1), "amountMinor"},
		{"duplicate string key", nil, strings.Replace(golden, `"cardBrand":"2",`, `"cardBrand":"2","cardBrand":"4",`, 1), "cardBrand"},
		{"body over 1 MiB", nil, `{"remarks":"` + strings.Repeat("a", 1<<20) + `"}`, "body"},
		{"malformed JSON", nil, `{"idempotencyKey":`, "body"},
		{"trailing data", nil, golden + ` {}`, "body"},
		{"not an object", nil, `[]`, "body"},

		{"amountMinor as a string", set("amountMinor", "693200"), "", "amountMinor"},
		{"amountMinor fractional", set("amountMinor", json.Number("6932.5")), "", "amountMinor"},
		{"amountMinor with a zero fraction", set("amountMinor", json.Number("693200.0")), "", "amountMinor"},
		{"amountMinor zero", set("amountMinor", json.Number("0")), "", "amountMinor"},
		{"amountMinor negative", set("amountMinor", json.Number("-1")), "", "amountMinor"},
		{"amountMinor over 99 999 999", set("amountMinor", json.Number("100000000")), "", "amountMinor"},
		{"installments 0", set("installments", json.Number("0")), "", "installments"},
		{"installments 3", set("installments", json.Number("3")), "", "installments"},
		{"last4 as a number", set("last4", json.Number("4242")), "", "last4"},
		{"last4 not digits", set("last4", "12a4"), "", "last4"},
		{"last4 too short", set("last4", "424"), "", "last4"},
		{"last4 Arabic-Indic digits", set("last4", "١٢٣٤"), "", "last4"},
		{"cardIssuer 3 digits", set("cardIssuer", "123"), "", "cardIssuer"},
		{"cardIssuer not digits", set("cardIssuer", "V"), "", "cardIssuer"},
		{"cardBrand not digits", set("cardBrand", "x"), "", "cardBrand"},
		{"cardBrand padded", set("cardBrand", " 2"), "", "cardBrand"},
		{"idempotencyKey not a UUID", set("idempotencyKey", "not-a-uuid"), "", "idempotencyKey"},
		{"idempotencyKey a path", set("idempotencyKey", "../../../../etc/passwd"), "", "idempotencyKey"},
		{"settledDate d/m/Y", set("settledDate", "14/10/2026"), "", "settledDate"},
		{"settledDate not a day", set("settledDate", "2026-02-30"), "", "settledDate"},
		{"settledDate with a time", set("settledDate", "2026-10-14T00:00:00"), "", "settledDate"},
		{"accountKey blank", set("accountKey", "  "), "", "accountKey"},
		{"accountKey 16 bytes", set("accountKey", strings.Repeat("9", 16)), "", "accountKey"},
		{"name blank", set("name", ""), "", "name"},
		{"name 51 Hebrew letters", set("name", strings.Repeat("א", 51)), "", "name"},
		{"name in Arabic", set("name", "شركة"), "", "name"},
		{"name with an emoji", set("name", "בל\"פ 🙂"), "", "name"},
		{"confirmationCode blank", set("confirmationCode", ""), "", "confirmationCode"},
		{"confirmationCode 10 bytes", set("confirmationCode", "0123456789"), "", "confirmationCode"},
		{"providerUid 251 bytes", set("providerUid", strings.Repeat("U", 251)), "", "providerUid"},
		{"remarks blank", set("remarks", " "), "", "remarks"},
		{"remarks 251 bytes", set("remarks", strings.Repeat("ר", 251)), "", "remarks"},
		{"agentKey 10 bytes", set("agentKey", strings.Repeat("7", 10)), "", "agentKey"},
		{"address 51 bytes", set("address", strings.Repeat("a", 51)), "", "address"},
		{"phone 51 bytes", set("phone", strings.Repeat("0", 51)), "", "phone"},
		{"email 51 bytes", set("email", strings.Repeat("e", 51)), "", "email"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := newReceiptInstall(t, 1000)
			body := []byte(tc.raw)
			if tc.raw == "" {
				m := wireMap(t)
				tc.mutate(m)
				body = mustJSON(t, m)
			}
			expect400(t, in, body, tc.field)
		})
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
		{"data dir held by another process", fmt.Errorf("%w (/secret/data/receipts/writer.lock)", hasavshevet.ErrReceiptDataInUse), 503, "RECEIPT_DATA_IN_USE"},
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
	want := hasavshevet.ReceiptRequest{
		IdempotencyKey:   wireKey,
		AccountKey:       "30021",
		SettledDate:      "2026-10-14",
		AmountMinor:      693200,
		Installments:     1,
		CardIssuer:       "2",
		CardBrand:        "2",
		Last4:            "4242",
		ConfirmationCode: "0505293",
		ProviderUID:      "25101412345678901234567",
		Remarks:          "26004295, 19057",
		AgentKey:         "7",
		Name:             `בל"פ`,
		Address:          "הרצל 1",
		Phone:            "03-5555555",
		Email:            "pay@example.test",
	}
	if f.gotReq != want {
		t.Errorf("writer got  %+v\nwant        %+v", f.gotReq, want)
	}
}

// End to end: a reserved receipt whose stored request no longer builds is a
// 500 for whoever comes next — never a 400 blaming a request that is fine.
func TestSendReceipt_UnbuildableStoredReceiptIs500(t *testing.T) {
	in := newReceiptInstall(t, 1000)
	stored := `{"key":"00000000-0000-4000-8000-000000000001","number":1000,"state":"reserved",` +
		`"request":{"idempotencyKey":"00000000-0000-4000-8000-000000000001","accountKey":"1","settledDate":"2026-10-08",` +
		`"amountMinor":100,"installments":1,"cardIssuer":"","cardBrand":"","last4":"12a4","confirmationCode":"1",` +
		`"providerUid":"","remarks":"r","agentKey":"","name":"n","address":"","phone":"","email":""},` +
		`"files":null,"import":"not_run","reservedAt":"2026-10-11T00:00:00Z"}`
	idx := filepath.Join(in.dataDir, "receipts", "index", "00000000-0000-4000-8000-000000000001.json")
	if err := os.MkdirAll(filepath.Dir(idx), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idx, []byte(stored), 0o600); err != nil {
		t.Fatal(err)
	}

	// A restart: a fresh writer, whose first Write recovers the planted entry.
	_ = in.writer.Close() // the old process is gone, and its lock with it
	w, err := hasavshevet.NewReceiptWriter(hasavshevet.ReceiptWriterConfig{SendReceiptDir: in.sendDir, DataDir: in.dataDir}, &noopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	// Before TempDir's cleanup: Windows cannot delete a held lock file.
	t.Cleanup(func() { _ = w.Close() })
	rec := postReceipt(t, NewSendReceiptHandler(w, true, &noopLogger{}), wireBytes(t))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d (%s); want 500", rec.Code, rec.Body.String())
	}
	if e := decodeError(t, rec); e.Code != "RECEIPT_STORED_REQUEST_INVALID" {
		t.Errorf("code = %q; want RECEIPT_STORED_REQUEST_INVALID", e.Code)
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
