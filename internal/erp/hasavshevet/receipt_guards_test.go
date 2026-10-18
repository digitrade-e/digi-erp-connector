package hasavshevet

// Guards of the number store and the idempotency index that had no failing
// test (DIG-929 gate, round 1). Each was checked to fail under its mutant.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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
	skipOnWindowsModeBits(t)
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
	skipOnWindowsModeBits(t)
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
		"I03 number past F3": {testKey(1), func(m map[string]any) { m["number"] = maxReceiptNumber + 1 }},
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

// The store never persists a next number outside 1 … 999 999 999 + 1 (the
// last value meaning "F3 exhausted"), whoever asks: an index entry planted
// with number 1 000 000 000 once made recovery store 1 000 000 001.
func TestReceiptNumberStore_NeverPersistsANumberPastF3(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receiptNumber.json")
	s := NewReceiptNumberStore(path)
	if err := s.Seed(5); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int64{maxReceiptNumber + 1, maxReceiptNumber + 2, 1 << 40} {
		if err := s.advancePast(n); err == nil {
			t.Errorf("advancePast(%d) accepted", n)
		}
		if got, _ := os.ReadFile(path); !bytes.Equal(got, before) {
			t.Fatalf("advancePast(%d) rewrote the store to %s", n, got)
		}
	}
	if err := s.write(0, ""); err == nil {
		t.Error("write(0) accepted")
	}
	// The last number is issuable; after it the store says exhausted.
	if err := s.advancePast(maxReceiptNumber); err != nil {
		t.Fatalf("advancePast(max) = %v", err)
	}
	if _, err := s.Peek(); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("Peek after the last number = %v; want ErrStoreUnavailable (exhausted)", err)
	}
}

// skipOnWindowsModeBits skips a guard test that makes a file or folder
// unwritable with chmod: on Windows os.Chmod only toggles the read-only
// attribute, which a directory ignores and which does not stop reads. The
// guards themselves are platform-independent and run on Linux.
func skipOnWindowsModeBits(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("chmod cannot make a path unwritable/unreadable on Windows")
	}
}
