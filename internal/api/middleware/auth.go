package middleware

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/api/respond"
)

// Auth accepts the installation's static bearer token and nothing else.
//
// The comparison is constant-time, so a wrong token cannot be discovered by
// measuring how long the rejection takes. Every failure is a flat 401: missing,
// malformed and incorrect are deliberately indistinguishable to a caller.
//
// An expired or invalid credential must produce exactly 401 — backends treat
// that status as "re-authenticate", and any other code turns a recoverable
// hiccup into a broken integration.
func Auth(token string, next http.Handler) http.Handler {
	expected := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Fields(r.Header.Get("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			respond.Error(w, http.StatusUnauthorized, "Unauthorized", "UNAUTHORIZED", nil)
			return
		}

		if subtle.ConstantTimeCompare([]byte(parts[1]), expected) != 1 {
			respond.Error(w, http.StatusUnauthorized, "Unauthorized", "UNAUTHORIZED", nil)
			return
		}

		next.ServeHTTP(w, r)
	})
}
