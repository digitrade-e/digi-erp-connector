package handlers

import (
	"fmt"
	"net/http"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/api/dto"
	"github.com/digitrade-e/digi-erp-connector/internal/api/respond"
	"github.com/digitrade-e/digi-erp-connector/internal/auth"
	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/logger"
)

// NewTokenHandler serves POST /auth/token: credentials in, a short-lived bearer
// token out.
//
// Unauthenticated by necessity — it *is* the credential check — but rate-limited
// and logged like every other route, so guessing is throttled and attempts are
// visible.
//
// The response contract is narrow and callers depend on it exactly:
//
//   - success must be HTTP 200. Not 201, not 204 — at least one caller treats
//     anything else as a failure.
//   - the body must contain access_token. A caller that stores a missing value
//     will send an empty credential on its next request and loop on 401.
//
// token_type and expires_in are included for completeness; callers may ignore
// them, and at least one does.
func NewTokenHandler(cfg config.AuthConfig, signer *auth.Signer, log logger.LoggerService) http.HandlerFunc {
	ttl := cfg.TTL()

	return func(w http.ResponseWriter, r *http.Request) {
		var req dto.TokenRequest
		if err := decodeJSONBody(w, r, &req); err != nil {
			// Do not distinguish a malformed body from wrong credentials: both
			// mean "no token for you", and saying which helps only an attacker.
			unauthorizedCredentials(w, log, "", r.RemoteAddr, "malformed request body")
			return
		}

		// Constant-time comparison on both fields. The username is as much a
		// secret as the password here — there is exactly one account.
		userOK := constantTimeEqual(req.Username, cfg.Username)
		passOK := constantTimeEqual(req.Password, cfg.Password)
		if !userOK || !passOK {
			unauthorizedCredentials(w, log, req.Username, r.RemoteAddr, "credential mismatch")
			return
		}

		token, expiresAt, err := signer.Sign(req.Username, ttl)
		if err != nil {
			if log != nil {
				log.Error("failed to sign an auth token", err)
			}
			respond.Error(w, http.StatusInternalServerError, "Could not issue a token", "TOKEN_ISSUE_FAILED", nil)
			return
		}

		if log != nil {
			log.Info(fmt.Sprintf("issued an auth token for %q from %s, valid until %s",
				req.Username, r.RemoteAddr, expiresAt.UTC().Format(time.RFC3339)))
		}

		respond.JSON(w, http.StatusOK, dto.TokenResponse{
			AccessToken: token,
			TokenType:   "Bearer",
			ExpiresIn:   int(ttl.Seconds()),
		})
	}
}

// unauthorizedCredentials logs the attempt and answers 401. The password is
// never logged; the username is, because knowing which account was tried is what
// makes the log useful.
func unauthorizedCredentials(w http.ResponseWriter, log logger.LoggerService, username, remote, reason string) {
	if log != nil {
		log.Warn(fmt.Sprintf("rejected /auth/token: user=%q from=%s (%s)", username, remote, reason))
	}
	respond.Error(w, http.StatusUnauthorized, "Invalid credentials", "INVALID_CREDENTIALS", nil)
}

// NewPingHandler serves GET /api/ping — "is the service up and is my credential
// good".
//
// Deliberately does not touch the database, which is what separates it from
// /api/health: an operator checking whether a connection is configured correctly
// should get a clear yes even while the customer's SQL Server is down. Health
// answers the other question — can this connector actually serve data — and
// returns 503 in that case.
func NewPingHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		respond.JSON(w, http.StatusOK, dto.PingResponse{
			OK: true,
			TS: time.Now().UnixMilli(),
		})
	}
}
