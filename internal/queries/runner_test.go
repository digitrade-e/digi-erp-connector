package queries

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	// Registers the "sqlserver" driver so a pool can be constructed without a
	// live server; these tests never reach one.
	_ "github.com/microsoft/go-mssqldb"
)

// deadPool returns a syntactically valid pool pointing at an address with no
// listener, which is what "the database is down" looks like to the runner.
func deadPool(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := sql.Open("sqlserver", "sqlserver://sa:pw@127.0.0.1:1?database=test")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}

// The daemon opens its pool lazily, so a database that is down still yields a
// valid *sql.DB whose queries fail on connect. That has to be reported as "no
// database" (503, worth retrying) and not as a query error (500, your request
// was wrong) — the distinction is what tells a backend how to react, and it is
// what docs/deployment-topologies.md promises for a database reached over the
// network.
func TestRunReportsAnUnreachableDatabaseAsNoDatabase(t *testing.T) {
	runner := NewRunner(deadPool(t), 5*time.Second, 100)

	_, err := runner.Run(context.Background(), "SELECT 1", nil)
	if err == nil {
		t.Fatal("expected an error against an address with no listener")
	}
	if !errors.Is(err, ErrNoDatabase) {
		t.Errorf("got %v, want it to wrap ErrNoDatabase so handlers answer 503", err)
	}
}

// The same classification must apply to the Exec path (plain DML), not only to
// queries that return rows.
func TestRunReportsUnreachableDatabaseForDML(t *testing.T) {
	runner := NewRunner(deadPool(t), 5*time.Second, 100)

	_, err := runner.Run(context.Background(), "UPDATE dbo.Items SET Price = 1", nil)
	if err == nil {
		t.Fatal("expected an error against an address with no listener")
	}
	if !errors.Is(err, ErrNoDatabase) {
		t.Errorf("got %v, want it to wrap ErrNoDatabase", err)
	}
}

// A cancelled request is the caller's doing, not a connectivity problem: it must
// keep its own error so the handler answers 504 rather than 503.
func TestRunPreservesContextCancellation(t *testing.T) {
	runner := NewRunner(deadPool(t), 5*time.Second, 100)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := runner.Run(ctx, "SELECT 1", nil)
	if err == nil {
		t.Fatal("expected an error from a cancelled context")
	}
	if errors.Is(err, ErrNoDatabase) {
		t.Errorf("cancellation was misreported as a connectivity failure: %v", err)
	}
}

// A nil pool must fail closed rather than panic.
func TestRunWithNilPool(t *testing.T) {
	runner := NewRunner(nil, time.Second, 10)

	if _, err := runner.Run(context.Background(), "SELECT 1", nil); !errors.Is(err, ErrNoDatabase) {
		t.Errorf("got %v, want ErrNoDatabase", err)
	}
}

func TestRunRejectsEmptyQuery(t *testing.T) {
	runner := NewRunner(deadPool(t), time.Second, 10)

	for _, q := range []string{"", "   ", "\n\t"} {
		if _, err := runner.Run(context.Background(), q, nil); !errors.Is(err, ErrEmptyQuery) {
			t.Errorf("Run(%q) = %v, want ErrEmptyQuery", q, err)
		}
	}
}
