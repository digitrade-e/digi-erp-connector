package main

import (
	"errors"
	"testing"
)

// A config with no DB port — a named instance through the SQL Server Browser,
// or no database — must load into a form that can be saved again: the field
// is empty, never "0", which parsePort refuses.
func TestPortText(t *testing.T) {
	tests := []struct {
		port int
		want string
	}{
		{0, ""},
		{-1, ""},
		{1433, "1433"},
		{63501, "63501"},
	}
	for _, tc := range tests {
		if got := portText(tc.port); got != tc.want {
			t.Errorf("portText(%d) = %q, want %q", tc.port, got, tc.want)
		}
	}
}

// Test connection / Test user with the field blank use the stored password,
// the one Save keeps (plan 086 C10); a typed one wins; neither is "".
func TestTypedOrStored(t *testing.T) {
	stored := func() ([]byte, error) { return []byte("stored-pw"), nil }
	none := func() ([]byte, error) { return nil, errors.New("not found") }
	tests := []struct {
		typed  string
		stored func() ([]byte, error)
		want   string
	}{
		{"", stored, "stored-pw"},
		{"typed-pw", stored, "typed-pw"},
		{"", none, ""},
		{"typed-pw", none, "typed-pw"},
	}
	for _, tc := range tests {
		if got := typedOrStored(tc.typed, tc.stored); got != tc.want {
			t.Errorf("typedOrStored(%q) = %q, want %q", tc.typed, got, tc.want)
		}
	}
}
