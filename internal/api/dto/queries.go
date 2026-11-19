package dto

// SavedQueryPayload is the wire representation of a saved query.
type SavedQueryPayload struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	SQL         string         `json:"sql"`
	Params      map[string]any `json:"params"`
}

// CreateSavedQueryRequest matches the legacy electron-mssql-app create body.
type CreateSavedQueryRequest struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	SQL         string         `json:"sql"`
	Params      map[string]any `json:"params"`
}

// UpdateSavedQueryRequest is a partial update; nil fields are unchanged.
type UpdateSavedQueryRequest struct {
	Description *string         `json:"description"`
	SQL         *string         `json:"sql"`
	Params      *map[string]any `json:"params"`
}

type CreateSavedQueryResponse struct {
	OK   bool   `json:"ok"`
	Name string `json:"name"`
}

type UpdateSavedQueryResponse struct {
	OK      bool              `json:"ok"`
	Updated SavedQueryPayload `json:"updated"`
}

type DeleteSavedQueryResponse struct {
	OK bool `json:"ok"`
}

// RunSavedQueryResponse merges the erp-connector envelope (api/status/
// rowCount/rows/recordsets) with the legacy electron-mssql-app fields
// (value/rowsAffected) so existing backend callers work unchanged.
type RunSavedQueryResponse struct {
	API          string             `json:"api"`
	Status       string             `json:"status"`
	Name         string             `json:"name"`
	RowCount     int                `json:"rowCount"`
	Rows         []map[string]any   `json:"rows"`
	Recordsets   [][]map[string]any `json:"recordsets"`
	Value        []map[string]any   `json:"value"`
	RowsAffected []int64            `json:"rowsAffected"`
}

// SendOrderStatusResponse reports the state of a queued order job.
type SendOrderStatusResponse struct {
	JobID        string   `json:"jobId"`
	Status       string   `json:"status"`
	OrderNumber  int64    `json:"orderNumber"`
	WrittenFiles []string `json:"writtenFiles,omitempty"`
	Error        string   `json:"error,omitempty"`
	// Import is what Hashavshevet's own import log says about the order's
	// importer run (research/reverse-engineering/hasavshevet-importer.md P1).
	// Absent when no importer ran, or on a connector older than this field.
	// A report: it never changes status.
	Import *OrderImportCheck `json:"import,omitempty"`
}

// OrderImportCheck is the import confirmation of one order. result is one of
// imported, logged_no_document, failed, not_logged, unchecked.
type OrderImportCheck struct {
	// Outcome is the one verdict to act on: imported | partial (imported
	// WITHOUT the refused lines in runLog.refusals: Masofon2's partial import
	// is on; never resend the whole order) | duplicate (already in
	// Hashavshevet, do not resend) | refused (runLog.refusals) | retry (refused
	// for a passing reason, not imported) | not_run (the profile reads another
	// file) | unknown. Absent on checks stored by an older connector.
	Outcome     string  `json:"outcome,omitempty"`
	Result      string  `json:"result"`
	Detail      string  `json:"detail,omitempty"`
	Profile     string  `json:"profile,omitempty"`
	LogStatus   string  `json:"logStatus,omitempty"`
	StockIDs    []int64 `json:"stockIds,omitempty"`
	DocumentID  int64   `json:"documentId,omitempty"`
	StockStatus *int64  `json:"stockStatus,omitempty"`
	MatchedBy   string  `json:"matchedBy,omitempty"`
	// RunLog is the Masofon run's own log (<bat>.ok|err.…LOG in ErrDataPath).
	RunLog *OrderImportRunLog `json:"runLog,omitempty"`
	// MovedTo is where a refused IMOVEIN.doc was moved (hasMoveRefusedOrders).
	MovedTo   string `json:"movedTo,omitempty"`
	TimedOut  bool   `json:"timedOut,omitempty"`
	CheckedAt string `json:"checkedAt"`
}

// OrderImportRunLog is a Masofon run log: ok or error, no input file, the
// run's last line, and up to 20 error lines (value | message | row).
type OrderImportRunLog struct {
	File string `json:"file"`
	// OK is the log's name (.ok / .err): the whole run's, not this order's.
	OK bool `json:"ok"`
	// Live: the run was stopped before it finished its log; this is the
	// unfinished <bat>.err.
	Live bool `json:"live,omitempty"`
	// Section is what the log says about this order's own file: imported |
	// partial | refused | duplicate | retry | no_input; absent when the log
	// has no file sections and was read whole.
	Section  string                  `json:"section,omitempty"`
	NoInput  bool                    `json:"noInput,omitempty"`
	Summary  string                  `json:"summary,omitempty"`
	Errors   []string                `json:"errors,omitempty"`
	Refusals []OrderImportRunRefusal `json:"refusals,omitempty"`
	// Transient: refused for a passing reason (another station issuing, a
	// second run, a deadlock): this run did not import the file.
	Transient bool `json:"transient,omitempty"`
	// SystemError: the run reported a system error; SystemErrors are the
	// importer error log's lines written during the run (up to 20).
	SystemError  bool     `json:"systemError,omitempty"`
	SystemErrors []string `json:"systemErrors,omitempty"`
	// OtherFiles counts other files' sections in the same log (not shown).
	OtherFiles int `json:"otherFiles,omitempty"`
}

// OrderImportRunRefusal is one refusal of the order's file: the value
// refused, Hashavshevet's code (Stock.NNN) and message, and the file's row.
type OrderImportRunRefusal struct {
	Value   string `json:"value,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
	Row     int    `json:"row,omitempty"`
}

// SendOrderQueueResponse counts the orders accepted and not yet written
// (GET /api/sendOrder). Both 0 means a restart loses nothing.
type SendOrderQueueResponse struct {
	Queued  int `json:"queued"`
	Running int `json:"running"`
}
