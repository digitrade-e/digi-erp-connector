package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/api/respond"
	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/db"
)

func NewHealthHandler(cfg config.Config, dbPassword string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		if err := db.TestConnection(ctx, cfg, dbPassword); err != nil {
			respond.Error(w, http.StatusServiceUnavailable, "Database connection failed", "DB_UNAVAILABLE", nil)
			return
		}

		respond.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}
