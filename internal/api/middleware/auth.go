package middleware

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/api/utils"
)

func Auth(token string, next http.Handler) http.Handler {
	expected := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Fields(r.Header.Get("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") ||
			subtle.ConstantTimeCompare([]byte(parts[1]), expected) != 1 {
			utils.WriteError(w, http.StatusUnauthorized, "Unauthorized", "UNAUTHORIZED", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}
