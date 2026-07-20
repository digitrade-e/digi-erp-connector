package queries

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"
)

func namedArgsByName(t *testing.T, args []any) map[string]any {
	t.Helper()
	byName := make(map[string]any, len(args))
	for _, arg := range args {
		na, ok := arg.(sql.NamedArg)
		if !ok {
			t.Fatalf("expected sql.NamedArg, got %T", arg)
		}
		byName[na.Name] = na.Value
	}
	return byName
}

func TestDetectIntegerParams(t *testing.T) {
	query := `
SELECT TOP (@top) *
FROM dbo.Stock
ORDER BY ValueDate DESC
OFFSET @offset ROWS FETCH NEXT @pageSize ROWS ONLY
`

	hints := detectIntegerParams(query)
	for _, name := range []string{"top", "offset", "pagesize"} {
		if _, ok := hints[name]; !ok {
			t.Fatalf("expected integer hint for %q", name)
		}
	}
}

func TestBuildNamedArgs_CoercesHintedStringIntegers(t *testing.T) {
	params := map[string]any{
		"dateFrom": "2026-01-01",
		"offset":   "0",
		"pageSize": "100",
		"search":   "00123",
	}
	query := "SELECT * FROM t ORDER BY id OFFSET @offset ROWS FETCH NEXT @pageSize ROWS ONLY"

	args := BuildNamedArgs(query, params)
	if len(args) != len(params) {
		t.Fatalf("expected %d args, got %d", len(params), len(args))
	}
	byName := namedArgsByName(t, args)

	if got, ok := byName["offset"].(int64); !ok || got != 0 {
		t.Fatalf("expected offset int64(0), got %T(%v)", byName["offset"], byName["offset"])
	}
	if got, ok := byName["pageSize"].(int64); !ok || got != 100 {
		t.Fatalf("expected pageSize int64(100), got %T(%v)", byName["pageSize"], byName["pageSize"])
	}
	if got, ok := byName["search"].(string); !ok || got != "00123" {
		t.Fatalf("expected search string %q, got %T(%v)", "00123", byName["search"], byName["search"])
	}
}

func TestBuildNamedArgs_ForcedStringParams(t *testing.T) {
	// electron-mssql-app behavior: skuArray/warehouse/sku/syncKey always bind
	// as strings even when they look numeric.
	params := map[string]any{
		"skuArray":  "1001,1002,1003",
		"warehouse": "10",
		"sku":       float64(12345),
		"syncKey":   "0042",
	}

	byName := namedArgsByName(t, BuildNamedArgs("SELECT 1", params))

	for name, want := range map[string]string{
		"skuArray":  "1001,1002,1003",
		"warehouse": "10",
		"sku":       "12345",
		"syncKey":   "0042",
	} {
		got, ok := byName[name].(string)
		if !ok || got != want {
			t.Fatalf("expected %s string %q, got %T(%v)", name, want, byName[name], byName[name])
		}
	}
}

func TestInferStringValue(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want any
	}{
		{name: "integer", raw: "42", want: int64(42)},
		{name: "decimal", raw: "3.14", want: float64(3.14)},
		{name: "plain string", raw: "hello", want: "hello"},
		{name: "leading zero stays... integer like electron", raw: "007", want: int64(7)},
		{name: "mixed alnum", raw: "10a", want: "10a"},
		{name: "negative not matched", raw: "-5", want: "-5"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := InferStringValue(tt.raw)
			if got != tt.want {
				t.Fatalf("expected %T(%v), got %T(%v)", tt.want, tt.want, got, got)
			}
		})
	}

	t.Run("date", func(t *testing.T) {
		got := InferStringValue("2026-01-15")
		d, ok := got.(time.Time)
		if !ok {
			t.Fatalf("expected time.Time, got %T(%v)", got, got)
		}
		if d.Year() != 2026 || d.Month() != time.January || d.Day() != 15 {
			t.Fatalf("unexpected date: %v", d)
		}
	})

	t.Run("date with time", func(t *testing.T) {
		got := InferStringValue("2026-01-15T10:30:00")
		if _, ok := got.(time.Time); !ok {
			t.Fatalf("expected time.Time, got %T(%v)", got, got)
		}
	})

	t.Run("invalid date stays string", func(t *testing.T) {
		got := InferStringValue("2026-99-99")
		if _, ok := got.(string); !ok {
			t.Fatalf("expected string, got %T(%v)", got, got)
		}
	})
}

func TestNormalizeParamValue(t *testing.T) {
	tests := []struct {
		name      string
		paramName string
		value     any
		hints     map[string]struct{}
		want      any
	}{
		{name: "float integer", paramName: "x", value: float64(42), want: int64(42)},
		{name: "float decimal", paramName: "x", value: float64(3.14), want: float64(3.14)},
		{name: "json number integer", paramName: "x", value: json.Number("12"), want: int64(12)},
		{name: "json number decimal", paramName: "x", value: json.Number("2.5"), want: float64(2.5)},
		{
			name:      "string integer with hint",
			paramName: "offset",
			value:     "10",
			hints:     map[string]struct{}{"offset": {}},
			want:      int64(10),
		},
		{name: "string integer without hint", paramName: "search", value: "10", want: "10"},
		{
			name:      "string non-integer with hint",
			paramName: "offset",
			value:     "10a",
			hints:     map[string]struct{}{"offset": {}},
			want:      "10a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeParamValue(tt.paramName, tt.value, tt.hints)
			switch want := tt.want.(type) {
			case float64:
				gotF, ok := got.(float64)
				if !ok || gotF != want {
					t.Fatalf("expected float64(%v), got %T(%v)", want, got, got)
				}
			default:
				if got != want {
					t.Fatalf("expected %T(%v), got %T(%v)", want, want, got, got)
				}
			}
		})
	}
}

func TestIsPlainDML(t *testing.T) {
	tests := []struct {
		query string
		want  bool
	}{
		{"SELECT * FROM t", false},
		{"WITH cte AS (SELECT 1 AS x) SELECT x FROM cte", false},
		{"EXEC dbo.SomeProc @p = 1", false},
		{"INSERT INTO t (a) VALUES (@a)", true},
		{"UPDATE t SET a = @a WHERE id = @id", true},
		{"DELETE FROM t WHERE id = @id", true},
		{"INSERT INTO t (a) OUTPUT INSERTED.id VALUES (@a)", false},
		{"UPDATE t SET a = 'output' WHERE id = @id", true}, // literal must not count as OUTPUT clause
	}

	for _, tt := range tests {
		if got := isPlainDML(tt.query); got != tt.want {
			t.Fatalf("isPlainDML(%q) = %v, want %v", tt.query, got, tt.want)
		}
	}
}
