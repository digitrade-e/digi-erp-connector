package hasavshevet

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeStoreFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write store: %v", err)
	}
}

// The store is seeded explicitly and fails closed on anything else. The
// existing order-number store restarts at 1 on a missing or corrupt file; a
// receipt store that did that would reissue numbers already printed on legal
// documents.
func TestReceiptNumberStore_RefusesWhatIsNotASeed(t *testing.T) {
	cases := map[string]string{
		"corrupt":             `{`,
		"empty file":          ``,
		"negative":            `{"nextReceiptNumber": -5}`,
		"zero":                `{"nextReceiptNumber": 0}`,
		"fraction":            `{"nextReceiptNumber": 1.5}`,
		"exponent":            `{"nextReceiptNumber": 1e3}`,
		"string":              `{"nextReceiptNumber": "1000"}`,
		"null":                `{"nextReceiptNumber": null}`,
		"field missing":       `{}`,
		"unknown field":       `{"nextReceiptNumber": 1000, "lastOrderNumber": 7}`,
		"trailing data":       `{"nextReceiptNumber": 1000} {}`,
		"order-store format":  `{"lastOrderNumber": 1000}`,
		"past nine digits":    `{"nextReceiptNumber": 1000000000}`,
		"array":               `[1000]`,
		"overflowing integer": `{"nextReceiptNumber": 99999999999999999999}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "receiptNumber.json")
			writeStoreFile(t, path, content)
			s := NewReceiptNumberStore(path)

			n, err := s.Peek()
			if !errors.Is(err, ErrStoreUnavailable) {
				t.Fatalf("Peek() = %d, %v; want ErrStoreUnavailable", n, err)
			}
			if err := s.advancePast(5); !errors.Is(err, ErrStoreUnavailable) {
				t.Fatalf("advancePast on a bad store = %v; want ErrStoreUnavailable", err)
			}
			got, _ := os.ReadFile(path)
			if string(got) != content {
				t.Errorf("the store file was rewritten to %q; a bad store must be left as found", got)
			}
		})
	}
}

func TestReceiptNumberStore_MissingIsUnavailableNeverOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipts", "receiptNumber.json")
	s := NewReceiptNumberStore(path)

	if n, err := s.Peek(); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("Peek() on a missing store = %d, %v; want ErrStoreUnavailable", n, err)
	}
	if err := s.advancePast(1); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("advancePast on a missing store = %v; want ErrStoreUnavailable", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a missing store must stay missing, stat = %v", err)
	}
}

func TestReceiptNumberStore_PeekAndAdvance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receiptNumber.json")
	writeStoreFile(t, path, `{"nextReceiptNumber": 1000}`)
	s := NewReceiptNumberStore(path)

	if n, err := s.Peek(); err != nil || n != 1000 {
		t.Fatalf("Peek() = %d, %v; want 1000", n, err)
	}
	// Peek does not consume.
	if n, _ := s.Peek(); n != 1000 {
		t.Fatalf("second Peek() = %d; want 1000", n)
	}
	if err := s.advancePast(1000); err != nil {
		t.Fatalf("advancePast(1000): %v", err)
	}
	if n, _ := s.Peek(); n != 1001 {
		t.Fatalf("Peek() after advancePast(1000) = %d; want 1001", n)
	}
	// Never lower: advancing past an older number is a no-op.
	if err := s.advancePast(500); err != nil {
		t.Fatalf("advancePast(500): %v", err)
	}
	if n, _ := NewReceiptNumberStore(path).Peek(); n != 1001 {
		t.Fatalf("Peek() from a fresh instance = %d; want 1001 (persisted, never lowered)", n)
	}
	// Recovery may jump forward past a number the index holds.
	if err := s.advancePast(1500); err != nil {
		t.Fatalf("advancePast(1500): %v", err)
	}
	if n, _ := s.Peek(); n != 1501 {
		t.Fatalf("Peek() after advancePast(1500) = %d; want 1501", n)
	}
}

func TestReceiptNumberStore_LastNumberThenExhausted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receiptNumber.json")
	writeStoreFile(t, path, `{"nextReceiptNumber": 999999999}`)
	s := NewReceiptNumberStore(path)

	if n, err := s.Peek(); err != nil || n != maxReceiptNumber {
		t.Fatalf("Peek() = %d, %v; want %d", n, err, maxReceiptNumber)
	}
	if err := s.advancePast(maxReceiptNumber); err != nil {
		t.Fatalf("advancePast(last): %v", err)
	}
	if n, err := s.Peek(); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("Peek() after the last nine-digit number = %d, %v; want ErrStoreUnavailable", n, err)
	}
}

func TestReceiptNumberStore_Seed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipts", "receiptNumber.json")
	s := NewReceiptNumberStore(path)

	for _, bad := range []int64{0, -1, maxReceiptNumber + 1} {
		if err := s.Seed(bad); err == nil {
			t.Errorf("Seed(%d) accepted an out-of-range seed", bad)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a refused seed must not create the store")
	}

	if err := s.Seed(5000); err != nil {
		t.Fatalf("Seed(5000): %v", err)
	}
	if n, err := s.Peek(); err != nil || n != 5000 {
		t.Fatalf("Peek() after Seed(5000) = %d, %v; want 5000", n, err)
	}

	// Written only if absent: a second seed — lower or higher — is refused.
	for _, again := range []int64{1, 5000, 9000} {
		if err := s.Seed(again); err == nil {
			t.Errorf("Seed(%d) over an existing store was accepted", again)
		}
	}
	if n, _ := s.Peek(); n != 5000 {
		t.Fatalf("Peek() after refused re-seeds = %d; want 5000", n)
	}

	// A corrupt store is evidence, not an empty slot: Seed must not paper over it.
	bad := filepath.Join(t.TempDir(), "receiptNumber.json")
	writeStoreFile(t, bad, `{`)
	if err := NewReceiptNumberStore(bad).Seed(10); err == nil {
		t.Error("Seed over a corrupt store was accepted")
	}
}
