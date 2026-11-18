package rehearsal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// box is a connector's files as a test lays them out: DataDir and sendOrderDir.
type box struct{ data, send string }

func newBox(t *testing.T) box {
	t.Helper()
	b := box{data: t.TempDir(), send: t.TempDir()}
	b.counter(t, 100)
	b.order(t, "a", 99, "written")
	b.order(t, "b", 100, "written")
	b.history(t, 99)
	b.history(t, 100)
	b.write(t, filepath.Join(b.data, "receipts", "receiptNumber.json"), `{"nextReceiptNumber": 501, "seededAt": "2026-10-21T00:00:00Z"}`)
	b.write(t, filepath.Join(b.data, "receipts", "index", "k1.json"), `{"key":"k1","number":500,"state":"written","request":{"customer":"SECRET-NAME"}}`)
	b.write(t, filepath.Join(b.data, "format-floor"), "1.0.30\n")
	return b
}

func (b box) write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}
func (b box) counter(t *testing.T, n int) {
	b.write(t, filepath.Join(b.send, "lastOrderNumber.json"), `{"lastOrderNumber": `+itoa(n)+`}`)
}
func (b box) order(t *testing.T, stem string, n int, state string) {
	b.write(t, filepath.Join(b.data, "orders", "index", stem+".json"), `{"key":"`+stem+`","number":`+itoa(n)+`,"state":"`+state+`","reservedAt":"x"}`)
}
func (b box) history(t *testing.T, n int) {
	b.write(t, filepath.Join(b.send, "history", itoa(n), "IMOVEIN_"+itoa(n)+".doc"), "doc")
}
func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func take(t *testing.T, b box) Snapshot {
	t.Helper()
	s, err := Take(b.data, b.send)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTake_ReadsTheBoxAndNoContent(t *testing.T) {
	b := newBox(t)
	s := take(t, b)
	if s.OrderCounter == nil || *s.OrderCounter != 100 || len(s.Orders) != 2 || s.HistoryMax != 100 || s.HistoryCount != 2 {
		t.Fatalf("snapshot = %+v", s)
	}
	if s.ReceiptNext == nil || *s.ReceiptNext != 501 || len(s.Receipts) != 1 || s.FormatFloor != "1.0.30" {
		t.Fatalf("snapshot = %+v", s)
	}
	j, _ := json.Marshal(s)
	if strings.Contains(string(j), "SECRET-NAME") {
		t.Fatal("the snapshot carries a receipt's request")
	}
	if len(Check(s, s)) != 0 {
		t.Fatalf("a snapshot against itself: %v", Check(s, s))
	}
}

// What the rehearsal must catch, each named.
func TestCheck_Findings(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(t *testing.T, b box)
		want   string
	}{
		{"counter went back", func(t *testing.T, b box) { b.counter(t, 99) }, "went backwards"},
		{"counter missing", func(t *testing.T, b box) { os.Remove(filepath.Join(b.send, "lastOrderNumber.json")) }, "lastOrderNumber.json"},
		{"counter below an issued number", func(t *testing.T, b box) { b.order(t, "c", 101, "written"); b.history(t, 101) }, "below"},
		{"index entry lost", func(t *testing.T, b box) { os.Remove(filepath.Join(b.data, "orders", "index", "a.json")) }, "lost"},
		{"index entry renumbered", func(t *testing.T, b box) { b.order(t, "a", 98, "written") }, "number"},
		{"number issued twice", func(t *testing.T, b box) { b.counter(t, 101); b.order(t, "c", 100, "written") }, "twice"},
		{"receipt store went back", func(t *testing.T, b box) {
			b.write(t, filepath.Join(b.data, "receipts", "receiptNumber.json"), `{"nextReceiptNumber": 500}`)
		}, "receipt"},
		{"receipt index lost", func(t *testing.T, b box) { os.Remove(filepath.Join(b.data, "receipts", "index", "k1.json")) }, "lost"},
		{"format floor lowered", func(t *testing.T, b box) { b.write(t, filepath.Join(b.data, "format-floor"), "1.0.29\n") }, "format-floor"},
		{"history folder lost", func(t *testing.T, b box) { os.RemoveAll(filepath.Join(b.send, "history", "99")) }, "history lost"},
		{"unreadable entry", func(t *testing.T, b box) { b.write(t, filepath.Join(b.data, "orders", "index", "a.json"), "{") }, "unreadable"},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newBox(t)
			before := take(t, b)
			c.change(t, b)
			after := take(t, b)
			f := strings.Join(Check(before, after), "\n")
			if !strings.Contains(f, c.want) {
				t.Fatalf("findings %q lack %q", f, c.want)
			}
		})
	}
}

// Plan 087: a test counter's orders (rule.counter) live in another range; the
// production counter is not "below" them. The named counters are checked on
// their own.
func TestCheck_NamedCounters(t *testing.T) {
	setup := func(t *testing.T) box {
		b := newBox(t)
		b.write(t, filepath.Join(b.data, "counters", "test.json"), `{"last": 9000001}`)
		b.write(t, filepath.Join(b.data, "orders", "index", "t.json"), `{"key":"t","number":9000001,"state":"written","rule":{"counter":"test"}}`)
		b.history(t, 9000001)
		return b
	}
	b := setup(t)
	s := take(t, b)
	if s.Counters["test"] == nil || *s.Counters["test"] != 9000001 {
		t.Fatalf("counters = %+v", s.Counters)
	}
	if f := Check(s, s); len(f) != 0 {
		t.Fatalf("findings on a healthy two-range box: %v", f)
	}
	// A configured counter whose history folder has no index entry (e.g. the
	// entry was moved aside): the production counter is still not "below" it.
	os.Remove(filepath.Join(b.data, "orders", "index", "t.json"))
	if s := take(t, b); len(Check(s, s)) != 0 {
		t.Fatalf("findings with a counter's history and no ruled entry: %v", Check(s, s))
	}
	for _, c := range []struct {
		name   string
		change func(t *testing.T, b box)
		want   string
	}{
		{"backwards", func(t *testing.T, b box) {
			b.write(t, filepath.Join(b.data, "counters", "test.json"), `{"last": 9000000}`)
		}, "counter test went backwards"},
		{"missing", func(t *testing.T, b box) { os.Remove(filepath.Join(b.data, "counters", "test.json")) }, "counter test (DataDir"},
		{"unreadable", func(t *testing.T, b box) { b.write(t, filepath.Join(b.data, "counters", "test.json"), `{`) }, "counter test unreadable"},
		{"below its order", func(t *testing.T, b box) {
			b.write(t, filepath.Join(b.data, "orders", "index", "u.json"), `{"key":"u","number":9000002,"state":"written","rule":{"counter":"test"}}`)
		}, "below order 9000002"},
		{"default below a rule-less order", func(t *testing.T, b box) { b.order(t, "c", 101, "written") }, "below the highest issued number 101"},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := setup(t)
			before := take(t, b)
			c.change(t, b)
			if f := strings.Join(Check(before, take(t, b)), "\n"); !strings.Contains(f, c.want) {
				t.Fatalf("findings %q lack %q", f, c.want)
			}
		})
	}
}

// What a healthy rehearsal does: a test order sent between snapshots, an
// entry's state moving on, the floor raised. None of it is a finding.
func TestCheck_HealthyChanges(t *testing.T) {
	b := newBox(t)
	before := take(t, b)
	b.counter(t, 101)
	b.order(t, "c", 101, "written")
	b.history(t, 101)
	b.order(t, "b", 100, "failed")
	b.write(t, filepath.Join(b.data, "format-floor"), "1.0.32\n")
	after := take(t, b)
	if f := Check(before, after); len(f) != 0 {
		t.Fatalf("findings on a healthy change: %v", f)
	}
	if d := Summary(before, after); !strings.Contains(d, "new: 1, changed: 1") {
		t.Fatalf("summary %q", d)
	}
}

// A first install (no counter, no index) is a valid "before".
func TestTake_EmptyBox(t *testing.T) {
	s, err := Take(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if s.OrderCounter != nil || len(s.Orders) != 0 || s.ReceiptNext != nil {
		t.Fatalf("snapshot = %+v", s)
	}
}
