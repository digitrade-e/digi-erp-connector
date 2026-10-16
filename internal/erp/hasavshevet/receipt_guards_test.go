package hasavshevet

// Guards of the number store and the idempotency index that had no failing
// test (DIG-929 gate, round 1). Each was checked to fail under its mutant.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func indexEntryPath(h *receiptHarness, key string) string {
	return filepath.Join(h.dataDir, "receipts", "index", key+".json")
}

// W26: a key's case must not make it a second receipt, and the echo is lower-case.
func TestReceiptWriter_KeyCaseIsOneReceipt(t *testing.T) {
	h := newReceiptHarness(t, 1000)
	w := h.writer()
	lower := "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	r1 := mustWrite(t, w, receiptFor(strings.ToUpper(lower)))
	r2 := mustWrite(t, w, receiptFor(lower))
	if r1.ReceiptNumber != 1000 || r2.ReceiptNumber != 1000 || r1.IdempotencyKey != lower || len(h.docs()) != 1 {
		t.Fatalf("upper=%d/%s lower=%d docs=%v; want one receipt 1000 echoed lower-case", r1.ReceiptNumber, r1.IdempotencyKey, r2.ReceiptNumber, h.docs())
	}
}

// W07: a store that could not move on must not let the next key take the same number.
func TestReceiptWriter_AdvanceFailureNeverReissuesTheNumber(t *testing.T) {
	h := newReceiptHarness(t, 1000)
	w := h.writer()
	rdir := filepath.Join(h.dataDir, "receipts")
	if err := os.MkdirAll(filepath.Join(rdir, "index"), 0o755); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	if err := os.Chmod(rdir, 0o555); err != nil {
		t.Fatal(err)
	}
	_, err := w.Write(context.Background(), receiptFor(testKey(1)))
	if err := os.Chmod(rdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err == nil {
		t.Fatal("Write succeeded with an unwritable store")
	}
	r2 := mustWrite(t, w, receiptFor(testKey(2)))
	if r2.ReceiptNumber == 1000 {
		t.Fatalf("key 2 was given 1000, which key 1 already owns")
	}
}

// I09: an index entry that exists but cannot be read is never a new key.
func TestReceiptIndex_UnreadableEntryIsNotANewKey(t *testing.T) {
	h := newReceiptHarness(t, 1000)
	w := h.writer()
	mustWrite(t, w, receiptFor(testKey(1)))
	p := indexEntryPath(h, testKey(1))
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(p, 0o600) }()
	_, err := w.Write(context.Background(), receiptFor(testKey(1)))
	if !errors.Is(err, ErrReceiptIndexUnreadable) {
		t.Fatalf("Write = %v; want ErrReceiptIndexUnreadable", err)
	}
	if n := h.nextNumber(); n != 1001 || len(h.docs()) != 1 {
		t.Fatalf("next=%d docs=%v; want the key not given a second number", n, h.docs())
	}
}

func plantEntryFrom(t *testing.T, h *receiptHarness, file string, mut func(m map[string]any)) {
	t.Helper()
	b, err := os.ReadFile(indexEntryPath(h, testKey(1)))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	mut(m)
	nb, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexEntryPath(h, file), nb, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReceiptIndex_PlantedEntriesFailClosed(t *testing.T) {
	for name, c := range map[string]struct {
		file string
		mut  func(m map[string]any)
	}{
		"I02 key mismatch":   {testKey(2), func(m map[string]any) { m["number"] = 1500 }},
		"I03 number 0":       {testKey(1), func(m map[string]any) { m["number"] = 0 }},
		"I04 unknown state":  {testKey(1), func(m map[string]any) { m["state"] = "done" }},
		"I05 unknown import": {testKey(1), func(m map[string]any) { m["import"] = "maybe" }},
		"I01 unknown field":  {testKey(1), func(m map[string]any) { m["extra"] = 1 }},
	} {
		t.Run(name, func(t *testing.T) {
			h := newReceiptHarness(t, 1000)
			w := h.writer()
			mustWrite(t, w, receiptFor(testKey(1)))
			plantEntryFrom(t, h, c.file, c.mut)
			_, err := w.Write(context.Background(), receiptFor(c.file))
			if !errors.Is(err, ErrReceiptIndexUnreadable) {
				t.Fatalf("Write = %v; want ErrReceiptIndexUnreadable", err)
			}
		})
	}
}
