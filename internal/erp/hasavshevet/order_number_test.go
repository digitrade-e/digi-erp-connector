package hasavshevet

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestOrderNumberStore_Sequential verifies that sequential calls produce incrementing numbers.
func TestOrderNumberStore_Sequential(t *testing.T) {
	dir := t.TempDir()
	store := NewOrderNumberStore(filepath.Join(dir, "lastOrderNumber.json"))

	for i := int64(1); i <= 5; i++ {
		n, err := store.Next()
		if err != nil {
			t.Fatalf("Next() error: %v", err)
		}
		if n != i {
			t.Errorf("Next() = %d, want %d", n, i)
		}
	}
}

// TestOrderNumberStore_Persistence verifies numbers survive creating a new store instance.
func TestOrderNumberStore_Persistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lastOrderNumber.json")

	s1 := NewOrderNumberStore(path)
	n, err := s1.Next()
	if err != nil {
		t.Fatalf("s1.Next() error: %v", err)
	}
	if n != 1 {
		t.Fatalf("first Next() = %d, want 1", n)
	}

	// New store instance reads from same file
	s2 := NewOrderNumberStore(path)
	n2, err := s2.Next()
	if err != nil {
		t.Fatalf("s2.Next() error: %v", err)
	}
	if n2 != 2 {
		t.Errorf("second Next() after reload = %d, want 2", n2)
	}
}

// TestOrderNumberStore_ExistingFile verifies continuation from a pre-existing JSON file
// (e.g. migrated from the legacy Node app config/lastOrderNumber.json).
func TestOrderNumberStore_ExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lastOrderNumber.json")

	// Write legacy-format seed file
	seed := orderNumberFile{LastOrderNumber: 1000294}
	b, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	store := NewOrderNumberStore(path)
	n, err := store.Next()
	if err != nil {
		t.Fatalf("Next() error: %v", err)
	}
	if n != 1000295 {
		t.Errorf("Next() = %d, want 1000295", n)
	}
}

// TestOrderNumberStore_Concurrent verifies that concurrent calls produce unique, sequential numbers.
func TestOrderNumberStore_Concurrent(t *testing.T) {
	dir := t.TempDir()
	store := NewOrderNumberStore(filepath.Join(dir, "lastOrderNumber.json"))

	const goroutines = 20
	results := make([]int64, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		idx := i
		go func() {
			defer wg.Done()
			n, err := store.Next()
			if err != nil {
				t.Errorf("goroutine %d: Next() error: %v", idx, err)
				return
			}
			results[idx] = n
		}()
	}
	wg.Wait()

	// All numbers must be in [1..goroutines] with no duplicates
	seen := make(map[int64]bool, goroutines)
	for i, n := range results {
		if n < 1 || n > goroutines {
			t.Errorf("results[%d] = %d out of expected range [1..%d]", i, n, goroutines)
		}
		if seen[n] {
			t.Errorf("duplicate order number %d", n)
		}
		seen[n] = true
	}
}

// Taking an order number for the key index is two steps — Peek, persist the
// index entry that owns the number, advancePast — so a crash between them
// leaves the entry ahead of the counter, never a number owned by nobody.
func TestOrderNumberStore_PeekAndAdvancePast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lastOrderNumber.json")
	store := NewOrderNumberStore(path)

	// A missing counter is a fresh install: the first order is 1.
	for i := 0; i < 2; i++ {
		if n, err := store.Peek(); err != nil || n != 1 {
			t.Fatalf("Peek on a missing counter = %d, %v; want 1 and no file written", n, err)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Peek wrote the counter: %v", err)
	}

	if err := store.advancePast(1); err != nil {
		t.Fatal(err)
	}
	if n, _ := store.Peek(); n != 2 {
		t.Fatalf("Peek after advancePast(1) = %d; want 2", n)
	}
	// Never lowers: advancing past a number already behind it is a no-op.
	before := readFile(t, path)
	if err := store.advancePast(1); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, readFile(t, path)) {
		t.Error("advancePast of a number already behind the counter rewrote it")
	}
	if err := store.advancePast(41); err != nil {
		t.Fatal(err)
	}
	if n, _ := store.Next(); n != 42 {
		t.Fatalf("Next after advancePast(41) = %d; want 42", n)
	}
	// The format stays the legacy one.
	var f orderNumberFile
	if err := json.Unmarshal(readFile(t, path), &f); err != nil || f.LastOrderNumber != 42 {
		t.Fatalf("counter file = %s (%v); want {\"lastOrderNumber\": 42}", readFile(t, path), err)
	}
	// Written by atomic rename: no temp file is left beside it.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("the counter's folder holds %d entries; want only lastOrderNumber.json", len(entries))
	}
}

// A counter that exists but cannot be read used to restart at 1 silently,
// handing Hashavshevet order numbers it already has. Now every use refuses and
// the file is left exactly as found; only a MISSING file starts at 1.
func TestOrderNumberStore_UnreadableCounterRefuses(t *testing.T) {
	for name, content := range map[string]string{
		"not json":          `{`,
		"empty":             ``,
		"no number":         `{}`,
		"null":              `{"lastOrderNumber": null}`,
		"string":            `{"lastOrderNumber": "12"}`,
		"fraction":          `{"lastOrderNumber": 1.5}`,
		"negative":          `{"lastOrderNumber": -3}`,
		"data after object": `{"lastOrderNumber": 3} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "lastOrderNumber.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			store := NewOrderNumberStore(path)
			if n, err := store.Peek(); !errors.Is(err, ErrOrderCounterUnreadable) {
				t.Errorf("Peek = %d, %v; want ErrOrderCounterUnreadable", n, err)
			}
			if err := store.advancePast(5); !errors.Is(err, ErrOrderCounterUnreadable) {
				t.Errorf("advancePast = %v; want ErrOrderCounterUnreadable", err)
			}
			if n, err := store.Next(); !errors.Is(err, ErrOrderCounterUnreadable) {
				t.Errorf("Next = %d, %v; want ErrOrderCounterUnreadable", n, err)
			}
			if got := string(readFile(t, path)); got != content {
				t.Errorf("the counter was rewritten to %q", got)
			}
		})
	}
}

// The legacy Node app's file may carry other fields; they are ignored, as
// before, and the number is continued.
func TestOrderNumberStore_LegacyFileWithOtherFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lastOrderNumber.json")
	if err := os.WriteFile(path, []byte(`{"lastOrderNumber": 1000294, "updatedAt": "2026-01-01"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := NewOrderNumberStore(path).Next(); err != nil || n != 1000295 {
		t.Fatalf("Next = %d, %v; want 1000295", n, err)
	}
}
