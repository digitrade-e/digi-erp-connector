package hasavshevet

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Order pre-check (hasOrderPrecheck, research/reverse-engineering/
// hasavshevet-importer.md P4): before a NEW order is given a number, ask the
// company database what Hashavshevet's importer would refuse — one refused
// line fails the importer's whole file, and the file is then retried by every
// run. Refused here, the caller gets 400 VALIDATION_ERROR naming the field
// (never the value) and no number is used. A repeat of a known historyId is
// never checked (Accept answers it from the index first). A database that
// cannot be read lets the order through: the check is a filter, never a new
// dependency of order intake.

// precheckReader reads what the pre-check needs; a seam for tests.
type precheckReader interface {
	// documentKnown: the Hashavshevet-6 code is mapped by DocumentsDefV6 to a
	// document type DocumentsDef defines.
	documentKnown(ctx context.Context, code int) (bool, error)
	accountKnown(ctx context.Context, key string) (bool, error)
	// missingItems returns the keys Items does not hold.
	missingItems(ctx context.Context, keys []string) ([]string, error)
}

type sqlPrecheck struct {
	db     *sql.DB
	dbName string // IsSafeDBName-checked before it is bracket-quoted
}

func (r sqlPrecheck) documentKnown(ctx context.Context, code int) (bool, error) {
	q := fmt.Sprintf(`SELECT COUNT(*) FROM [%[1]s].[dbo].[DocumentsDefV6] v
JOIN [%[1]s].[dbo].[DocumentsDef] d ON d.DocumentID = v.ID
WHERE v.Version6Code = @code`, r.dbName)
	var n int
	err := r.db.QueryRowContext(ctx, q, sql.Named("code", strconv.Itoa(code))).Scan(&n)
	return n > 0, err
}

func (r sqlPrecheck) accountKnown(ctx context.Context, key string) (bool, error) {
	q := fmt.Sprintf(`SELECT COUNT(*) FROM [%s].[dbo].[Accounts] WHERE AccountKey = @key`, r.dbName)
	var n int
	err := r.db.QueryRowContext(ctx, q, sql.Named("key", key)).Scan(&n)
	return n > 0, err
}

// precheckItemBatch keeps every statement far under SQL Server's 2 100
// parameters.
const precheckItemBatch = 500

func (r sqlPrecheck) missingItems(ctx context.Context, keys []string) ([]string, error) {
	found := map[string]bool{}
	for start := 0; start < len(keys); start += precheckItemBatch {
		batch := keys[start:min(start+precheckItemBatch, len(keys))]
		names := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, k := range batch {
			names[i] = "@k" + strconv.Itoa(i)
			args[i] = sql.Named("k"+strconv.Itoa(i), k)
		}
		q := fmt.Sprintf(`SELECT ItemKey FROM [%s].[dbo].[Items] WHERE ItemKey IN (%s)`, r.dbName, strings.Join(names, ", "))
		rows, err := r.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				rows.Close()
				return nil, err
			}
			found[strings.ToUpper(strings.TrimSpace(k))] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	var missing []string
	for _, k := range keys {
		if !found[strings.ToUpper(strings.TrimSpace(k))] {
			missing = append(missing, k)
		}
	}
	return missing, nil
}

const precheckTimeout = 5 * time.Second

func (s *Sender) precheckReaderFor() precheckReader {
	if s.precheck != nil {
		return s.precheck
	}
	dbName := strings.TrimSpace(s.cfg.DB.Database)
	if s.db == nil || dbName == "" || !IsSafeDBName(dbName) {
		return nil
	}
	return sqlPrecheck{db: s.db, dbName: dbName}
}

// precheckOrder returns a *FieldError for the first thing Hashavshevet would
// refuse, or nil — also when the database cannot be read (logged).
func (s *Sender) precheckOrder(req OrderRequest) error {
	r := s.precheckReaderFor()
	if r == nil {
		s.log.Warn("order pre-check skipped: no database to check against")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), precheckTimeout)
	defer cancel()
	skip := func(what string, err error) error {
		s.log.Warn(fmt.Sprintf("order pre-check historyId=%q: %s not readable (%v); the order goes through unchecked", req.HistoryID, what, err))
		return nil
	}

	if code := headerDocumentCode(req); code != 0 {
		ok, err := r.documentKnown(ctx, code)
		if err != nil {
			return skip("document types", err)
		}
		if !ok {
			field := "documentType"
			if req.DocumentID != 0 {
				field = "documentId"
			}
			return &FieldError{Field: field, Reason: "the document code is not a document type of this Hashavshevet company"}
		}
	}

	key, field := strings.TrimSpace(req.UserExtID), "userExtId"
	if req.Account != nil {
		key, field = strings.TrimSpace(req.Account.AccountKey), "account/accountKey"
	}
	if key != "" {
		ok, err := r.accountKnown(ctx, key)
		if err != nil {
			return skip("accounts", err)
		}
		if !ok {
			return &FieldError{Field: field, Reason: "the account is not in Hashavshevet"}
		}
	}

	var keys []string
	index := map[string]int{}
	for i, d := range req.Details {
		k := strings.TrimSpace(d.SKU)
		if k == "" || k == "*" || k == "#" { // comment lines; empty is the layout check's
			continue
		}
		if _, seen := index[strings.ToUpper(k)]; !seen {
			index[strings.ToUpper(k)] = i
			keys = append(keys, k)
		}
	}
	if len(keys) > 0 {
		missing, err := r.missingItems(ctx, keys)
		if err != nil {
			return skip("items", err)
		}
		if len(missing) > 0 {
			return &FieldError{Field: fmt.Sprintf("details[%d].sku", index[strings.ToUpper(strings.TrimSpace(missing[0]))]),
				Reason: fmt.Sprintf("the item is not in Hashavshevet (%d of the order's items are not)", len(missing))}
		}
	}
	return nil
}
