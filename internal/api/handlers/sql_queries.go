package handlers

import (
	"context"
	"errors"
	"net/http"

	"github.com/digitrade-e/digi-erp-connector/internal/api/dto"
	"github.com/digitrade-e/digi-erp-connector/internal/api/utils"
	"github.com/digitrade-e/digi-erp-connector/internal/queries"
)

// NewRunSavedQueryHandler serves GET /api/sqlqueries/{name}.
//
// Ported from electron-mssql-app: stored default params are merged with URL
// query-string params (URL wins), string values are type-inferred unless the
// parameter name is a forced-string (skuArray/warehouse/sku/syncKey), and the
// stored SQL runs with every value bound as a named parameter.
func NewRunSavedQueryHandler(store *queries.Store, runner *queries.Runner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if runner == nil {
			utils.WriteError(w, http.StatusServiceUnavailable, "Database connection unavailable", "DB_UNAVAILABLE", nil)
			return
		}

		name := r.PathValue("name")
		def, ok := store.Get(name)
		if !ok {
			utils.WriteError(w, http.StatusNotFound, "Query not found", "NOT_FOUND", nil)
			return
		}

		merged := make(map[string]any, len(def.Params)+len(r.URL.Query()))
		for k, v := range def.Params {
			merged[k] = v
		}
		for k, vals := range r.URL.Query() {
			if len(vals) > 0 {
				merged[k] = vals[0]
			}
		}
		for k, v := range merged {
			if s, isStr := v.(string); isStr && !queries.IsForcedString(k) {
				merged[k] = queries.InferStringValue(s)
			}
		}

		result, err := runner.Run(r.Context(), def.SQL, merged)
		if err != nil {
			switch {
			case errors.Is(err, queries.ErrNoDatabase):
				utils.WriteError(w, http.StatusServiceUnavailable, "Database connection unavailable", "DB_UNAVAILABLE", nil)
			case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
				utils.WriteError(w, http.StatusGatewayTimeout, "Query timeout", "SQL_TIMEOUT", nil)
			case errors.Is(err, queries.ErrRowLimit):
				utils.WriteError(w, http.StatusRequestEntityTooLarge, "Row limit exceeded", "SQL_ROW_LIMIT", nil)
			case errors.Is(err, queries.ErrEmptyQuery):
				utils.WriteError(w, http.StatusBadRequest, "Query sql is required", "SQL_REQUIRED", nil)
			default:
				utils.WriteError(w, http.StatusInternalServerError, "Query execution failed", "DB_ERROR", nil)
			}
			return
		}

		rows := result.Rows()
		utils.WriteJSON(w, http.StatusOK, dto.RunSavedQueryResponse{
			API:          r.URL.Path,
			Status:       "success",
			Name:         name,
			RowCount:     len(rows),
			Rows:         rows,
			Recordsets:   result.Recordsets,
			Value:        rows,
			RowsAffected: result.RowsAffected,
		})
	}
}
