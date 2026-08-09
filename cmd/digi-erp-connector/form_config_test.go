//go:build windows

package main

// The GUI's own save-time guards. Both functions under test are deliberately
// widget-free so they can run without a window: the rest of this package needs a
// real MainWindow and is not unit-testable.

import (
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

func TestValidateLegacyCompat(t *testing.T) {
	full := config.LegacyCompatConfig{
		Enabled:     true,
		JWTUser:     "digitrade",
		JWTPassword: "123456",
		JWTSecret:   "s3cr3t",
	}

	tests := []struct {
		name        string
		legacy      config.LegacyCompatConfig
		wantErr     bool
		wantMissing []string
	}{
		{
			name:   "disabled block needs nothing",
			legacy: config.LegacyCompatConfig{Enabled: false},
		},
		{
			name:   "disabled block tolerates blanks",
			legacy: config.LegacyCompatConfig{Enabled: false, JWTUser: "", JWTSecret: ""},
		},
		{
			name:   "fully configured block passes",
			legacy: full,
		},
		{
			name:        "enabled with everything blank names all three",
			legacy:      config.LegacyCompatConfig{Enabled: true},
			wantErr:     true,
			wantMissing: []string{"JWT user", "JWT password", "JWT secret"},
		},
		{
			name:        "missing secret only",
			legacy:      config.LegacyCompatConfig{Enabled: true, JWTUser: "digitrade", JWTPassword: "123456"},
			wantErr:     true,
			wantMissing: []string{"JWT secret"},
		},
		{
			name:        "missing user only",
			legacy:      config.LegacyCompatConfig{Enabled: true, JWTPassword: "123456", JWTSecret: "s3cr3t"},
			wantErr:     true,
			wantMissing: []string{"JWT user"},
		},
		{
			// A whitespace-only credential would pass a bare != "" check and then
			// fail authentication for real, so it counts as missing.
			name:        "whitespace-only password counts as missing",
			legacy:      config.LegacyCompatConfig{Enabled: true, JWTUser: "digitrade", JWTPassword: "   ", JWTSecret: "s3cr3t"},
			wantErr:     true,
			wantMissing: []string{"JWT password"},
		},
		{
			// allowRawSQL is an independent switch; it must not affect validation.
			name:   "allowRawSQL is orthogonal",
			legacy: config.LegacyCompatConfig{Enabled: true, JWTUser: "u", JWTPassword: "p", JWTSecret: "s", AllowRawSQL: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateLegacyCompat(tt.legacy)
			if tt.wantErr && err == nil {
				t.Fatalf("expected an error, got nil")
			}
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			for _, field := range tt.wantMissing {
				if !strings.Contains(err.Error(), field) {
					t.Errorf("error should name %q, got: %v", field, err)
				}
			}
			// Fields that are present must not be reported as missing.
			for _, field := range []string{"JWT user", "JWT password", "JWT secret"} {
				if contains(tt.wantMissing, field) {
					continue
				}
				if strings.Contains(err.Error(), field) {
					t.Errorf("error should not name %q, got: %v", field, err)
				}
			}
		})
	}
}

// TestValidateLegacyCompatMatchesServer pins the GUI guard to the daemon's own
// precondition in api.NewServer: enabled plus any blank credential is refused
// there, so it must be refused here too or a save produces a config the service
// will not start on.
func TestValidateLegacyCompatMatchesServer(t *testing.T) {
	blanks := []config.LegacyCompatConfig{
		{Enabled: true, JWTUser: "", JWTPassword: "p", JWTSecret: "s"},
		{Enabled: true, JWTUser: "u", JWTPassword: "", JWTSecret: "s"},
		{Enabled: true, JWTUser: "u", JWTPassword: "p", JWTSecret: ""},
	}
	for _, legacy := range blanks {
		if err := validateLegacyCompat(legacy); err == nil {
			t.Errorf("NewServer rejects %+v, so the GUI must too", legacy)
		}
	}
}

func TestParseOptionalMinutes(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
		ok    bool
	}{
		{name: "empty means default", input: "", want: 0, ok: true},
		{name: "whitespace means default", input: "   ", want: 0, ok: true},
		{name: "the electron value", input: "30", want: 30, ok: true},
		{name: "surrounding spaces are trimmed", input: " 45 ", want: 45, ok: true},
		{name: "one minute is allowed", input: "1", want: 1, ok: true},
		{name: "one day is allowed", input: "1440", want: 1440, ok: true},
		{name: "zero is not a lifetime", input: "0", ok: false},
		{name: "negative is rejected", input: "-5", ok: false},
		{name: "beyond one day is rejected", input: "1441", ok: false},
		{name: "non-numeric is rejected", input: "30m", ok: false},
		{name: "float is rejected", input: "30.5", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseOptionalMinutes(tt.input)
			if ok != tt.ok {
				t.Fatalf("parseOptionalMinutes(%q) ok = %v, want %v", tt.input, ok, tt.ok)
			}
			if ok && got != tt.want {
				t.Errorf("parseOptionalMinutes(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
