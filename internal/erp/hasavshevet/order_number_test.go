package hasavshevet

import (
	"encoding/json"
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
