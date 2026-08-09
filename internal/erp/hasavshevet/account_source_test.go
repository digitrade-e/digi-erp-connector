package hasavshevet

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"

	// Registers the "sqlserver" driver; these tests never reach a server.
	_ "github.com/microsoft/go-mssqldb"
)

type noopLogger struct{}

func (noopLogger) Info(string)         {}
func (noopLogger) Error(string, error) {}
func (noopLogger) Warn(string)         {}
func (noopLogger) Success(string)      {}
func (noopLogger) Close() error        { return nil }

// deadDB is a valid pool pointing at an address with no listener — a configured
// database that happens to be unreachable.
func deadDB(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := sql.Open("sqlserver", "sqlserver://sa:pw@127.0.0.1:1?database=test")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}

// A connector deployed only to write order files has no database of its own.
// These tests pin the contract that makes that deployment possible: customer
// details supplied with the order replace the [dbo].[Accounts] lookup entirely.
// See docs/deployment-topologies.md.

func senderWithoutDB(t *testing.T) *Sender {
	t.Helper()
	return NewSender(nil, config.Config{}, nil, noopLogger{})
}

func TestResolveAccountUsesRequestDetailsWithoutADatabase(t *testing.T) {
	s := senderWithoutDB(t)

	got, err := s.resolveAccount(context.Background(), OrderRequest{
		UserExtID: "1234",
		Account: &AccountDetails{
			AccountKey: "1234",
			FullName:   "לקוח בדיקה",
			Address:    "הרצל 1",
			City:       "תל אביב",
			Phone:      "03-1234567",
			Agent:      "7",
			HProtect:   "X",
		},
	})
	if err != nil {
		t.Fatalf("resolveAccount with supplied details: %v", err)
	}

	if got.AccountKey != "1234" || got.FullName != "לקוח בדיקה" || got.City != "תל אביב" {
		t.Errorf("supplied details were not used verbatim: %+v", got)
	}
	if got.Agent != "7" || got.HProtect != "X" {
		t.Errorf("agent/hProtect lost: %+v", got)
	}
}

// The account key identifies the customer in the IMOVEIN header. Callers that
// already send userExtId should not have to repeat it.
func TestResolveAccountFallsBackToUserExtID(t *testing.T) {
	s := senderWithoutDB(t)

	got, err := s.resolveAccount(context.Background(), OrderRequest{
		UserExtID: "5678",
		Account:   &AccountDetails{FullName: "No key supplied"},
	})
	if err != nil {
		t.Fatalf("resolveAccount: %v", err)
	}
	if got.AccountKey != "5678" {
		t.Errorf("accountKey = %q, want it to fall back to userExtId", got.AccountKey)
	}
}

func TestResolveAccountRequiresAnIdentifier(t *testing.T) {
	s := senderWithoutDB(t)

	_, err := s.resolveAccount(context.Background(), OrderRequest{
		Account: &AccountDetails{FullName: "Nameless"},
	})
	if err == nil {
		t.Fatal("expected an error when neither accountKey nor userExtId is present")
	}
}

// Without a database and without supplied details there is nothing to build an
// order from. The error must name both ways out, because whoever reads it is
// deciding between configuring a database and changing the backend payload.
func TestResolveAccountWithoutDatabaseOrDetails(t *testing.T) {
	s := senderWithoutDB(t)

	_, err := s.resolveAccount(context.Background(), OrderRequest{UserExtID: "1234"})
	if err == nil {
		t.Fatal("expected an error with no database and no account details")
	}
	msg := err.Error()
	for _, want := range []string{"account", "db.host"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q should mention %q so the reader knows the options", msg, want)
		}
	}
}

// A configured database with no db.database set cannot look anything up either.
func TestResolveAccountWithDatabaseButNoCatalog(t *testing.T) {
	s := NewSender(deadDB(t), config.Config{}, nil, noopLogger{})

	_, err := s.resolveAccount(context.Background(), OrderRequest{UserExtID: "1234"})
	if err == nil {
		t.Fatal("expected an error when db.database is empty")
	}
	if !strings.Contains(err.Error(), "db.database") {
		t.Errorf("error %q should name the missing setting", err)
	}
}

// The whole point, end to end: a connector with no database at all produces a
// complete, correctly sized IMOVEIN pair from an order that carries its own
// customer details.
func TestProcessOrderWithoutADatabase(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{SendOrderDir: dir} // no DB block whatsoever
	store := NewOrderNumberStore(filepath.Join(dir, "lastOrderNumber.json"))
	s := NewSender(nil, cfg, store, noopLogger{})

	res, err := s.ProcessOrder(context.Background(), OrderRequest{
		DocumentType: "ORDER",
		UserExtID:    "1234",
		DueDate:      "2026-08-09",
		CreatedDate:  "2026-08-09",
		HistoryID:    "H-1",
		Currency:     "ILS",
		Total:        100,
		Account: &AccountDetails{
			AccountKey: "1234",
			FullName:   "Test Customer",
			Address:    "Herzl 1",
			City:       "Tel Aviv",
		},
		Details: []OrderLineItem{
			{Title: "Widget", SKU: "1001", Quantity: 2, SinglePrice: 25, TotalPrice: 50},
			{Title: "Gadget", SKU: "1002", Quantity: 1, SinglePrice: 50, TotalPrice: 50},
		},
	})
	if err != nil {
		t.Fatalf("ProcessOrder without a database: %v", err)
	}
	if res.OrderNumber == 0 {
		t.Error("no order number assigned")
	}

	doc := filepath.Join(dir, "IMOVEIN.doc")
	info, err := os.Stat(doc)
	if err != nil {
		t.Fatalf("IMOVEIN.doc not written: %v", err)
	}
	// One fixed-width record per line item (header fields repeat on each row),
	// plus a trailing LF. Two line items ordered above.
	const rowSize = prmTotalRecordLength + 1
	if info.Size() != 2*rowSize {
		t.Errorf("IMOVEIN.doc is %d bytes, want %d (2 line items x %d-byte record + LF)",
			info.Size(), 2*rowSize, prmTotalRecordLength)
	}
	if _, err := os.Stat(filepath.Join(dir, "IMOVEIN.prm")); err != nil {
		t.Errorf("IMOVEIN.prm not written: %v", err)
	}
	// The history copy must be there too.
	if _, err := os.Stat(filepath.Join(dir, "history",
		strconv.FormatInt(res.OrderNumber, 10),
		"IMOVEIN_"+strconv.FormatInt(res.OrderNumber, 10)+".doc")); err != nil {
		t.Errorf("history copy missing: %v", err)
	}
}
