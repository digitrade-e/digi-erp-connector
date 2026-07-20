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
}
