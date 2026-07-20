package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/digitrade-e/digi-erp-connector/internal/api/dto"
	"github.com/digitrade-e/digi-erp-connector/internal/api/utils"
	"github.com/digitrade-e/digi-erp-connector/internal/queries"
)

const customSQLMaxBodyBytes = 1 << 20

// NewCreateCustomSQLHandler stores a new saved query.
// Serves POST /api/custom_sql and the legacy alias POST /api/create_custom_sql.
func NewCreateCustomSQLHandler(store *queries.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, customSQLMaxBodyBytes)
		defer r.Body.Close()

		var req dto.CreateSavedQueryRequest
		dec := json.NewDecoder(r.Body)
		dec.UseNumber()
		if err := dec.Decode(&req); err != nil {
			utils.WriteError(w, http.StatusBadRequest, "Invalid JSON body", "INVALID_JSON", nil)
			return
		}
		if err := ensureEOF(dec); err != nil {
			utils.WriteError(w, http.StatusBadRequest, "Invalid JSON body", "INVALID_JSON", nil)
			return
		}

		err := store.Create(req.Name, queries.Definition{
			Description: req.Description,
			SQL:         req.SQL,
			Params:      req.Params,
		})
		if err != nil {
			writeQueryStoreError(w, err)
			return
		}
		utils.WriteJSON(w, http.StatusOK, dto.CreateSavedQueryResponse{OK: true, Name: req.Name})
	}
}

// NewListCustomSQLHandler serves GET /api/custom_sql.
func NewListCustomSQLHandler(store *queries.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		items := store.List()
		out := make([]dto.SavedQueryPayload, 0, len(items))
		for _, it := range items {
			out = append(out, dto.SavedQueryPayload{
				Name:        it.Name,
				Description: it.Description,
				SQL:         it.SQL,
				Params:      it.Params,
			})
		}
		utils.WriteJSON(w, http.StatusOK, out)
	}
}

// NewGetCustomSQLHandler serves GET /api/custom_sql/{name}.
func NewGetCustomSQLHandler(store *queries.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		def, ok := store.Get(name)
		if !ok {
			utils.WriteError(w, http.StatusNotFound, "Query not found", "NOT_FOUND", nil)
			return
		}
		utils.WriteJSON(w, http.StatusOK, dto.SavedQueryPayload{
			Name:        name,
			Description: def.Description,
			SQL:         def.SQL,
			Params:      def.Params,
		})
	}
}

// NewUpdateCustomSQLHandler serves PATCH /api/custom_sql/{name}.
func NewUpdateCustomSQLHandler(store *queries.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, customSQLMaxBodyBytes)
		defer r.Body.Close()

		var req dto.UpdateSavedQueryRequest
		dec := json.NewDecoder(r.Body)
		dec.UseNumber()
		if err := dec.Decode(&req); err != nil {
			utils.WriteError(w, http.StatusBadRequest, "Invalid JSON body", "INVALID_JSON", nil)
			return
		}
		if err := ensureEOF(dec); err != nil {
			utils.WriteError(w, http.StatusBadRequest, "Invalid JSON body", "INVALID_JSON", nil)
			return
		}

		name := r.PathValue("name")
		def, err := store.Update(name, req.Description, req.SQL, req.Params)
		if err != nil {
			writeQueryStoreError(w, err)
			return
		}
		utils.WriteJSON(w, http.StatusOK, dto.UpdateSavedQueryResponse{
			OK: true,
			Updated: dto.SavedQueryPayload{
				Name:        name,
				Description: def.Description,
				SQL:         def.SQL,
				Params:      def.Params,
			},
		})
	}
}

// NewDeleteCustomSQLHandler serves DELETE /api/custom_sql/{name}.
func NewDeleteCustomSQLHandler(store *queries.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := store.Delete(r.PathValue("name")); err != nil {
			writeQueryStoreError(w, err)
			return
		}
		utils.WriteJSON(w, http.StatusOK, dto.DeleteSavedQueryResponse{OK: true})
	}
}

func writeQueryStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, queries.ErrExists):
		utils.WriteError(w, http.StatusConflict, "Query name must be unique", "NAME_CONFLICT", nil)
	case errors.Is(err, queries.ErrNotFound):
		utils.WriteError(w, http.StatusNotFound, "Query not found", "NOT_FOUND", nil)
	case errors.Is(err, queries.ErrInvalidName):
		utils.WriteError(w, http.StatusBadRequest, "Query name is required and must be valid", "NAME_REQUIRED", nil)
	case errors.Is(err, queries.ErrInvalidSQL):
		utils.WriteError(w, http.StatusBadRequest, "Query sql is required", "SQL_REQUIRED", nil)
	default:
		utils.WriteError(w, http.StatusInternalServerError, "Failed to persist saved queries", "STORE_ERROR", nil)
	}
}
