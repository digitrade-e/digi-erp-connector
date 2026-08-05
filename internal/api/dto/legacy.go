package dto

// This file holds the request/response shapes of the electron-mssql-app
// compatibility surface. They intentionally reproduce the old Node app's JSON
// field-for-field (snake_case error strings included) so a backend written
// against it needs no changes. Do not "clean these up" — the shapes are a
// contract with software we do not control.

// LegacyTokenRequest is the body of POST /auth/token.
type LegacyTokenRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// LegacyTokenResponse mirrors jsonwebtoken-era output:
// {"access_token":"...","token_type":"Bearer","expires_in":1800}
type LegacyTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

// LegacyPingResponse is {"ok":true,"ts":<epoch millis>}.
type LegacyPingResponse struct {
	OK bool  `json:"ok"`
	TS int64 `json:"ts"`
}

// LegacyErrorResponse is the old app's error body: a single snake_case code in
// the "error" field, e.g. {"error":"not_found"}. This differs from
// utils.ErrorResponse on purpose — legacy routes answer in the legacy shape.
type LegacyErrorResponse struct {
	Error string `json:"error"`
}

// LegacyTestConnectionResponse is {"ok":true} / {"ok":false,"error":"..."}.
type LegacyTestConnectionResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// LegacyMSSQLOverride is the mssql block POST /api/test-connection accepts.
// Pointer fields distinguish "absent" from "explicitly false/zero" so a partial
// body merges onto the running config the way the old app merged it.
type LegacyMSSQLOverride struct {
	Server                 *string `json:"server"`
	Database               *string `json:"database"`
	User                   *string `json:"user"`
	Password               *string `json:"password"`
	Port                   *int    `json:"port"`
	Encrypt                *bool   `json:"encrypt"`
	TrustServerCertificate *bool   `json:"trustServerCertificate"`
}

// LegacyTestConnectionRequest is {"mssql":{...}}.
type LegacyTestConnectionRequest struct {
	MSSQL *LegacyMSSQLOverride `json:"mssql"`
}

// LegacyQueryRequest is the body of POST /api/query.
type LegacyQueryRequest struct {
	Query  string         `json:"sql"`
	Params map[string]any `json:"params"`
}

// LegacyQueryResponse is the old runner envelope: {"value":[...],"rowsAffected":[...]}.
type LegacyQueryResponse struct {
	Value        []map[string]any `json:"value"`
	RowsAffected []int64          `json:"rowsAffected"`
}
