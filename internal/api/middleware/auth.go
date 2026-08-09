package middleware

import (
	"net/http"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/api/respond"
)

// TokenVerifier validates a bearer credential — in practice a token issued by
// POST /auth/token. It returns nil when the credential is acceptable.
type TokenVerifier func(credential string) error

// Auth requires a token this installation issued.
//
// There is one credential and one way to obtain it. The static bearer token this
// connector used to accept as an alternative was removed on 2026-08-09: two
// credentials meant two things to rotate and two ways in, and the caller that
// matters never used the other one.
//
// Every failure is a flat 401: missing, malformed, wrong and expired are
// deliberately indistinguishable. That status is also a contract — callers treat
// exactly 401 as "re-authenticate and retry once". A 403 or a 500 for an expired
// token turns a self-healing hiccup into an integration that stays broken until
// somebody clears a cached credential by hand.
func Auth(verify TokenVerifier, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Fields(r.Header.Get("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			unauthorized(w)
			return
		}

		if verify == nil || verify(parts[1]) != nil {
			unauthorized(w)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func unauthorized(w http.ResponseWriter) {
	respond.Error(w, http.StatusUnauthorized, "Unauthorized", "UNAUTHORIZED", nil)
}
