package middleware

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/api/utils"
)

// LegacyTokenVerifier validates a bearer credential that is not the static
// token — in practice a legacy electron-mssql-app JWT. It returns nil when the
// credential is acceptable. Auth calls it only after the constant-time static
// comparison fails, and only when legacy compatibility is enabled (a nil
// verifier means "static token only").
type LegacyTokenVerifier func(credential string) error

func Auth(token string, next http.Handler) http.Handler {
	return AuthWithLegacy(token, nil, next)
}

// AuthWithLegacy accepts either the static bearer token or, when verifyLegacy is
// non-nil, a credential that verifier accepts. The static path keeps its
// constant-time comparison and is always tried first, so enabling the legacy
// path cannot weaken or slow down the primary one.
func AuthWithLegacy(token string, verifyLegacy LegacyTokenVerifier, next http.Handler) http.Handler {
	expected := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Fields(r.Header.Get("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			utils.WriteError(w, http.StatusUnauthorized, "Unauthorized", "UNAUTHORIZED", nil)
			return
		}

		credential := parts[1]
		if subtle.ConstantTimeCompare([]byte(credential), expected) == 1 {
			next.ServeHTTP(w, r)
			return
		}

		if verifyLegacy != nil && verifyLegacy(credential) == nil {
			next.ServeHTTP(w, r)
			return
		}

		utils.WriteError(w, http.StatusUnauthorized, "Unauthorized", "UNAUTHORIZED", nil)
	})
}
