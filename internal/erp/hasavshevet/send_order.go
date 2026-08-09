package hasavshevet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/logger"
)

// OrderRequest is the internal representation of a send-order request,
// translated from the API DTO before enqueueing.
// DBName is not part of the request; the Sender resolves it from config.
type OrderRequest struct {
	DocumentType  string
	UserExtID     string
	DueDate       string
	CreatedDate   string
	Comment       string
	Discount      float64
	HistoryID     string
	Total         float64
	Currency      string
	CustomerEmail string // optional; used for PDF email delivery
	Details       []OrderLineItem
	// Account optionally carries the customer details normally read from
	// [dbo].[Accounts]. When set it is used as-is and no lookup happens, which
	// is what lets a connector with no database build orders.
	Account *AccountDetails
}

// AccountDetails is the caller-supplied form of the account record: the columns
// the connector would otherwise read from [dbo].[Accounts].
type AccountDetails struct {
	AccountKey string
	FullName   string
	Address    string
	City       string
	Phone      string
	Agent      string
	HProtect   string
}

// OrderLineItem is one line item in an order.
type OrderLineItem struct {
	Title         string
	SKU           string
	Quantity      float64
	Packs         float64 // number of packages → IMOVEIN line33 (אריזות)
	OriginalPrice float64
	SinglePrice   float64
	TotalPrice    float64
	Discount      float64
}

// OrderResult is returned by ProcessOrder on success.
type OrderResult struct {
	OrderNumber  int64
	WrittenFiles []string
	Account      AccountInfo
}

// AccountInfo holds customer data needed for PDF generation.
// Exported mirror of the internal accountInfo queried from the DB.
type AccountInfo struct {
	AccountKey string
	FullName   string
	Address    string
	City       string
	Phone      string
}

// accountInfo holds the DB columns needed from the Accounts table.
type accountInfo struct {
	AccountKey string
	FullName   string
	Address    string
	City       string
	Phone      string
	Agent      string
	HProtect   string
}

// Sender orchestrates the Hasavshevet send-order pipeline:
// account lookup → currency rate → file generation → has.exe execution.
//
// ProcessOrder must only be called from a single goroutine at a time
// (guaranteed by OrderQueue's single-worker model) because IMOVEIN.doc/.prm
// are shared files in SendOrderDir that cannot be written concurrently.
type Sender struct {
	db          *sql.DB
	cfg         config.Config
	numberStore *OrderNumberStore
	log         logger.LoggerService
}

// NewSender creates a Sender. db and numberStore must be non-nil.
func NewSender(db *sql.DB, cfg config.Config, numberStore *OrderNumberStore, log logger.LoggerService) *Sender {
	return &Sender{db: db, cfg: cfg, numberStore: numberStore, log: log}
}

// ProcessOrder executes the full send-order pipeline for one order.
// It is NOT safe for concurrent IMOVEIN file access; always call via OrderQueue.
func (s *Sender) ProcessOrder(ctx context.Context, req OrderRequest) (*OrderResult, error) {
	orderNum, err := s.nextOrderNumber()
	if err != nil {
		return nil, err
	}
	return s.processOrderWithNumber(ctx, req, orderNum)
}

// nextOrderNumber increments and persists the order sequence.
func (s *Sender) nextOrderNumber() (int64, error) {
	if s.numberStore == nil {
		return 0, errors.New("order number store is not configured")
	}
	orderNum, err := s.numberStore.Next()
	if err != nil {
		return 0, fmt.Errorf("get order number: %w", err)
	}
	return orderNum, nil
}

// processOrderWithNumber executes the full send-order flow using a pre-reserved
// order number. If orderNum is zero, it reserves one before continuing.
func (s *Sender) processOrderWithNumber(ctx context.Context, req OrderRequest, orderNum int64) (*OrderResult, error) {
	if strings.TrimSpace(s.cfg.SendOrderDir) == "" {
		return nil, errors.New("sendOrderDir is not configured")
	}

	// 1. Pre-flight validation (business rules + Hasavshevet mandatory field spec)
	if err := validateOrderRequest(req); err != nil {
		return nil, err
	}

	// 2. Concurrency-safe order number (reserved at queue submit when possible).
	if orderNum == 0 {
		var err error
		orderNum, err = s.nextOrderNumber()
		if err != nil {
			return nil, err
		}
	}

	dbName := strings.TrimSpace(s.cfg.DB.Database)
	s.log.Info(fmt.Sprintf("processing order orderNumber=%d historyId=%s userExtId=%s dbName=%q",
		orderNum, req.HistoryID, req.UserExtID, dbName))

	// 3. Account details: supplied by the caller, or looked up in the ERP database
	account, err := s.resolveAccount(ctx, req)
	if err != nil {
		return nil, err
	}

	// 4. Currency rate (non-fatal; default 1.0 matches legacy behaviour)
	rate := 1.0
	if s.db != nil && dbName != "" {
		if r, err := s.queryRate(ctx, dbName, req.Currency); err != nil {
			s.log.Warn(fmt.Sprintf("rate lookup failed currency=%s: %v; using 1.0", req.Currency, err))
		} else {
			rate = r
		}
	}

	// 5. Build DOC and PRM content
	hdr, moves := buildIMOVEIN(orderNum, account, req, rate)
	docBytes, err := generateDOC(hdr, moves)
	if err != nil {
		return nil, fmt.Errorf("generate DOC: %w", err)
	}
	prmBytes := generatePRM()

	// 6. Ensure output directories exist
	dir := s.cfg.SendOrderDir
	historyDir := filepath.Join(dir, "history", fmt.Sprintf("%d", orderNum))
	if err := os.MkdirAll(historyDir, 0o755); err != nil {
		return nil, fmt.Errorf("create history dir: %w", err)
	}

	// 7. Write files
	// Active import files (read by has.exe): single-worker queue prevents collision.
	// History copies: permanent audit trail per order.
	toWrite := []struct {
		path string
		data []byte
	}{
		{filepath.Join(dir, "IMOVEIN.doc"), docBytes},
		{filepath.Join(dir, "IMOVEIN.prm"), prmBytes},
		{filepath.Join(historyDir, fmt.Sprintf("IMOVEIN_%d.doc", orderNum)), docBytes},
		{filepath.Join(historyDir, fmt.Sprintf("IMOVEIN_%d.prm", orderNum)), prmBytes},
	}

	var writtenFiles []string
	for _, f := range toWrite {
		if err := os.WriteFile(f.path, f.data, 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", f.path, err)
		}
		writtenFiles = append(writtenFiles, f.path)
	}

	// 8. Execute Hasavshevet importer (Windows only; no-op on other platforms).
	// HasBatFile (Masofon-generated BAT launcher) takes precedence over HasExePath.
	// The single-worker queue guarantees the previous import is finished before
	// the next order's files are written and the importer is invoked again.
	switch {
	case strings.TrimSpace(s.cfg.HasBatFile) != "":
		start := time.Now()
		exitCode, output, execErr := runBatFile(ctx, s.cfg.HasBatFile)
		s.log.Info(fmt.Sprintf("digi.bat exit=%d durationMs=%d output=%q orderNumber=%d",
			exitCode, time.Since(start).Milliseconds(), output, orderNum))
		if execErr != nil || exitCode != 0 {
			s.log.Error(fmt.Sprintf("digi.bat failed orderNumber=%d exit=%d", orderNum, exitCode), execErr)
		}
	case strings.TrimSpace(s.cfg.HasExePath) != "":
		start := time.Now()
		exitCode, output, execErr := runImporter(ctx, s.cfg.HasExePath, s.cfg.HasParamFile, dir)
		s.log.Info(fmt.Sprintf("has.exe exit=%d durationMs=%d output=%q orderNumber=%d",
			exitCode, time.Since(start).Milliseconds(), output, orderNum))
		if execErr != nil {
			// Log but do not fail: files are written; import can be retried.
			s.log.Error(fmt.Sprintf("has.exe failed orderNumber=%d", orderNum), execErr)
		}
	}

	s.log.Success(fmt.Sprintf("order complete orderNumber=%d files=%v", orderNum, writtenFiles))
	return &OrderResult{
		OrderNumber:  orderNum,
		WrittenFiles: writtenFiles,
		Account: AccountInfo{
			AccountKey: account.AccountKey,
			FullName:   account.FullName,
			Address:    account.Address,
			City:       account.City,
			Phone:      account.Phone,
		},
	}, nil
}

// buildIMOVEIN maps a validated OrderRequest to the stockHeader + []stockMove
// needed by generateDOC.
func buildIMOVEIN(orderNum int64, account accountInfo, req OrderRequest, rate float64) (stockHeader, []stockMove) {
	now := time.Now()
	createdDate := parseTimeOrNow(req.CreatedDate, now)
	dueDate := parseTimeOrNow(req.DueDate, now)

	shortDate := fmt.Sprintf("%02d/%02d/%04d",
		createdDate.Day(), int(createdDate.Month()), createdDate.Year())

	discount := req.Discount
	if discount >= 100 {
		discount = 99.99
	}

	// Document type → Hasavshevet header DocumentID code
	var headerDocID, wareHouse int
	switch req.DocumentType {
	case "ORDER":
		wareHouse = 1
		if req.Currency == `ש"ח` {
			headerDocID = 30
		} else {
			headerDocID = 32
		}
	case "QUOATE":
		headerDocID = 40
		wareHouse = 1
	case "RETURN":
		headerDocID = 74
		wareHouse = 1
	}

	hdr := stockHeader{
		AccountKey:   account.AccountKey,
		MyID:         orderNum,
		DocumentID:   headerDocID,
		AccountName:  sanitizeField(account.FullName),
		Address:      sanitizeField(account.Address),
		City:         sanitizeField(account.City),
		Phone:        sanitizeField(account.Phone),
		Asmahta2:     fmt.Sprintf("%d", orderNum),
		ShortDate:    shortDate,
		Agent:        account.Agent,
		WareHouse:    wareHouse,
		DiscountPrcR: fmt.Sprintf("%.2f", discount),
		VatPrc:       "18.00",
		Copies:       "1",
		Currency:     req.Currency,
		Rate:         fmt.Sprintf("%.4f", rate),
		Remarks:      sanitizeComment(req.Comment, 250),
		HProtect:     account.HProtect,
	}

	_ = dueDate // available for future per-line DueDate fields

	moves := make([]stockMove, 0, len(req.Details))
	for _, d := range req.Details {
		// Packs (line33/אריזות): only emit when known (>0) so legacy callers that
		// omit packs keep writing a blank field, identical to prior behaviour.
		packs := ""
		if d.Packs > 0 {
			packs = fmt.Sprintf("%.2f", d.Packs)
		}
		moves = append(moves, stockMove{
			ItemKey:     d.SKU,
			ItemName:    sanitizeField(d.Title),
			Quantity:    fmt.Sprintf("%.2f", d.Quantity),
			Packs:       packs,
			Price:       fmt.Sprintf("%.2f", d.OriginalPrice),
			DiscountPrc: fmt.Sprintf("%.2f", d.Discount),
			Unit:        "יח'",
		})
	}

	return hdr, moves
}

// queryAccount fetches account columns required for the DOC header.
// resolveAccount returns the customer details for the order.
//
// Details supplied with the request win and skip the database entirely — that is
// what allows a connector with no database (an order-writing node sitting beside
// the ERP importer) to build IMOVEIN files at all. Otherwise they are read from
// [dbo].[Accounts] as before.
func (s *Sender) resolveAccount(ctx context.Context, req OrderRequest) (accountInfo, error) {
	if a := req.Account; a != nil {
		account := accountInfo{
			AccountKey: strings.TrimSpace(a.AccountKey),
			FullName:   a.FullName,
			Address:    a.Address,
			City:       a.City,
			Phone:      a.Phone,
			Agent:      a.Agent,
			HProtect:   a.HProtect,
		}
		// The account key identifies the customer in the IMOVEIN record; fall
		// back to the order's userExtId, which is the same identifier.
		if account.AccountKey == "" {
			account.AccountKey = strings.TrimSpace(req.UserExtID)
		}
		if account.AccountKey == "" {
			return accountInfo{}, errors.New("account.accountKey or userExtId is required")
		}
		return account, nil
	}

	if s.db == nil {
		return accountInfo{}, errors.New(
			"no database is configured and the order carries no \"account\" details: " +
				"either send account details with the order, or configure db.host/db.user/db.port/db.database")
	}

	dbName := strings.TrimSpace(s.cfg.DB.Database)
	if dbName == "" {
		return accountInfo{}, errors.New("db.database is not set, so the customer account cannot be looked up")
	}

	account, err := s.queryAccount(ctx, dbName, req.UserExtID)
	if err != nil {
		return accountInfo{}, fmt.Errorf("query account %q: %w", req.UserExtID, err)
	}
	return account, nil
}

func (s *Sender) queryAccount(ctx context.Context, dbName, userExtID string) (accountInfo, error) {
	if !isSafeDBName(dbName) {
		return accountInfo{}, fmt.Errorf("invalid dbName %q", dbName)
	}
	query := fmt.Sprintf(
		`SELECT TOP 1 AccountKey, FullName, Address, City, Phone, Agent, HProtect`+
			` FROM [%s].[dbo].[Accounts] WHERE AccountKey = @userExtId`,
		dbName,
	)
	row := s.db.QueryRowContext(ctx, query, sql.Named("userExtId", userExtID))

	var a accountInfo
	var fullName, address, city, phone, agent, hprotect sql.NullString
	if err := row.Scan(&a.AccountKey, &fullName, &address, &city, &phone, &agent, &hprotect); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return accountInfo{}, fmt.Errorf("account not found: %q", userExtID)
		}
		return accountInfo{}, err
	}
	a.FullName = fullName.String
	a.Address = address.String
	a.City = city.String
	a.Phone = phone.String
	a.Agent = agent.String
	a.HProtect = hprotect.String
	return a, nil
}

// queryRate fetches the latest exchange rate for currencyCode.
// Returns 1.0 when no rate is found (matches legacy fallback).
func (s *Sender) queryRate(ctx context.Context, dbName, currencyCode string) (float64, error) {
	if !isSafeDBName(dbName) {
		return 1.0, fmt.Errorf("invalid dbName %q", dbName)
	}
	query := fmt.Sprintf(
		`SELECT TOP 1 Rate FROM [%s].[dbo].[Rates]`+
			` WHERE CurrencyCode = @currencyCode ORDER BY DatF DESC`,
		dbName,
	)
	row := s.db.QueryRowContext(ctx, query, sql.Named("currencyCode", currencyCode))
	var rate sql.NullFloat64
	if err := row.Scan(&rate); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 1.0, nil
		}
		return 1.0, err
	}
	if !rate.Valid || rate.Float64 == 0 {
		return 1.0, nil
	}
	return rate.Float64, nil
}

// validateOrderRequest enforces required fields per the legacy controller and
// Hasavshevet mandatory field spec (line4, line8/asmachta2, line22/SKU, line23/qty≠0).
func validateOrderRequest(req OrderRequest) error {
	var missing []string
	if req.DocumentType == "" {
		missing = append(missing, "documentType")
	}
	if req.UserExtID == "" {
		missing = append(missing, "userExtId")
	}
	if req.DueDate == "" {
		missing = append(missing, "dueDate")
	}
	if req.CreatedDate == "" {
		missing = append(missing, "createdDate")
	}
	if req.HistoryID == "" {
		missing = append(missing, "historyId")
	}
	if len(req.Details) == 0 {
		missing = append(missing, "details (must be non-empty array)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required fields: %s", strings.Join(missing, ", "))
	}

	switch req.DocumentType {
	case "ORDER", "QUOATE", "RETURN":
	default:
		return fmt.Errorf("invalid documentType %q; allowed: ORDER, QUOATE, RETURN", req.DocumentType)
	}

	for i, d := range req.Details {
		if d.SKU == "" {
			return fmt.Errorf("details[%d]: sku is required (line22)", i)
		}
		if d.Quantity == 0 {
			return fmt.Errorf("details[%d]: quantity cannot be zero (line23 Hasavshevet spec)", i)
		}
		if d.Title == "" {
			return fmt.Errorf("details[%d]: title is required", i)
		}
	}
	return nil
}

// isSafeDBName returns true when name contains only alphanumeric and underscore chars.
// Mirrors the legacy Node ensureDbNameSafe pattern to prevent SQL injection via dbName.
func isSafeDBName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	return true
}

// parseTimeOrNow parses an ISO-8601 date string; returns fallback on any parse failure.
func parseTimeOrNow(s string, fallback time.Time) time.Time {
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return fallback
}

// sanitizeField removes single quotes and converts newlines to ". "
// matching the legacy Node sanitisation applied to free-text DB fields.
func sanitizeField(s string) string {
	s = strings.ReplaceAll(s, "'", "")
	s = strings.ReplaceAll(s, "\n", ". ")
	return s
}

// sanitizeComment applies sanitizeField and truncates to maxLen runes.
func sanitizeComment(s string, maxLen int) string {
	s = sanitizeField(s)
	runes := []rune(s)
	if len(runes) > maxLen {
		return string(runes[:maxLen])
	}
	return s
}
