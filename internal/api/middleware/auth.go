package middleware

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/api/respond"
)

// TokenVerifier validates a bearer credential that is not the static token — in
// practice a token issued by POST /auth/token. It returns nil when the
// credential is acceptable.
//
// AuthWithExchange calls it only after the constant-time static comparison
// fails, and only when the exchange is enabled. A nil verifier means "static
// token only".
type TokenVerifier func(credential string) error

// AuthWithExchange accepts either the static bearer token or, when verify is
// non-nil, a credential that verify accepts — the two credentials described in
// docs/authentication.md.
//
// The static comparison runs first and is constant-time, so a wrong token cannot
// be discovered by timing and enabling the exchange cannot weaken or slow down
// the primary path.
//
// Every failure is a flat 401: missing, malformed and incorrect are deliberately
// indistinguishable. That status is also a contract — callers treat exactly 401
// as "re-authenticate and retry once". A 403 or a 500 for an expired token turns
// a self-healing hiccup into an integration that stays broken until somebody
// clears a cached credential by hand.
func AuthWithExchange(token string, verify TokenVerifier, next http.Handler) http.Handler {
	expected := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Fields(r.Header.Get("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			respond.Error(w, http.StatusUnauthorized, "Unauthorized", "UNAUTHORIZED", nil)
			return
		}

		credential := parts[1]
		if subtle.ConstantTimeCompare([]byte(credential), expected) == 1 {
			next.ServeHTTP(w, r)
			return
		}

		if verify != nil && verify(credential) == nil {
			next.ServeHTTP(w, r)
			return
		}

		respond.Error(w, http.StatusUnauthorized, "Unauthorized", "UNAUTHORIZED", nil)
	})
}
