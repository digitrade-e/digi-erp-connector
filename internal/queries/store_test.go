package queries

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "queries.json")
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s, path
}

func TestStoreCreateGetListDelete(t *testing.T) {
	s, path := newTestStore(t)

	def := Definition{
		Description: "items by warehouse",
		SQL:         "SELECT * FROM dbo.Items WHERE WhsCode = @warehouse",
		Params:      map[string]any{"warehouse": "10"},
	}
	if err := s.Create("items_by_warehouse", def); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.Create("items_by_warehouse", def); err != ErrExists {
		t.Fatalf("expected ErrExists, got %v", err)
	}

	got, ok := s.Get("items_by_warehouse")
	if !ok || got.SQL != def.SQL || got.Description != def.Description {
		t.Fatalf("Get mismatch: %+v", got)
	}

	list := s.List()
	if len(list) != 1 || list[0].Name != "items_by_warehouse" {
		t.Fatalf("List mismatch: %+v", list)
	}

	// Persistence: a fresh store must see the same data.
	reloaded, err := NewStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Count() != 1 {
		t.Fatalf("expected 1 query after reload, got %d", reloaded.Count())
	}

	if err := s.Delete("items_by_warehouse"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete("items_by_warehouse"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestStoreUpdatePartial(t *testing.T) {
	s, _ := newTestStore(t)

	if err := s.Create("q", Definition{SQL: "SELECT 1", Params: map[string]any{"a": float64(1)}}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	desc := "updated description"
	got, err := s.Update("q", &desc, nil, nil)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Description != desc || got.SQL != "SELECT 1" {
		t.Fatalf("partial update clobbered fields: %+v", got)
	}
	if len(got.Params) != 1 {
		t.Fatalf("partial update clobbered params: %+v", got.Params)
	}

	empty := ""
	if _, err := s.Update("q", nil, &empty, nil); err != ErrInvalidSQL {
		t.Fatalf("expected ErrInvalidSQL for empty sql, got %v", err)
	}

	if _, err := s.Update("missing", &desc, nil, nil); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestStoreValidation(t *testing.T) {
	s, _ := newTestStore(t)

	if err := s.Create("", Definition{SQL: "SELECT 1"}); err != ErrInvalidName {
		t.Fatalf("expected ErrInvalidName for empty name, got %v", err)
	}
	if err := s.Create("a/b", Definition{SQL: "SELECT 1"}); err != ErrInvalidName {
		t.Fatalf("expected ErrInvalidName for slash, got %v", err)
	}
	if err := s.Create("ok", Definition{SQL: "   "}); err != ErrInvalidSQL {
		t.Fatalf("expected ErrInvalidSQL for empty sql, got %v", err)
	}
}

// TestStoreReadsElectronFormat verifies the on-disk format is drop-in
// compatible with electron-mssql-app's custom_sql_data.json so existing
// installations can be imported by copying the file.
func TestStoreReadsElectronFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom_sql_data.json")
	legacy := `{
  "stock_by_sku": {
    "description": "stock lookup",
    "sql": "SELECT * FROM dbo.Stock WHERE ItemCode IN (SELECT value FROM STRING_SPLIT(@skuArray, ','))",
    "params": { "skuArray": "", "warehouse": "10" }
  },
  "no_params_array": {
    "description": "electron stored empty defaults as an array",
    "sql": "SELECT 1",
    "params": []
  }
}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy file: %v", err)
	}

	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore on legacy file: %v", err)
	}
	def, ok := s.Get("stock_by_sku")
	if !ok {
		t.Fatalf("legacy query not found")
	}
	if def.Description != "stock lookup" || def.Params["warehouse"] != "10" {
		t.Fatalf("legacy fields mismatch: %+v", def)
	}

	// Round-trip: mutating must keep the same shape.
	if err := s.Create("second", Definition{SQL: "SELECT 2"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var raw map[string]Definition
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("round-trip parse: %v", err)
	}
	if len(raw) != 3 || raw["stock_by_sku"].Description != "stock lookup" {
		t.Fatalf("round-trip mismatch: %+v", raw)
	}
	if arr, ok := s.Get("no_params_array"); !ok || arr.Params == nil || len(arr.Params) != 0 {
		t.Fatalf("array params should normalize to empty map: %+v", arr)
	}
}
