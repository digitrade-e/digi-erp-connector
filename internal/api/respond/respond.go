// Package respond writes the connector's JSON responses.
//
// Every native route answers with either a payload via JSON or the standard
// error envelope via Error, so callers can rely on one error shape:
//
//	{"error": "<human message>", "code": "<MACHINE_CODE>", "details": {}}
//
// Messages are for operators and never carry raw driver or filesystem text;
// codes are what clients should branch on. The one exception is the legacy
// electron-mssql-app compatibility routes, which reproduce the old app's
// {"error":"snake_case"} bodies — see internal/api/dto.LegacyErrorResponse.
package respond

import (
	"encoding/json"
	"net/http"
)

// ErrorResponse is the standard error envelope.
type ErrorResponse struct {
	Error   string         `json:"error"`
	Code    string         `json:"code"`
	Details map[string]any `json:"details,omitempty"`
}

// Error writes the standard error envelope. code should be a stable
// SCREAMING_SNAKE_CASE identifier that clients can match on; message is for
// humans reading logs.
func Error(w http.ResponseWriter, status int, message, code string, details map[string]any) {
	if details == nil {
		details = map[string]any{}
	}
	JSON(w, status, ErrorResponse{
		Error:   message,
		Code:    code,
		Details: details,
	})
}

// JSON writes payload as UTF-8 JSON with the given status.
//
// An encoding failure cannot be reported to the client — the status line and
// headers are already on the wire — so it is deliberately discarded and the
// client sees a truncated body.
func JSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
