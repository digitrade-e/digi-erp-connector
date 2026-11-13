package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/api/dto"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
	"golang.org/x/text/encoding/charmap"
)

// DIG-1315: the optional "header" object of POST /api/sendOrder.

// IMOVEIN byte spans of the header fields (0-based, end exclusive), from
// imovein.go's widths.
var headerSpans = map[string][2]int{
	"line5": {26, 76}, "line6": {76, 126}, "line7": {126, 176}, "line35": {724, 754},
	"line45": {1111, 1161}, "line46": {1161, 1211}, "line47": {1211, 1261}, "line48": {1261, 1311},
	"line49": {1311, 1361}, "line50": {1361, 1374}, "line51": {1374, 1387}, "line52": {1387, 1400},
	"line81": {2569, 2582}, "line82": {2582, 2595},
}

func headerLine(t *testing.T, doc []byte, line string) string {
	t.Helper()
	s := headerSpans[line]
	b, err := charmap.Windows1255.NewDecoder().Bytes(doc[s[0]:s[1]])
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(string(b), " ")
}

func sendRawOrder(t *testing.T, handler http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/sendOrder", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler(w, req)
	return w
}

// bodyWithHeader is validOrderBody (with account details, no database) and
// the given raw "header" JSON spliced in; "" leaves "header" out.
func bodyWithHeader(t *testing.T, header string) string {
	t.Helper()
	body := validOrderBody()
	body["account"] = map[string]any{"accountKey": "CUST001", "fullName": "לקוח בדיקה"}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if header == "" {
		return string(b)
	}
	return strings.TrimSuffix(string(b), "}") + `,"header":` + header + `}`
}

// postAndWrite posts body to a fresh queue, waits for order 1 and returns the
// answer and the IMOVEIN it wrote.
func postAndWrite(t *testing.T, body string) (map[string]any, []byte) {
	t.Helper()
	root := t.TempDir()
	q := startedIndexedQueue(t, root)
	w := sendRawOrder(t, NewSendOrderHandler(q, true), body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("POST = %d %s; want 202", w.Code, w.Body.String())
	}
	answer := decodeBody(t, w)
	waitDone(t, q, "1")
	doc, err := os.ReadFile(filepath.Join(root, "orders", "history", "1", "IMOVEIN_1.doc"))
	if err != nil {
		t.Fatal(err)
	}
	return answer, doc
}

// Without "header" the file is the same as with an empty or null one, or one
// whose values are all null or blank — and the answer has no headerApplied
// key at all when "header" is absent or null (its shape before DIG-1315).
func TestSendOrderHandler_NoHeaderIsTodaysOrder(t *testing.T) {
	baseAnswer, base := postAndWrite(t, bodyWithHeader(t, ""))
	if _, ok := baseAnswer["headerApplied"]; ok {
		t.Fatalf("an order without header answers headerApplied: %v", baseAnswer)
	}
	for _, tc := range []struct {
		header   string
		wantEcho bool
	}{
		{`null`, false},
		{`{}`, true},
		{`{"city":null,"extraSum2":null}`, true},
		{`{"city":"","address":"   ","extraText1":""}`, true},
	} {
		t.Run(tc.header, func(t *testing.T) {
			answer, doc := postAndWrite(t, bodyWithHeader(t, tc.header))
			if !bytes.Equal(doc, base) {
				t.Errorf("header %s changed the file", tc.header)
			}
			echo, ok := answer["headerApplied"]
			if ok != tc.wantEcho {
				t.Fatalf("headerApplied present = %v; want %v (%v)", ok, tc.wantEcho, answer)
			}
			if ok {
				if list, isList := echo.([]any); !isList || len(list) != 0 {
					t.Fatalf("headerApplied = %#v; want []", echo)
				}
			}
		})
	}
}

// Every header field reaches its IMOVEIN line (Hebrew in Windows-1255, numbers
// as sent, JSON numbers or strings), and the answer names what was applied, in
// the fixed order, leaving out what was null or blank.
func TestSendOrderHandler_HeaderReachesTheFileAndIsEchoed(t *testing.T) {
	header := `{
		"extraNum2": 1, "extraNum1": "1", "extraSum3": 18, "extraSum2": "99", "extraSum1": 12.50,
		"extraText5": 4500012345, "extraText4": null, "extraText3": "", "extraText2": "הרצל 12",
		"extraText1": "לקוח בדיקה|משה כהן",
		"phone": "050-1234567", "city": "תל אביב", "address": "הרצל 12\nדירה 3", "accountNameDetail": "משה כהן"
	}`
	answer, doc := postAndWrite(t, bodyWithHeader(t, header))
	want := map[string]string{
		"line5": "לקוח בדיקה|משה כהן", "line6": "הרצל 12. דירה 3", "line7": "תל אביב", "line35": "050-1234567",
		"line45": "לקוח בדיקה|משה כהן", "line46": "הרצל 12", "line47": "", "line48": "", "line49": "4500012345",
		"line50": "12.50", "line51": "99", "line52": "18", "line81": "1", "line82": "1",
	}
	for line, w := range want {
		if got := headerLine(t, doc, line); got != w {
			t.Errorf("%s = %q; want %q", line, got, w)
		}
	}
	gotEcho, _ := json.Marshal(answer["headerApplied"])
	wantEcho := `["accountNameDetail","address","city","phone","extraText1","extraText2","extraText5","extraSum1","extraSum2","extraSum3","extraNum1","extraNum2"]`
	if string(gotEcho) != wantEcho {
		t.Fatalf("headerApplied = %s; want %s", gotEcho, wantEcho)
	}
}

// A header the connector cannot write as sent is a 400 VALIDATION_ERROR
// naming the key — an unknown key, a key in another case, a key given twice,
// a value too long or of the wrong kind — before a number is reserved, and
// the value is never echoed.
func TestSendOrderHandler_RefusesABadHeader(t *testing.T) {
	long := strings.Repeat("ש", 51)
	for _, tc := range []struct {
		name, header, field string
	}{
		{"unknown key", `{"extraText6":"x"}`, "header/extraText6"},
		{"free-form key", `{"deliveryNote":"yes"}`, "header/deliveryNote"},
		{"wrong case", `{"ExtraText1":"x"}`, "header/ExtraText1"},
		{"Hashavshevet name", `{"AccountName":"x"}`, "header/AccountName"},
		{"duplicate", `{"city":"a","city":"b"}`, "header/city"},
		{"too long Hebrew", `{"extraText1":"` + long + `"}`, "header/extraText1"},
		{"detail too long with the separator", `{"accountNameDetail":"` + strings.Repeat("א", 50) + `"}`, "header/accountNameDetail"},
		{"phone too long", `{"phone":"` + strings.Repeat("5", 31) + `"}`, "header/phone"},
		{"sum too long", `{"extraSum1":12345678901234}`, "header/extraSum1"},
		{"sum exponent", `{"extraSum2":1e2}`, "header/extraSum2"},
		{"sum text", `{"extraSum2":"yes"}`, "header/extraSum2"},
		{"flag as a boolean", `{"extraNum1":true}`, "header/extraNum1"},
		{"object value", `{"city":{"name":"x"}}`, "header/city"},
		{"array value", `{"city":["x"]}`, "header/city"},
		{"a tab", `{"extraText4":"a\tb"}`, "header/extraText4"},
		{"header is an array", `[{"city":"x"}]`, "header"},
		{"header is a string", `"city"`, "header"},
		{"long unknown key is capped", `{"` + strings.Repeat("k", 200) + `":1}`, capEchoedField("header/" + strings.Repeat("k", 200))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			q := newIndexedTestQueue(t, root)
			w := sendRawOrder(t, NewSendOrderHandler(q, true), bodyWithHeader(t, tc.header))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("POST = %d %s; want 400", w.Code, w.Body.String())
			}
			resp := decodeBody(t, w)
			if resp["code"] != "VALIDATION_ERROR" {
				t.Fatalf("code = %v; want VALIDATION_ERROR (%v)", resp["code"], resp)
			}
			details, _ := resp["details"].(map[string]any)
			if details["field"] != tc.field {
				t.Fatalf("details.field = %v; want %s (%v)", details["field"], tc.field, resp)
			}
			if strings.Contains(w.Body.String(), long[:20]) || strings.Contains(w.Body.String(), "a\\tb") {
				t.Fatalf("the refusal echoes the value: %s", w.Body.String())
			}
			if _, err := os.Stat(filepath.Join(root, "orders", "lastOrderNumber.json")); err == nil {
				t.Fatal("a number was reserved for a refused order")
			}
		})
	}
}

// The wire keys of dto.SendOrderHeader (what the strict key walk allows) are
// exactly the header fields the writer knows, in the same order.
func TestSendOrderHeader_KeysAreTheWritersFields(t *testing.T) {
	typ := reflect.TypeOf(dto.SendOrderHeader{})
	var keys []string
	for i := 0; i < typ.NumField(); i++ {
		keys = append(keys, typ.Field(i).Tag.Get("json"))
	}
	if got, want := strings.Join(keys, ","), strings.Join(hasavshevet.HeaderFieldNames(), ","); got != want {
		t.Fatalf("dto keys %s; writer fields %s", got, want)
	}
}

// A replay of a historyId that carried a header echoes what this request
// carries, like the first answer; the document stays the first request's.
func TestSendOrderHandler_ReplayEchoesTheHeader(t *testing.T) {
	root := t.TempDir()
	q := newIndexedTestQueue(t, root)
	h := NewSendOrderHandler(q, true)
	body := bodyWithHeader(t, `{"extraText5":"PO-1"}`)
	first := decodeBody(t, sendRawOrder(t, h, body))
	second := decodeBody(t, sendRawOrder(t, h, body))
	if second["replay"] != true || second["jobId"] != first["jobId"] {
		t.Fatalf("second answer %v is not a replay of %v", second, first)
	}
	for _, a := range []map[string]any{first, second} {
		if got, _ := json.Marshal(a["headerApplied"]); string(got) != `["extraText5"]` {
			t.Fatalf("headerApplied = %s; want [\"extraText5\"]", got)
		}
	}
}
