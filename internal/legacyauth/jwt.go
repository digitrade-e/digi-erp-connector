// Package legacyauth implements the minimal HS256 JWT flow that
// electron-mssql-app (the Node predecessor) exposed at POST /auth/token.
//
// It exists purely for cutover compatibility: a backend that still performs the
// old credentials→JWT exchange keeps working against digi-erp-connector without
// being redeployed. Tokens the old Node app already issued verify here too, as
// long as the same secret is configured — the wire format is identical
// (jsonwebtoken defaults: HS256, base64url, {sub, iat, exp} claims).
//
// This is NOT the primary auth model. The static bearer token in
// internal/api/middleware remains the real one; see docs/legacy-compat.md.
package legacyauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var (
	ErrMalformed    = errors.New("legacyauth: malformed token")
	ErrBadAlgorithm = errors.New("legacyauth: unsupported algorithm")
	ErrSignature    = errors.New("legacyauth: signature mismatch")
	ErrExpired      = errors.New("legacyauth: token expired")
	ErrNoSecret     = errors.New("legacyauth: secret is empty")
)

// header is the fixed JOSE header both this package and jsonwebtoken emit.
type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// Claims mirrors what the Node app put in its tokens.
type Claims struct {
	Subject  string `json:"sub"`
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"`
}

// Signer issues and verifies legacy tokens with a shared HMAC secret.
type Signer struct {
	secret []byte
	// now is injectable for tests; nil means time.Now.
	now func() time.Time
}

func NewSigner(secret string) (*Signer, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, ErrNoSecret
	}
	return &Signer{secret: []byte(secret)}, nil
}

func (s *Signer) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Sign issues a token for subject valid for ttl. The returned expiry is the
// absolute deadline, which the handler reports back as expires_in seconds.
func (s *Signer) Sign(subject string, ttl time.Duration) (token string, expiresAt time.Time, err error) {
	issued := s.clock()
	expiresAt = issued.Add(ttl)

	head, err := json.Marshal(header{Alg: "HS256", Typ: "JWT"})
	if err != nil {
		return "", time.Time{}, err
	}
	body, err := json.Marshal(Claims{
		Subject:  subject,
		IssuedAt: issued.Unix(),
		Expires:  expiresAt.Unix(),
	})
	if err != nil {
		return "", time.Time{}, err
	}

	signingInput := encode(head) + "." + encode(body)
	return signingInput + "." + encode(s.mac(signingInput)), expiresAt, nil
}

// Verify checks the signature and expiry and returns the token's claims.
// It deliberately accepts any additional claims the old app may have set:
// only alg, signature and exp are enforced.
func (s *Signer) Verify(token string) (Claims, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return Claims{}, ErrMalformed
	}

	rawHead, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	var h header
	if err := json.Unmarshal(rawHead, &h); err != nil {
		return Claims{}, ErrMalformed
	}
	// Reject "none" and anything we do not actually verify.
	if h.Alg != "HS256" {
		return Claims{}, ErrBadAlgorithm
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	expected := s.mac(parts[0] + "." + parts[1])
	if !hmac.Equal(sig, expected) {
		return Claims{}, ErrSignature
	}

	rawBody, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	var claims Claims
	if err := json.Unmarshal(rawBody, &claims); err != nil {
		return Claims{}, ErrMalformed
	}

	// exp is required: an unexpiring legacy token is not something we accept.
	if claims.Expires <= 0 {
		return Claims{}, ErrMalformed
	}
	if !s.clock().Before(time.Unix(claims.Expires, 0)) {
		return Claims{}, ErrExpired
	}

	return claims, nil
}

func (s *Signer) mac(signingInput string) []byte {
	m := hmac.New(sha256.New, s.secret)
	_, _ = m.Write([]byte(signingInput))
	return m.Sum(nil)
}

func encode(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}
