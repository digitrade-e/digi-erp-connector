package hasavshevet

import (
	"encoding/json"
	"os"
	"sync"
)

// OrderNumberStore is a concurrency-safe, file-backed order number counter.
// The JSON file format {"lastOrderNumber": N} matches the legacy Node app
// (config/lastOrderNumber.json) so existing sequences can be continued.
type OrderNumberStore struct {
	mu   sync.Mutex
	path string
}

type orderNumberFile struct {
	LastOrderNumber int64 `json:"lastOrderNumber"`
}

// NewOrderNumberStore creates a store backed by the file at path.
// The file is created on first use if it does not exist.
func NewOrderNumberStore(path string) *OrderNumberStore {
	return &OrderNumberStore{path: path}
}

// Next atomically increments the order number, persists it, and returns the new value.
// It is safe for concurrent use from multiple goroutines; the backing mutex
// serialises all reads and writes.
func (s *OrderNumberStore) Next() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var data orderNumberFile
	if b, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(b, &data)
	}

	data.LastOrderNumber++

	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(s.path, b, 0o600); err != nil {
		return 0, err
	}
	return data.LastOrderNumber, nil
}
