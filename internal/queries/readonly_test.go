package queries

import (
	"errors"
	"testing"
)

func TestValidateReadOnly(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  error
	}{
		// Accepted: everything the old Node endpoint accepted and is read-only.
		{"simple select", "SELECT 1", nil},
		{"lowercase", "select top (10) * from dbo.Items", nil},
		{"cte", "WITH x AS (SELECT 1 AS a) SELECT a FROM x", nil},
		{"leading whitespace", "\n\t  SELECT * FROM dbo.Items", nil},
		{"trailing semicolon", "SELECT 1;", nil},
		{"trailing semicolon and space", "SELECT 1 ;  ", nil},
		{"semicolon inside literal", "SELECT * FROM t WHERE note = 'a;b'", nil},
		{"keyword inside literal", "SELECT * FROM t WHERE note = 'please DROP this'", nil},
		{"escaped quote in literal", "SELECT * FROM t WHERE note = 'it''s fine'", nil},
		{"column named updated", "SELECT updatedAt FROM t", nil},

		// Refused.
		{"empty", "", ErrSQLRequired},
		{"whitespace only", "   \n ", ErrSQLRequired},
		{"insert", "INSERT INTO t VALUES (1)", ErrSQLNotReadOnly},
		{"update", "UPDATE t SET a = 1", ErrSQLNotReadOnly},
		{"delete", "DELETE FROM t", ErrSQLNotReadOnly},
		{"exec proc", "EXEC dbo.DoThing", ErrSQLNotReadOnly},
		{"select then drop", "SELECT 1; DROP TABLE t", ErrSQLMultiStatement},
		{"select with embedded update", "SELECT 1 FROM t WHERE 1=1 UPDATE t SET a=1", ErrSQLNotReadOnly},
		{"line comment", "SELECT 1 -- comment", ErrSQLComments},
		{"block comment", "SELECT /* hi */ 1", ErrSQLComments},
		{"not a select", "TRUNCATE TABLE t", ErrSQLNotReadOnly},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateReadOnly(tc.query)
			if tc.want == nil {
				if err != nil {
					t.Errorf("ValidateReadOnly(%q) = %v, want nil", tc.query, err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("ValidateReadOnly(%q) = %v, want %v", tc.query, err, tc.want)
			}
		})
	}
}

// "select 1; drop" must be caught even though the semicolon-trimming relaxation
// allows a single trailing one.
func TestValidateReadOnlyTrailingSemicolonIsNotAnEscape(t *testing.T) {
	if err := ValidateReadOnly("SELECT 1;;"); err != nil {
		t.Errorf("multiple trailing semicolons should be tolerated: %v", err)
	}
	if err := ValidateReadOnly("SELECT 1; SELECT 2"); !errors.Is(err, ErrSQLMultiStatement) {
		t.Errorf("chained statements = %v, want ErrSQLMultiStatement", err)
	}
}
