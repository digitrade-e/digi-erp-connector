package hasavshevet

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

type fakePrecheck struct {
	codes    map[int]bool
	accounts map[string]bool
	items    map[string]bool
	err      error
	calls    int
}

func (f *fakePrecheck) documentKnown(_ context.Context, code int) (bool, error) {
	f.calls++
	return f.codes[code], f.err
}

func (f *fakePrecheck) accountKnown(_ context.Context, key string) (bool, error) {
	f.calls++
	return f.accounts[key], f.err
}

func (f *fakePrecheck) missingItems(_ context.Context, keys []string) ([]string, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	var out []string
	for _, k := range keys {
		if !f.items[strings.ToUpper(k)] {
			out = append(out, k)
		}
	}
	return out, nil
}

func knownBox() *fakePrecheck {
	return &fakePrecheck{codes: map[int]bool{32: true, 31: true}, accounts: map[string]bool{"1234": true}, items: map[string]bool{"1001": true, "1002": true}}
}

func TestPrecheckOrder(t *testing.T) {
	base := drainTestOrder(1) // ORDER, currency ILS → code 32; account 1234; item 1001
	cases := []struct {
		name  string
		edit  func(*OrderRequest)
		box   func(*fakePrecheck)
		field string // "" = accepted
	}{
		{"all known", nil, nil, ""},
		{"document code unknown", nil, func(f *fakePrecheck) { delete(f.codes, 32) }, "documentType"},
		{"documentId unknown", func(r *OrderRequest) { r.DocumentID = 77 }, nil, "documentId"},
		{"account unknown", nil, func(f *fakePrecheck) { delete(f.accounts, "1234") }, "account/accountKey"},
		{"userExtId unknown", func(r *OrderRequest) { r.Account = nil; r.UserExtID = "9" }, nil, "userExtId"},
		{"second item unknown", func(r *OrderRequest) {
			r.Details = append(r.Details, OrderLineItem{SKU: "1002", Quantity: 1}, OrderLineItem{SKU: "X9", Quantity: 1})
		}, nil, "details[2].sku"},
		{"comment lines are not items", func(r *OrderRequest) { r.Details = append(r.Details, OrderLineItem{SKU: "*"}) }, nil, ""},
		// The database cannot be read: the order goes through (logged).
		{"database unreadable", nil, func(f *fakePrecheck) { f.err = errors.New("timeout") }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := base
			req.Details = append([]OrderLineItem(nil), base.Details...)
			acc := *base.Account
			req.Account = &acc
			if tc.edit != nil {
				tc.edit(&req)
			}
			box := knownBox()
			if tc.box != nil {
				tc.box(box)
			}
			s := NewSender(nil, config.Config{}, nil, noopLogger{})
			s.precheck = box
			err := s.precheckOrder(req)
			var fe *FieldError
			switch {
			case tc.field == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.field != "" && (!errors.As(err, &fe) || fe.Field != tc.field):
				t.Fatalf("err = %v; want a FieldError on %s", err, tc.field)
			}
			if fe != nil && (strings.Contains(fe.Reason, "X9") || strings.Contains(fe.Reason, "1234")) {
				t.Errorf("reason %q echoes a value", fe.Reason)
			}
		})
	}
}

// The pre-check refuses a new order before a number is used, and never
// touches a repeat of a known historyId.
func TestOrderQueue_PrecheckOnlyForNewOrders(t *testing.T) {
	h := newOrderHarness(t)
	q := h.queue(true)
	box := knownBox()
	q.sender.precheck = box
	q.sender.cfg.HasOrderPrecheck = true

	acc := mustAccept(t, q, keyedOrder("K-1"))
	waitForStatus(t, q, acc.JobID, JobStatusDone)

	// The item disappears from Hashavshevet: a NEW order is refused, no number used…
	delete(box.items, "1001")
	if _, err := q.Accept(keyedOrder("K-2")); err == nil {
		t.Fatal("a new order with an unknown item was accepted")
	} else {
		var fe *FieldError
		if !errors.As(err, &fe) || fe.Field != "details[0].sku" {
			t.Fatalf("err = %v; want a FieldError on details[0].sku", err)
		}
	}
	// …and a repeat of the first is answered from the index, unchecked.
	calls := box.calls
	rep, err := q.Accept(keyedOrder("K-1"))
	if err != nil || !rep.Replay || rep.JobID != acc.JobID {
		t.Fatalf("repeat = %+v, %v; want the first order's replay", rep, err)
	}
	if box.calls != calls {
		t.Errorf("the repeat was pre-checked (%d calls)", box.calls-calls)
	}
	// The refused order used no number: the next accepted one follows the first.
	box.items["1001"] = true
	next := mustAccept(t, q, keyedOrder("K-3"))
	first, _ := strconv.ParseInt(acc.JobID, 10, 64)
	if got, _ := strconv.ParseInt(next.JobID, 10, 64); got != first+1 {
		t.Fatalf("next order number %s; want %d (a refused order uses no number)", next.JobID, first+1)
	}
}
