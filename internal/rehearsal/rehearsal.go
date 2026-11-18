// Package rehearsal checks what a connector upgrade or downgrade must keep
// (DIG-1304, plan 075 P1-6; the GROOPY rehearsal before BFL's install): the
// order counter, the order index (DIG-1291), the order history, the receipts'
// number store and index, and the format floor. Take reads them into a
// Snapshot — numbers, states and file hashes only, never an order's or a
// receipt's content — and Check names everything that went wrong between
// two snapshots. The CLI is cmd/connector-rehearsal; the procedure is
// docs/order-cutover-proof.md.
//
// It only reads. It never stops the service, copies DataDir\receipts or
// writes a counter: the backup and the install are the operator's steps.
package rehearsal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Entry is one index file: its name, hash, and the number and state it holds.
type Entry struct {
	File       string `json:"file"`
	Sha256     string `json:"sha256"`
	Number     int64  `json:"number"`
	State      string `json:"state"`
	Counter    string `json:"counter,omitempty"` // an order's documentRule counter (plan 087); "" = the default
	Unreadable string `json:"unreadable,omitempty"`
}

// Snapshot is a box's numbering state at one moment.
type Snapshot struct {
	TakenAt      string `json:"takenAt"`
	DataDir      string `json:"dataDir"`
	SendOrderDir string `json:"sendOrderDir"`
	OrderCounter *int64 `json:"orderCounter"` // lastOrderNumber.json: last issued; nil = missing
	CounterError string `json:"counterError,omitempty"`
	// Counters are plan 087's own counter stores, DataDir\counters\<key>.json
	// {"last": N}; nil value = unreadable (CounterErrors says why).
	Counters      map[string]*int64 `json:"counters,omitempty"`
	CounterErrors map[string]string `json:"counterErrors,omitempty"`
	Orders        []Entry           `json:"orders"` // DataDir\orders\index
	HistoryMax    int64             `json:"historyMax"`
	HistoryCount  int               `json:"historyCount"` // sendOrderDir\history\<n>
	ReceiptNext   *int64            `json:"receiptNext"`  // DataDir\receipts\receiptNumber.json; nil = missing
	ReceiptError  string            `json:"receiptError,omitempty"`
	Receipts      []Entry           `json:"receipts"` // DataDir\receipts\index
	FormatFloor   string            `json:"formatFloor"`
}

// Take reads the box. A missing file or folder is a fresh box, not an error.
func Take(dataDir, sendOrderDir string) (Snapshot, error) {
	s := Snapshot{TakenAt: time.Now().UTC().Format(time.RFC3339), DataDir: dataDir, SendOrderDir: sendOrderDir}
	var err error
	s.OrderCounter, s.CounterError = readNumber(filepath.Join(sendOrderDir, "lastOrderNumber.json"), "lastOrderNumber")
	if s.Orders, err = readIndex(filepath.Join(dataDir, "orders", "index")); err != nil {
		return s, err
	}
	if s.Counters, s.CounterErrors, err = readCounters(filepath.Join(dataDir, "counters")); err != nil {
		return s, err
	}
	if s.HistoryMax, s.HistoryCount, err = readHistory(filepath.Join(sendOrderDir, "history")); err != nil {
		return s, err
	}
	s.ReceiptNext, s.ReceiptError = readNumber(filepath.Join(dataDir, "receipts", "receiptNumber.json"), "nextReceiptNumber")
	if s.Receipts, err = readIndex(filepath.Join(dataDir, "receipts", "index")); err != nil {
		return s, err
	}
	if b, err := os.ReadFile(filepath.Join(dataDir, "format-floor")); err == nil {
		s.FormatFloor = strings.TrimSpace(string(b))
	}
	return s, nil
}

func readNumber(path, key string) (*int64, string) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ""
	}
	if err != nil {
		return nil, err.Error()
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Sprintf("%s: not JSON", filepath.Base(path))
	}
	n, err := strconv.ParseInt(string(m[key]), 10, 64)
	if err != nil {
		return nil, fmt.Sprintf("%s: %s is %s", filepath.Base(path), key, m[key])
	}
	return &n, ""
}

func readIndex(dir string) ([]Entry, error) {
	files, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, f.Name()))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		e := Entry{File: f.Name(), Sha256: hex.EncodeToString(sum[:])}
		var v struct {
			Number int64  `json:"number"`
			State  string `json:"state"`
			Rule   *struct {
				Counter string `json:"counter"`
			} `json:"rule"`
		}
		if err := json.Unmarshal(b, &v); err != nil || v.Number < 1 {
			e.Unreadable = "not an index entry"
		} else {
			e.Number, e.State = v.Number, v.State
			if v.Rule != nil {
				e.Counter = v.Rule.Counter
			}
		}
		out = append(out, e)
	}
	return out, nil
}

func readCounters(dir string) (map[string]*int64, map[string]string, error) {
	files, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	counters, errs := map[string]*int64{}, map[string]string{}
	for _, f := range files {
		key, ok := strings.CutSuffix(f.Name(), ".json")
		if f.IsDir() || !ok {
			continue
		}
		n, why := readNumber(filepath.Join(dir, f.Name()), "last")
		counters[key] = n
		if why != "" {
			errs[key] = why
		} else if n == nil {
			errs[key] = "missing"
		}
	}
	if len(errs) == 0 {
		errs = nil
	}
	return counters, errs, nil
}

func readHistory(dir string) (highest int64, count int, err error) {
	files, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	for _, f := range files {
		n, err := strconv.ParseInt(f.Name(), 10, 64)
		if !f.IsDir() || err != nil || n < 1 {
			continue
		}
		count++
		highest = max(highest, n)
	}
	return highest, count, nil
}

// Check names everything between before and after that an upgrade or a
// downgrade must never do. No finding = the box kept its numbering.
func Check(before, after Snapshot) []string {
	var f []string
	add := func(format string, a ...any) { f = append(f, fmt.Sprintf(format, a...)) }

	// The order counter: present, never backwards, never below a number issued.
	switch {
	case after.CounterError != "":
		add("order counter unreadable: %s", after.CounterError)
	case before.OrderCounter != nil && after.OrderCounter == nil:
		add("order counter lastOrderNumber.json is missing (was %d): the next order would restart at 1", *before.OrderCounter)
	case before.OrderCounter != nil && *after.OrderCounter < *before.OrderCounter:
		add("order counter went backwards: %d → %d", *before.OrderCounter, *after.OrderCounter)
	}
	// With plan 087's counters, numbers of other ranges share the index and
	// sendOrderDir\history: lastOrderNumber.json is held only against the
	// orders without a rule, and the history only when there is one range.
	ruled := len(after.Counters) > 0
	for _, e := range after.Orders {
		ruled = ruled || e.Counter != ""
	}
	if after.OrderCounter != nil {
		var highest int64
		if !ruled {
			highest = after.HistoryMax
		}
		for _, e := range after.Orders {
			// A rule naming a counter with no store of its own names the
			// adopted one: lastOrderNumber.json (plan 087).
			if _, own := after.Counters[e.Counter]; e.Counter == "" || !own {
				highest = max(highest, e.Number)
			}
		}
		if *after.OrderCounter < highest {
			add("order counter %d is below the highest issued number %d (index or history): a number could be issued twice", *after.OrderCounter, highest)
		}
	}
	for key, was := range before.Counters {
		now, ok := after.Counters[key]
		switch {
		case !ok:
			add("counter %s (DataDir\\counters\\%s.json) is missing", key, key)
		case after.CounterErrors[key] != "":
			add("counter %s unreadable: %s", key, after.CounterErrors[key])
		case was != nil && now != nil && *now < *was:
			add("counter %s went backwards: %d → %d", key, *was, *now)
		}
	}
	for key, now := range after.Counters {
		if now == nil {
			continue
		}
		for _, e := range after.Orders {
			if e.Counter == key && e.Number > *now {
				add("counter %s is at %d, below order %d it issued (%s)", key, *now, e.Number, e.File)
			}
		}
	}

	f = append(f, checkIndex("order", before.Orders, after.Orders)...)
	f = append(f, checkIndex("receipt", before.Receipts, after.Receipts)...)

	if after.HistoryCount < before.HistoryCount {
		add("order history lost folders: %d → %d", before.HistoryCount, after.HistoryCount)
	}

	switch {
	case after.ReceiptError != "":
		add("receipt number store unreadable: %s", after.ReceiptError)
	case before.ReceiptNext != nil && after.ReceiptNext == nil:
		add("receipt number store receiptNumber.json is missing (next was %d)", *before.ReceiptNext)
	case before.ReceiptNext != nil && *after.ReceiptNext < *before.ReceiptNext:
		add("receipt number store went backwards: next %d → %d", *before.ReceiptNext, *after.ReceiptNext)
	}

	if before.FormatFloor != "" && lowerRelease(after.FormatFloor, before.FormatFloor) {
		add("format-floor went down: %q → %q (it is never lowered)", before.FormatFloor, after.FormatFloor)
	}
	return f
}

// checkIndex: every entry kept, under the same number; no number twice; none
// unreadable that was readable.
func checkIndex(kind string, before, after []Entry) []string {
	var f []string
	now := map[string]Entry{}
	for _, e := range after {
		now[e.File] = e
	}
	for _, b := range before {
		a, ok := now[b.File]
		switch {
		case !ok:
			f = append(f, fmt.Sprintf("%s index entry %s (number %d) lost", kind, b.File, b.Number))
		case a.Unreadable != "" && b.Unreadable == "":
			f = append(f, fmt.Sprintf("%s index entry %s (number %d) became unreadable", kind, b.File, b.Number))
		case b.Unreadable == "" && a.Number != b.Number:
			f = append(f, fmt.Sprintf("%s index entry %s changed its number %d → %d", kind, b.File, b.Number, a.Number))
		}
	}
	by := map[int64][]string{}
	for _, e := range after {
		if e.Unreadable == "" {
			by[e.Number] = append(by[e.Number], e.File)
		}
	}
	var twice []int64
	for n, files := range by {
		if len(files) > 1 {
			twice = append(twice, n)
		}
	}
	sort.Slice(twice, func(i, j int) bool { return twice[i] < twice[j] })
	for _, n := range twice {
		files := by[n]
		sort.Strings(files)
		f = append(f, fmt.Sprintf("%s number %d issued twice: %v", kind, n, files))
	}
	return f
}

// Summary is what moved, for the log of the rehearsal.
func Summary(before, after Snapshot) string {
	was := map[string]Entry{}
	for _, e := range before.Orders {
		was[e.File] = e
	}
	added, changed := 0, 0
	for _, e := range after.Orders {
		b, ok := was[e.File]
		switch {
		case !ok:
			added++
		case b.Sha256 != e.Sha256:
			changed++
		}
	}
	num := func(p *int64) string {
		if p == nil {
			return "missing"
		}
		return strconv.FormatInt(*p, 10)
	}
	return fmt.Sprintf("order counter %s → %s; order index entries new: %d, changed: %d; history %d → %d folders; receipt next %s → %s, %d → %d index entries; format-floor %q → %q",
		num(before.OrderCounter), num(after.OrderCounter), added, changed, before.HistoryCount, after.HistoryCount,
		num(before.ReceiptNext), num(after.ReceiptNext), len(before.Receipts), len(after.Receipts), before.FormatFloor, after.FormatFloor)
}

// lowerRelease: a is a lower release than b ("1.0.29" < "1.0.30"); an
// unreadable a counts as lower (fail closed).
func lowerRelease(a, b string) bool {
	pa, okA := parseRelease(a)
	pb, okB := parseRelease(b)
	if !okB {
		return false
	}
	if !okA {
		return true
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func parseRelease(s string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(s), "v"), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
