package hasavshevet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// processOrderWithNumber tells a failure before its first file write
// (ErrOrderNotWritten: nothing of the order is on disk, so it may be tried
// again under the same number) from one after it (a file may exist: final).
func TestProcessOrder_FailuresBeforeTheFirstWriteAreNotWritten(t *testing.T) {
	valid := keyedOrder("k")
	noAccount := keyedOrder("k")
	noAccount.Account = nil
	invalid := keyedOrder("k")
	invalid.Details = nil

	for _, tc := range []struct {
		name       string
		req        OrderRequest
		noDir      bool
		block      string // a directory planted where this path must be a file
		notWritten bool
	}{
		{name: "sendOrderDir unset", req: valid, noDir: true, notWritten: true},
		{name: "invalid request", req: invalid, notWritten: true},
		{name: "no account and no database", req: noAccount, notWritten: true},
		{name: "history folder cannot be created", req: valid, block: "history", notWritten: true},
		{name: "IMOVEIN.doc cannot be opened", req: valid, block: "IMOVEIN.doc", notWritten: true},
		{name: "IMOVEIN.prm cannot be opened (IMOVEIN.doc written)", req: valid, block: "IMOVEIN.prm", notWritten: false},
		{name: "history copy cannot be opened", req: valid, block: filepath.Join("history", "7", "IMOVEIN_7.doc"), notWritten: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := config.Config{SendOrderDir: dir}
			if tc.noDir {
				cfg.SendOrderDir = ""
			}
			if tc.block == "history" {
				if err := os.WriteFile(filepath.Join(dir, "history"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			} else if tc.block != "" {
				if err := os.MkdirAll(filepath.Join(dir, tc.block), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			s := NewSender(nil, cfg, NewOrderNumberStore(filepath.Join(dir, "lastOrderNumber.json")), noopLogger{})

			_, err := s.processOrderWithNumber(context.Background(), tc.req, 7)
			if err == nil {
				t.Fatal("processOrderWithNumber succeeded")
			}
			if got := errors.Is(err, ErrOrderNotWritten); got != tc.notWritten {
				t.Errorf("errors.Is(%v, ErrOrderNotWritten) = %v; want %v", err, got, tc.notWritten)
			}
		})
	}
}

// The marker does not change what the error says.
func TestProcessOrder_NotWrittenKeepsTheMessage(t *testing.T) {
	s := NewSender(nil, config.Config{}, nil, noopLogger{})
	_, err := s.processOrderWithNumber(context.Background(), keyedOrder("k"), 7)
	if err == nil || err.Error() != "sendOrderDir is not configured" {
		t.Errorf("error = %v; want the plain message", err)
	}
}
