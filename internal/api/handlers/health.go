package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/api/utils"
	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/db"
)

func NewHealthHandler(cfg config.Config, dbPassword string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		if err := db.TestConnection(ctx, cfg, dbPassword); err != nil {
			utils.WriteError(w, http.StatusServiceUnavailable, "Database connection failed", "DB_UNAVAILABLE", nil)
			return
		}

		utils.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}
