package queries

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"
)

const (
	DefaultTimeout = 30 * time.Second
	DefaultMaxRows = 10000
)

var (
	ErrEmptyQuery = errors.New("query sql is empty")
	ErrRowLimit   = errors.New("row limit exceeded")
)

// Result holds the outcome of executing a saved query.
type Result struct {
	Recordsets   [][]map[string]any
	RowsAffected []int64
}

// Rows returns the first recordset (never nil).
func (r *Result) Rows() []map[string]any {
	if len(r.Recordsets) > 0 && r.Recordsets[0] != nil {
		return r.Recordsets[0]
	}
	return make([]map[string]any, 0)
}

// Runner executes saved queries against the connector's database.
//
// Saved queries are operator/backend-managed and therefore trusted: unlike
// the legacy raw-SQL endpoint there is no read-only restriction. All request
// values are still bound as named parameters — never concatenated.
type Runner struct {
	db      *sql.DB
	timeout time.Duration
	maxRows int
}

func NewRunner(db *sql.DB, timeout time.Duration, maxRows int) *Runner {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if maxRows <= 0 {
		maxRows = DefaultMaxRows
	}
	return &Runner{db: db, timeout: timeout, maxRows: maxRows}
}

// Run executes query with the given parameters and collects all recordsets.
func (r *Runner) Run(ctx context.Context, query string, params map[string]any) (*Result, error) {
	if strings.TrimSpace(query) == "" {
		return nil, ErrEmptyQuery
	}

	args := BuildNamedArgs(query, params)

	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	// Plain DML without an OUTPUT clause returns no recordsets; Exec is the
	// only way to surface the affected-row count (electron-mssql-app compat).
	if isPlainDML(query) {
		res, err := r.db.ExecContext(cctx, query, args...)
		if err != nil {
			return nil, err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			affected = 0
		}
		return &Result{
			Recordsets:   make([][]map[string]any, 0),
			RowsAffected: []int64{affected},
		}, nil
	}

	rows, err := r.db.QueryContext(cctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	recordsets, err := collectRecordsets(rows, r.maxRows)
	if err != nil {
		return nil, err
	}

	affected := make([]int64, 0, len(recordsets))
	for _, set := range recordsets {
		affected = append(affected, int64(len(set)))
	}
	return &Result{Recordsets: recordsets, RowsAffected: affected}, nil
}

// isPlainDML reports whether query is an INSERT/UPDATE/DELETE/MERGE without
// an OUTPUT clause, i.e. a statement that produces no recordset.
func isPlainDML(query string) bool {
	lower := strings.ToLower(stripStringLiterals(query))
	fields := strings.Fields(lower)
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "insert", "update", "delete", "merge":
		return !outputClauseRe.MatchString(lower)
	default:
		return false
	}
}

var outputClauseRe = regexp.MustCompile(`\boutput\b`)

// stripStringLiterals removes '...' literals (with '' escapes) so keyword
// detection never triggers on literal values.
func stripStringLiterals(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inString := false

	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inString {
			if ch == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inString = false
			}
			continue
		}
		if ch == '\'' {
			inString = true
			continue
		}
		b.WriteByte(ch)
	}

	return b.String()
}

func collectRecordsets(rows *sql.Rows, maxRows int) ([][]map[string]any, error) {
	recordsets := make([][]map[string]any, 0, 1)
	total := 0

	for {
		cols, err := rows.Columns()
		if err != nil {
			return nil, err
		}

		set := make([]map[string]any, 0)
		for rows.Next() {
			if maxRows > 0 && total >= maxRows {
				return nil, ErrRowLimit
			}

			values := make([]any, len(cols))
			scanArgs := make([]any, len(cols))
			for i := range values {
				scanArgs[i] = &values[i]
			}

			if err := rows.Scan(scanArgs...); err != nil {
				return nil, err
			}

			row := make(map[string]any, len(cols))
			for i, col := range cols {
				v := values[i]
				if b, ok := v.([]byte); ok {
					row[col] = string(b)
					continue
				}
				row[col] = v
			}
			set = append(set, row)
			total++
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}

		recordsets = append(recordsets, set)
		if !rows.NextResultSet() {
			break
		}
	}

	return recordsets, nil
}
