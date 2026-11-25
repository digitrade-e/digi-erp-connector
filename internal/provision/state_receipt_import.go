package provision

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
	"github.com/digitrade-e/digi-erp-connector/internal/secrets"
)

// One receipt's import in state (plan 096 phase 3): state {"receiptNumber": N} looks up, in the company database
// this box reads, the import runs that read the receipt's file, the document the newest one created, and its
// section of the Masofon run log (hasavshevet.CheckReceiptImport). It is asked of the box that holds the database —
// on a split install not the box that wrote the file — and stores nothing.

// receiptImportTimeout bounds the lookup: two indexed reads and a run-log file.
const receiptImportTimeout = 30 * time.Second

// StateReceiptImport is state's receiptImport block.
type StateReceiptImport struct {
	// Untrusted is always true: the run log's lines and file names are the ERP's text.
	Untrusted bool `json:"untrusted"`
	hasavshevet.ReceiptImport
}

func stateReceiptImport(ctx context.Context, env Env, cfg *config.Config, number int64) *StateReceiptImport {
	out := &StateReceiptImport{Untrusted: true}
	out.ReceiptNumber = number
	out.CheckedAt = env.Now().UTC().Format(time.RFC3339)
	unchecked := func(detail string) *StateReceiptImport {
		out.Result, out.Detail = hasavshevet.ReceiptImportUnchecked, detail
		return out
	}
	if cfg == nil {
		return unchecked("config.yaml cannot be read: no database to look in")
	}
	pattern := ""
	if strings.TrimSpace(cfg.Documents.Receipts.Folder) != "" {
		pattern = cfg.ReceiptNamePattern()
	}
	out.FileName = hasavshevet.ReceiptFileName(pattern, number)
	if cfg.ERP != config.ERPHasavshevet {
		return unchecked("this box's ERP is not Hashavshevet")
	}
	dbName := strings.TrimSpace(cfg.DB.Database)
	if strings.TrimSpace(cfg.DB.Host) == "" || dbName == "" {
		return unchecked("this box reads no database (db.host / db.database): ask the box that does")
	}
	if !hasavshevet.IsSafeDBName(dbName) {
		return unchecked("db.database is not a plain database name")
	}
	pw, err := env.Sys.SecretGet(config.DBPasswordKey(cfg.ERP))
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		return unchecked("the database password is not stored on this box")
	case err != nil:
		return unchecked("the database password cannot be read")
	}
	conn, err := env.OpenDB(*cfg, string(pw))
	if err != nil {
		// A driver's error can carry the DSN, password included: never echoed.
		return unchecked("the database cannot be opened with this box's login")
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, receiptImportTimeout)
	defer cancel()
	logDir := hasavshevet.ResolveRunLogDirs(*cfg).LogDir
	got, _ := conn.ReceiptImport(ctx, dbName, number, out.FileName, logDir)
	out.ReceiptImport = got
	return out
}
