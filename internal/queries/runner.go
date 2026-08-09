package queries

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
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
	// ErrNoDatabase is returned when the runner has no database handle — the
	// daemon starts even if the initial connection failed, so every execution
	// path must fail closed rather than dereference a nil *sql.DB.
	ErrNoDatabase = errors.New("no database connection")
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
	if r == nil || r.db == nil {
		return nil, ErrNoDatabase
	}
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
			return nil, r.classifyExecError(ctx, err)
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
		return nil, r.classifyExecError(ctx, err)
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

// classifyExecError decides whether a failed statement means "the database is
// unreachable" (503, retry later) or "this query failed" (500, a real error).
//
// The daemon builds its pool lazily, so a database that is down produces a
// perfectly valid *sql.DB whose queries fail on connect. Reporting that as a
// query error tells the caller their request was wrong, when in fact the server
// simply has no database right now. Distinguishing them by parsing driver error
// text would be fragile, so we ask the pool instead: if a fresh ping also fails,
// it is a connectivity problem.
//
// This runs only on the error path, so it costs nothing in the normal case.
func (r *Runner) classifyExecError(ctx context.Context, err error) error {
	// A caller-cancelled or timed-out request is not a connectivity problem.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	pingCtx, cancel := context.WithTimeout(ctx, connectivityProbeTimeout)
	defer cancel()
	if pingErr := r.db.PingContext(pingCtx); pingErr != nil {
		return fmt.Errorf("%w: %v", ErrNoDatabase, pingErr)
	}
	return err
}

// connectivityProbeTimeout bounds the ping in classifyExecError; it must stay
// short because it delays reporting a genuine query error.
const connectivityProbeTimeout = 3 * time.Second

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

// stripStringLiterals removes '...' literals (with ” escapes) so keyword
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

// legacyTimeLayout renders datetimes exactly as the Node connector did
// (JS Date.toJSON: UTC, always three fractional digits). Go's default
// time.Time marshalling drops a zero fraction and emits
// "2026-03-08T00:00:00Z" where the old app sent "2026-03-08T00:00:00.000Z";
// backends parsing that string strictly would break on the difference.
const legacyTimeLayout = "2006-01-02T15:04:05.000Z"

// isNumericTypeName lists the SQL Server types the MSSQL driver hands back as
// raw bytes. Left as-is they marshal to JSON *strings*, whereas the Node driver
// reported them as JSON *numbers* — arithmetic on the backend would silently
// change meaning (JS "0.00" + 1 === "0.001").
func isNumericTypeName(name string) bool {
	switch strings.ToUpper(name) {
	case "DECIMAL", "NUMERIC", "MONEY", "SMALLMONEY":
		return true
	}
	return false
}

func columnTypeName(colTypes []*sql.ColumnType, i int) string {
	if i < 0 || i >= len(colTypes) || colTypes[i] == nil {
		return ""
	}
	return colTypes[i].DatabaseTypeName()
}

// normalizeScanned converts a scanned driver value into the JSON shape the
// electron-mssql-app connector produced, so responses stay wire-compatible.
func normalizeScanned(v any, typeName string) any {
	switch t := v.(type) {
	case []byte:
		s := string(t)
		if isNumericTypeName(typeName) {
			// Only emit a bare number when it really is one; anything else
			// stays a string rather than producing invalid JSON.
			//
			// The digits are re-formatted to the shortest representation that
			// round-trips through float64, because that is precisely what the
			// Node connector emitted: its driver produced a JS number and
			// JSON.stringify printed 13085, not the server's 13085.00. Keeping
			// the raw scale would change int-vs-float typing for backends that
			// care (PHP json_decode gives int for 0, float for 0.00). Float64
			// is also exactly the precision the old connector had, so this is
			// no worse than what production serves today.
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return json.Number(strconv.FormatFloat(f, 'f', -1, 64))
			}
		}
		return s
	case time.Time:
		return t.UTC().Format(legacyTimeLayout)
	default:
		return v
	}
}

func collectRecordsets(rows *sql.Rows, maxRows int) ([][]map[string]any, error) {
	recordsets := make([][]map[string]any, 0, 1)
	total := 0

	for {
		cols, err := rows.Columns()
		if err != nil {
			return nil, err
		}
		// Column types drive the numeric normalisation below. Failure is not
		// fatal: without them values fall back to their raw representation.
		colTypes, ctErr := rows.ColumnTypes()
		if ctErr != nil {
			colTypes = nil
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
				row[col] = normalizeScanned(values[i], columnTypeName(colTypes, i))
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
