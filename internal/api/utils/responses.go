package utils

import (
	"encoding/json"
	"net/http"
)

type ErrorResponse struct {
	Error   string         `json:"error"`
	Code    string         `json:"code"`
	Details map[string]any `json:"details,omitempty"`
}

func WriteError(w http.ResponseWriter, status int, message, code string, details map[string]any) {
	if details == nil {
		details = map[string]any{}
	}
	WriteJSON(w, status, ErrorResponse{
		Error:   message,
		Code:    code,
		Details: details,
	})
}

func WriteNotImplemented(w http.ResponseWriter) {
	WriteError(w, http.StatusNotImplemented, "Not implemented", "NOT_IMPLEMENTED", nil)
}

func WriteJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
