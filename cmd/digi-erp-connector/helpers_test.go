package main

import "testing"

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
