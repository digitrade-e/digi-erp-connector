package config

import (
	"strings"
	"testing"
	"time"
)

// Validate is the one precondition the daemon and the GUI share. If it stops
// rejecting a blank credential, the GUI can save a config the service will not
// start on — and the exchange would accept empty strings, which is an open door
// rather than an inconvenience.
func TestAuthConfigValidate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     AuthConfig
		wantErr string
	}{
		{"complete", AuthConfig{Username: "bfl", Password: "pw"}, ""},
		{"empty", AuthConfig{}, "username"},
		{"no username", AuthConfig{Password: "pw"}, "username"},
		{"no password", AuthConfig{Username: "bfl"}, "password"},
		{"whitespace is not a password", AuthConfig{Username: "bfl", Password: "   "}, "password"},
		{"whitespace is not a username", AuthConfig{Username: "\t", Password: "pw"}, "username"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("Validate() = nil, want an error naming %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("error %q should name the missing setting %q", err, tc.wantErr)
			}
		})
	}
}

// A malformed lifetime falls back rather than failing startup, but the GUI must
// still be able to tell the operator about it.
func TestAuthConfigTokenTTL(t *testing.T) {
	for _, tc := range []struct {
		in    string
		want  time.Duration
		valid bool
	}{
		{"", DefaultTokenTTL, true},
		{"   ", DefaultTokenTTL, true},
		{"30m", 30 * time.Minute, true},
		{"1h", time.Hour, true},
		{"30 minutes", DefaultTokenTTL, false},
		{"-5m", DefaultTokenTTL, false},
		{"0s", DefaultTokenTTL, false},
	} {
		a := AuthConfig{TokenTTL: tc.in}
		if got := a.TTL(); got != tc.want {
			t.Errorf("TTL(%q) = %v, want %v", tc.in, got, tc.want)
		}
		if got := a.TokenTTLValid(); got != tc.valid {
			t.Errorf("TokenTTLValid(%q) = %v, want %v", tc.in, got, tc.valid)
		}
	}
}
