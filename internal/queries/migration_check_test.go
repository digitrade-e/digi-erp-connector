package queries

import (
	"os"
	"testing"
)

// Temporary check: validates a real migrated queries.json parses correctly.
// Run: MIGRATED_QUERIES_PATH=<path> go test -run TestMigratedFileParses ./internal/queries
func TestMigratedFileParses(t *testing.T) {
	path := os.Getenv("MIGRATED_QUERIES_PATH")
	if path == "" {
		t.Skip("MIGRATED_QUERIES_PATH not set")
	}

	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("failed to parse migrated file: %v", err)
	}
	t.Logf("parsed %d queries", s.Count())
	for _, q := range s.List() {
		if err := ValidateName(q.Name); err != nil {
			t.Errorf("query %q: invalid name: %v", q.Name, err)
		}
		if q.SQL == "" {
			t.Errorf("query %q: empty sql", q.Name)
		}
	}
}
