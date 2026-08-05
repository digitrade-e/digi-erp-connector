package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/digitrade-e/digi-erp-connector/internal/api/dto"
	"github.com/digitrade-e/digi-erp-connector/internal/api/utils"
	"github.com/digitrade-e/digi-erp-connector/internal/logger"
	"github.com/digitrade-e/digi-erp-connector/internal/queries"
)

// NewLegacyQueryHandler serves POST /api/query — the electron-mssql-app ad-hoc
// SQL route, available only when config legacyCompat.allowRawSQL is true.
//
// This is the one endpoint in this codebase that accepts SQL text from a
// caller. It exists because the production backend this connector replaces may
// still use it, and cutting over must not break it. It is therefore fenced in:
//
//   - read-only validated (single SELECT/WITH statement, no comments, keyword
//     blocklist applied after string literals are stripped)
//   - every parameter bound via sql.Named — never concatenated
//   - the full statement is logged on every call, so you can see exactly what
//     the backend sends and retire this route once it sends nothing
//
// Saved queries remain the supported model; see docs/legacy-compat.md.
func NewLegacyQueryHandler(runner *queries.Runner, log logger.LoggerService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if runner == nil {
			writeLegacyError(w, http.StatusServiceUnavailable, "query_failed")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, legacyBodyLimit)
		defer r.Body.Close()

		var req dto.LegacyQueryRequest
		dec := json.NewDecoder(r.Body)
		// UseNumber keeps large integers exact; the binder understands json.Number.
		dec.UseNumber()
		if err := dec.Decode(&req); err != nil {
			writeLegacyError(w, http.StatusBadRequest, "sql_required")
			return
		}
		if err := ensureEOF(dec); err != nil {
			writeLegacyError(w, http.StatusBadRequest, "sql_required")
			return
		}

		// Logged in full and at every call: this is the audit trail for a route
		// that takes SQL from the network.
		if log != nil {
			log.Info(fmt.Sprintf("legacy-compat route used: POST /api/query from=%s params=%d sql=%q",
				r.RemoteAddr, len(req.Params), req.Query))
		}

		if err := queries.ValidateReadOnly(req.Query); err != nil {
			switch {
			case errors.Is(err, queries.ErrSQLRequired):
				writeLegacyError(w, http.StatusBadRequest, "sql_required")
			default:
				// The old app answered only_select_allowed for everything it
				// refused; keep that single string so callers' checks match.
				if log != nil {
					log.Warn(fmt.Sprintf("legacy /api/query rejected: %v", err))
				}
				writeLegacyError(w, http.StatusBadRequest, "only_select_allowed")
			}
			return
		}

		result, err := runner.Run(r.Context(), req.Query, req.Params)
		if err != nil {
			if log != nil {
				log.Error("legacy /api/query execution failed", err)
			}
			switch {
			case errors.Is(err, queries.ErrNoDatabase):
				writeLegacyError(w, http.StatusServiceUnavailable, "query_failed")
			case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
				writeLegacyError(w, http.StatusGatewayTimeout, "query_failed")
			case errors.Is(err, queries.ErrRowLimit):
				writeLegacyError(w, http.StatusRequestEntityTooLarge, "query_failed")
			default:
				writeLegacyError(w, http.StatusInternalServerError, "query_failed")
			}
			return
		}

		utils.WriteJSON(w, http.StatusOK, dto.LegacyQueryResponse{
			Value:        result.Rows(),
			RowsAffected: result.RowsAffected,
		})
	}
}
