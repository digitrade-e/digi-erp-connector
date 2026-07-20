package queries

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"
)

// Definition is a single saved SQL query. The on-disk JSON format is
// compatible with the legacy electron-mssql-app custom_sql_data.json:
//
//	{ "<name>": {"description": "...", "sql": "...", "params": {...}}, ... }
//
// Params holds default parameter values; the runner merges request
// parameters over them at execution time.
type Definition struct {
	Description string         `json:"description"`
	SQL         string         `json:"sql"`
	Params      map[string]any `json:"params"`
}

// Named pairs a query name with its definition for list responses.
type Named struct {
	Name string `json:"name"`
	Definition
}

var (
	ErrExists      = errors.New("query name already exists")
	ErrNotFound    = errors.New("query not found")
	ErrInvalidName = errors.New("invalid query name")
	ErrInvalidSQL  = errors.New("query sql is required")
)

const maxQueryNameLen = 200

// Store is a thread-safe registry of saved queries persisted to a JSON file.
// Mutations are written atomically (temp file + rename), mirroring config.Save.
type Store struct {
	mu   sync.RWMutex
	path string
	data map[string]Definition
}

// NewStore loads the registry from path. A missing file yields an empty store.
func NewStore(path string) (*Store, error) {
	s := &Store{path: path, data: make(map[string]Definition)}

	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) > 0 {
		if err := json.Unmarshal(b, &s.data); err != nil {
			return nil, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
		}
	}
	if s.data == nil {
		s.data = make(map[string]Definition)
	}
	return s, nil
}

// ValidateName rejects empty, oversized, control-character, and path-like names.
func ValidateName(name string) error {
	if strings.TrimSpace(name) == "" || len(name) > maxQueryNameLen {
		return ErrInvalidName
	}
	for _, r := range name {
		if unicode.IsControl(r) || r == '/' || r == '\\' {
			return ErrInvalidName
		}
	}
	return nil
}

// Count returns the number of stored queries.
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data)
}

// List returns all queries sorted by name.
func (s *Store) List() []Named {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Named, 0, len(s.data))
	for name, def := range s.data {
		out = append(out, Named{Name: name, Definition: def})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the definition for name.
func (s *Store) Get(name string) (Definition, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	def, ok := s.data[name]
	return def, ok
}

// Create adds a new query. Fails with ErrExists if the name is taken.
func (s *Store) Create(name string, def Definition) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if strings.TrimSpace(def.SQL) == "" {
		return ErrInvalidSQL
	}
	if def.Params == nil {
		def.Params = map[string]any{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[name]; ok {
		return ErrExists
	}
	s.data[name] = def
	return s.persistLocked()
}

// Update applies a partial update; nil fields are left unchanged.
func (s *Store) Update(name string, description *string, sqlText *string, params *map[string]any) (Definition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	def, ok := s.data[name]
	if !ok {
		return Definition{}, ErrNotFound
	}
	if description != nil {
		def.Description = *description
	}
	if sqlText != nil {
		if strings.TrimSpace(*sqlText) == "" {
			return Definition{}, ErrInvalidSQL
		}
		def.SQL = *sqlText
	}
	if params != nil {
		if *params == nil {
			def.Params = map[string]any{}
		} else {
			def.Params = *params
		}
	}
	s.data[name] = def
	if err := s.persistLocked(); err != nil {
		return Definition{}, err
	}
	return def, nil
}

// Delete removes a query.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.data[name]; !ok {
		return ErrNotFound
	}
	delete(s.data, name)
	return s.persistLocked()
}

func (s *Store) persistLocked() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	out, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, "queries-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_ = tmp.Chmod(0o600)

	_, writeErr := tmp.Write(out)
	syncErr := tmp.Sync()
	closeErr := tmp.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(tmpName)
		if writeErr != nil {
			return writeErr
		}
		if syncErr != nil {
			return syncErr
		}
		return closeErr
	}

	_ = os.Remove(s.path)
	if err := os.Rename(tmpName, s.path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}
