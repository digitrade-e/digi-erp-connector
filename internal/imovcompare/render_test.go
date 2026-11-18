package imovcompare

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

const goldenAccount = `{"accountKey":"40217","fullName":"לקוח 'בדיקה' בע\"מ","address":"הרצל 1\nקומה 2","city":"חיפה","phone":"04-1234567","agent":"7","hProtect":"514000000"}`

// The wire bodies of DIG-1297's "order" and "return" cases: what erp-manager
// posts for them. Through the real route and writer they must give the legacy
// writer's own files (testdata/imovein-legacy).
func goldenBody(t *testing.T, kind string, withAccount bool) []byte {
	t.Helper()
	body := map[string]any{
		"documentType": "ORDER", "userExtId": "40217", "dueDate": "2026-11-10", "createdDate": "2026-11-05T09:30:00Z",
		"comment": "הערה ללקוח\nשורה שנייה|הערת מנהל", "discount": 5, "historyId": "4711-0b6c", "total": 165,
		"currency": `ש"ח`, "documentId": 31, "warehouse": 81, "vatPercent": "18.00", "asmahta2": "4711",
		"details": []map[string]any{
			{"title": "מוצר א'", "sku": "1001", "quantity": 2, "packs": 1, "originalPrice": 75, "singlePrice": 70.13, "totalPrice": 140.25, "discount": 6.5},
			{"title": "Widget B – 5₪", "sku": "SKU-2002", "quantity": 1.5, "originalPrice": 10, "singlePrice": 10, "totalPrice": 15, "discount": 0},
		},
	}
	if kind == "return" {
		body["documentType"], body["documentId"], body["warehouse"], body["asmahta2"] = "RETURN", 74, 8, "4713"
		body["discount"], body["comment"], body["historyId"] = 0, "החזרה", "4713-0b6c"
		body["details"] = []map[string]any{{"title": "החזרה", "sku": "1001", "quantity": -1, "originalPrice": 75, "singlePrice": 75, "totalPrice": -75, "discount": 0}}
	}
	if withAccount {
		body["account"] = json.RawMessage(goldenAccount)
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRender_TheRealRouteGivesTheLegacyFiles(t *testing.T) {
	for _, c := range []struct {
		kind, golden, file string
		number             int64
	}{
		{"order", "order.doc", "IMOVEIN_100001.doc", 100001},
		{"return", "return.doc", "TMP_IMOVEIN_100003.doc", 100003},
	} {
		t.Run(c.kind, func(t *testing.T) {
			got, err := Render(context.Background(), goldenBody(t, c.kind, true), c.number, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got.FileName != c.file {
				t.Fatalf("file = %q, want %q", got.FileName, c.file)
			}
			if r := Compare(legacyGolden(t, c.golden), got.Doc, nil); !r.Identical {
				t.Fatalf("not the legacy bytes:\n%s", r)
			}
		})
	}
}

// A body without "account" takes the one given (offline there is no
// Hashavshevet Accounts table to read); a body with one keeps its own.
func TestRender_AccountOnlyWhenTheBodyHasNone(t *testing.T) {
	acc := json.RawMessage(goldenAccount)
	got, err := Render(context.Background(), goldenBody(t, "order", false), 100001, acc)
	if err != nil {
		t.Fatal(err)
	}
	if !Compare(legacyGolden(t, "order.doc"), got.Doc, nil).Identical {
		t.Fatal("the given account was not used")
	}
	other := json.RawMessage(strings.Replace(goldenAccount, "חיפה", "עכו", 1))
	got, err = Render(context.Background(), goldenBody(t, "order", true), 100001, other)
	if err != nil {
		t.Fatal(err)
	}
	if !Compare(legacyGolden(t, "order.doc"), got.Doc, nil).Identical {
		t.Fatal("the body's own account was replaced")
	}
}

func TestRender_Refusals(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		body   []byte
		number int64
		want   string
	}{
		{"no account and no database", goldenBody(t, "order", false), 100001, "account"},
		{"route refuses the body", []byte(`{"documentType":"ORDER"}`), 100001, "400"},
		{"not JSON", []byte(`nope`), 100001, "JSON"},
		{"number zero", goldenBody(t, "order", true), 0, "number"},
		{"number too big", goldenBody(t, "order", true), 1_000_000_000, "number"},
		{"legacy widths", bytes.Replace(goldenBody(t, "order", true), []byte(`"vatPercent":"18.00"`), []byte(`"vatPercent":"18.25"`), 1), 100001, "400"}, // five characters in a four-wide field
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Render(ctx, c.body, c.number, nil)
			t.Logf("err = %v", err)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one with %q", err, c.want)
			}
		})
	}
}
