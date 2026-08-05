package handlers

// electron-mssql-app compatibility handlers.
//
// These routes exist only when config legacyCompat.enabled is true. They
// reproduce the old Node connector's paths, bodies and error strings so a
// backend that has not been migrated to the saved-query + static-token model
// keeps working across the cutover. Every handler logs when it is hit: that log
// is how you find out which of these the backend still needs before switching
// the compat layer off.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/api/dto"
	"github.com/digitrade-e/digi-erp-connector/internal/api/respond"
	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/db"
	"github.com/digitrade-e/digi-erp-connector/internal/legacyauth"
	"github.com/digitrade-e/digi-erp-connector/internal/logger"
	"github.com/digitrade-e/digi-erp-connector/internal/queries"
)

// writeLegacyError answers in the old app's shape: {"error":"snake_case_code"}.
func writeLegacyError(w http.ResponseWriter, status int, code string) {
	respond.JSON(w, status, dto.LegacyErrorResponse{Error: code})
}

func logLegacyHit(log logger.LoggerService, route string) {
	if log == nil {
		return
	}
	log.Info(fmt.Sprintf("legacy-compat route used: %s — migrate the caller and disable legacyCompat when this stops appearing", route))
}

// NewLegacyTokenHandler serves POST /auth/token.
//
// Unauthenticated by design (it is the credential exchange), but rate-limited
// like every other route. Credentials come from config, never from source.
func NewLegacyTokenHandler(cfg config.LegacyCompatConfig, signer *legacyauth.Signer, log logger.LoggerService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		logLegacyHit(log, "POST /auth/token")

		var req dto.LegacyTokenRequest
		if err := decodeJSONBody(w, r, &req); err != nil {
			writeLegacyError(w, http.StatusUnauthorized, "invalid_credentials")
			return
		}

		if req.Username != cfg.JWTUser || req.Password != cfg.JWTPassword {
			if log != nil {
				log.Warn(fmt.Sprintf("legacy /auth/token rejected: user=%q from=%s", req.Username, r.RemoteAddr))
			}
			writeLegacyError(w, http.StatusUnauthorized, "invalid_credentials")
			return
		}

		ttl := cfg.LegacyJWTExpiry()
		token, _, err := signer.Sign(req.Username, ttl)
		if err != nil {
			if log != nil {
				log.Error("legacy /auth/token failed to sign", err)
			}
			writeLegacyError(w, http.StatusInternalServerError, "token_error")
			return
		}

		respond.JSON(w, http.StatusOK, dto.LegacyTokenResponse{
			AccessToken: token,
			TokenType:   "Bearer",
			ExpiresIn:   int(ttl.Seconds()),
		})
	}
}

// NewLegacyPingHandler serves GET /api/ping — {"ok":true,"ts":<epoch millis>}.
// Unlike /api/health it does not touch the database, matching the old app.
func NewLegacyPingHandler(log logger.LoggerService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		logLegacyHit(log, "GET /api/ping")
		respond.JSON(w, http.StatusOK, dto.LegacyPingResponse{
			OK: true,
			TS: time.Now().UnixMilli(),
		})
	}
}

// NewLegacyTestConnectionHandler serves POST /api/test-connection: try the
// supplied MSSQL settings (merged onto the running config) and report success.
//
// Deviation from the old app, deliberate: the driver's error text is logged
// rather than returned. digi-erp-connector does not return raw DB driver errors
// to API clients, and the only consumer of that text was the Electron settings
// UI, which this cutover retires. The response shape is unchanged.
func NewLegacyTestConnectionHandler(cfg config.Config, dbPassword string, log logger.LoggerService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		logLegacyHit(log, "POST /api/test-connection")

		// An empty body is allowed and means "test the running configuration",
		// which is how the old app behaved. (It compared err.Error() to the
		// string "EOF"; errors.Is is the same check without the fragility.)
		var req dto.LegacyTestConnectionRequest
		if err := decodeJSONBody(w, r, &req); err != nil && !errors.Is(err, io.EOF) {
			respond.JSON(w, http.StatusBadRequest, dto.LegacyTestConnectionResponse{
				OK:    false,
				Error: "invalid_json",
			})
			return
		}

		candidate := cfg
		password := dbPassword
		if m := req.MSSQL; m != nil {
			if m.Server != nil {
				candidate.DB.Host = *m.Server
			}
			if m.Database != nil {
				candidate.DB.Database = *m.Database
			}
			if m.User != nil {
				candidate.DB.User = *m.User
			}
			if m.Port != nil {
				candidate.DB.Port = *m.Port
			}
			if m.Encrypt != nil {
				candidate.DB.Encrypt = *m.Encrypt
			}
			if m.TrustServerCertificate != nil {
				candidate.DB.TrustServerCertificate = *m.TrustServerCertificate
			}
			if m.Password != nil {
				password = *m.Password
			}
		}

		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		if err := db.TestConnection(ctx, candidate, password); err != nil {
			if log != nil {
				log.Warn(fmt.Sprintf("legacy /api/test-connection failed: host=%s db=%s user=%s err=%v",
					candidate.DB.Host, candidate.DB.Database, candidate.DB.User, err))
			}
			respond.JSON(w, http.StatusBadRequest, dto.LegacyTestConnectionResponse{
				OK:    false,
				Error: "connection_failed",
			})
			return
		}

		respond.JSON(w, http.StatusOK, dto.LegacyTestConnectionResponse{OK: true})
	}
}

// NewLegacyCustomersHandler serves GET /api/customers?limit=N — the old app's
// sample route: TOP (@limit) rows from dbo.Items, newest Id first, default 50,
// hard cap 200. Returns a bare JSON array like the original.
func NewLegacyCustomersHandler(runner *queries.Runner, log logger.LoggerService) http.HandlerFunc {
	const query = "SELECT TOP (@limit) * FROM dbo.Items ORDER BY Id DESC"
	return func(w http.ResponseWriter, r *http.Request) {
		logLegacyHit(log, "GET /api/customers")

		if runner == nil {
			writeLegacyError(w, http.StatusServiceUnavailable, "query_failed")
			return
		}

		limit := 50
		if raw := r.URL.Query().Get("limit"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil {
				limit = n
			}
		}
		if limit > 200 {
			limit = 200
		}
		if limit < 1 {
			limit = 1
		}

		result, err := runner.Run(r.Context(), query, map[string]any{"limit": int64(limit)})
		if err != nil {
			if log != nil {
				log.Error("legacy /api/customers failed", err)
			}
			if errors.Is(err, queries.ErrNoDatabase) {
				writeLegacyError(w, http.StatusServiceUnavailable, "query_failed")
				return
			}
			writeLegacyError(w, http.StatusInternalServerError, "query_failed")
			return
		}

		respond.JSON(w, http.StatusOK, result.Rows())
	}
}

// NewLegacyOrderHandler serves GET /api/orders/{id} — the old app's second
// sample route: one dbo.Items row by Id, 400 invalid_id / 404 not_found.
func NewLegacyOrderHandler(runner *queries.Runner, log logger.LoggerService) http.HandlerFunc {
	const query = "SELECT * FROM dbo.Items WHERE Id = @id"
	return func(w http.ResponseWriter, r *http.Request) {
		logLegacyHit(log, "GET /api/orders/{id}")

		if runner == nil {
			writeLegacyError(w, http.StatusServiceUnavailable, "query_failed")
			return
		}

		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeLegacyError(w, http.StatusBadRequest, "invalid_id")
			return
		}

		result, err := runner.Run(r.Context(), query, map[string]any{"id": id})
		if err != nil {
			if log != nil {
				log.Error("legacy /api/orders/{id} failed", err)
			}
			if errors.Is(err, queries.ErrNoDatabase) {
				writeLegacyError(w, http.StatusServiceUnavailable, "query_failed")
				return
			}
			writeLegacyError(w, http.StatusInternalServerError, "query_failed")
			return
		}

		rows := result.Rows()
		if len(rows) == 0 {
			writeLegacyError(w, http.StatusNotFound, "not_found")
			return
		}

		respond.JSON(w, http.StatusOK, rows[0])
	}
}
