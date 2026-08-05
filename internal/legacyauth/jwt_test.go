package legacyauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const testSecret = "DIGITRADE_DEVEPOPMENT_MSSQL"

func TestSignVerifyRoundTrip(t *testing.T) {
	s, err := NewSigner(testSecret)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	token, expiresAt, err := s.Sign("digitrade", 30*time.Minute)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if strings.Count(token, ".") != 2 {
		t.Fatalf("token is not three dot-separated parts: %q", token)
	}

	claims, err := s.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Subject != "digitrade" {
		t.Errorf("sub = %q, want digitrade", claims.Subject)
	}
	if claims.Expires != expiresAt.Unix() {
		t.Errorf("exp = %d, want %d", claims.Expires, expiresAt.Unix())
	}
	if claims.IssuedAt == 0 {
		t.Error("iat not set")
	}
}

// The wire format must match jsonwebtoken's so tokens the old Node app issued
// still verify: HS256/JWT header, base64url without padding.
func TestTokenWireFormat(t *testing.T) {
	s, _ := NewSigner(testSecret)
	token, _, _ := s.Sign("digitrade", time.Minute)

	if strings.Contains(token, "=") {
		t.Errorf("token contains base64 padding: %q", token)
	}

	rawHead, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[0])
	if err != nil {
		t.Fatalf("header not base64url: %v", err)
	}
	var h header
	if err := json.Unmarshal(rawHead, &h); err != nil {
		t.Fatalf("header not JSON: %v", err)
	}
	if h.Alg != "HS256" || h.Typ != "JWT" {
		t.Errorf("header = %+v, want {HS256 JWT}", h)
	}
}

func TestVerifyRejects(t *testing.T) {
	s, _ := NewSigner(testSecret)
	valid, _, _ := s.Sign("digitrade", time.Minute)
	parts := strings.Split(valid, ".")

	// A token signed with a different secret must not verify.
	other, _ := NewSigner("some-other-secret")
	foreign, _, _ := other.Sign("digitrade", time.Minute)

	// alg:none with the signature stripped — the classic JWT bypass.
	noneHead := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	algNone := noneHead + "." + parts[1] + "."

	tests := []struct {
		name  string
		token string
		want  error
	}{
		{"empty", "", ErrMalformed},
		{"two parts", parts[0] + "." + parts[1], ErrMalformed},
		{"tampered payload", parts[0] + ".ZXZpbA." + parts[2], ErrSignature},
		{"tampered signature", parts[0] + "." + parts[1] + ".AAAA", ErrSignature},
		{"foreign secret", foreign, ErrSignature},
		{"alg none", algNone, ErrBadAlgorithm},
		{"static bearer token", "not-a-jwt-at-all", ErrMalformed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.Verify(tc.token); !errors.Is(err, tc.want) {
				t.Errorf("Verify(%q) = %v, want %v", tc.token, err, tc.want)
			}
		})
	}
}

func TestVerifyExpiry(t *testing.T) {
	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	s, _ := NewSigner(testSecret)
	s.now = func() time.Time { return base }

	token, _, err := s.Sign("digitrade", 30*time.Minute)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	// One second before expiry: still valid.
	s.now = func() time.Time { return base.Add(30*time.Minute - time.Second) }
	if _, err := s.Verify(token); err != nil {
		t.Errorf("token should still be valid: %v", err)
	}

	// At and past expiry: rejected.
	s.now = func() time.Time { return base.Add(30 * time.Minute) }
	if _, err := s.Verify(token); !errors.Is(err, ErrExpired) {
		t.Errorf("at expiry: got %v, want ErrExpired", err)
	}
	s.now = func() time.Time { return base.Add(time.Hour) }
	if _, err := s.Verify(token); !errors.Is(err, ErrExpired) {
		t.Errorf("past expiry: got %v, want ErrExpired", err)
	}
}

// A token with no exp claim is refused rather than treated as eternal.
func TestVerifyRequiresExpiry(t *testing.T) {
	s, _ := NewSigner(testSecret)
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"digitrade"}`))
	signingInput := head + "." + body
	token := signingInput + "." + base64.RawURLEncoding.EncodeToString(s.mac(signingInput))

	if _, err := s.Verify(token); !errors.Is(err, ErrMalformed) {
		t.Errorf("token without exp: got %v, want ErrMalformed", err)
	}
}

func TestNewSignerRequiresSecret(t *testing.T) {
	for _, secret := range []string{"", "   "} {
		if _, err := NewSigner(secret); !errors.Is(err, ErrNoSecret) {
			t.Errorf("NewSigner(%q) = %v, want ErrNoSecret", secret, err)
		}
	}
}
