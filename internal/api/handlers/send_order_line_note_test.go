package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/text/encoding/charmap"
)

// DIG-1551: the optional "note" on each line of POST /api/sendOrder.

// line71Span is IMOVEIN line71's byte span (0-based, end exclusive) in record 0.
var line71Span = [2]int{2004, 2024}

func bodyWithLineNote(t *testing.T, note any) string {
	t.Helper()
	body := validOrderBody()
	body["account"] = map[string]any{"accountKey": "CUST001", "fullName": "לקוח בדיקה"}
	line := body["details"].([]any)[0].(map[string]any)
	line["note"] = note
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSendOrderHandler_LineNoteIsWrittenToLine71(t *testing.T) {
	_, doc := postAndWrite(t, bodyWithLineNote(t, "לעטוף כמתנה"))
	b, err := charmap.Windows1255.NewDecoder().Bytes(doc[line71Span[0]:line71Span[1]])
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimRight(string(b), " "); got != "לעטוף כמתנה" {
		t.Fatalf("line71 = %q; want the line's note", got)
	}
}

func TestSendOrderHandler_NoLineNoteIsTodaysOrder(t *testing.T) {
	_, base := postAndWrite(t, bodyWithHeader(t, ""))
	_, empty := postAndWrite(t, bodyWithLineNote(t, ""))
	if string(base) != string(empty) {
		t.Fatal(`"note": "" changed the file`)
	}
}

// One character too many is a 400 naming the line, before any number is given.
func TestSendOrderHandler_OverlongLineNoteIs400(t *testing.T) {
	q := startedIndexedQueue(t, t.TempDir())
	w := sendRawOrder(t, NewSendOrderHandler(q, true), bodyWithLineNote(t, strings.Repeat("א", 21)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST = %d %s; want 400", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.Body.String(), "VALIDATION_ERROR") || !strings.Contains(w.Body.String(), `"details[0]/note"`) {
		t.Fatalf("answer %s; want VALIDATION_ERROR with field details[0]/note", w.Body.String())
	}
	if p := q.Pending(); p != 0 {
		t.Fatalf("%d orders queued after a refusal", p)
	}
}
