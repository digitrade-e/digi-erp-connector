// Package auth issues and verifies the short-lived tokens handed out by
// POST /auth/token.
//
// The connector accepts two credentials, and this is one of them: a caller
// exchanges a username and password for an HS256 JWT and then presents that JWT
// as a bearer token. The other is the static token in config (see
// internal/api/middleware). Both are first-class; see docs/authentication.md.
//
// The wire format matches what jsonwebtoken produces — HS256, base64url,
// {sub, iat, exp} — because the backends that use this exchange were written
// against a Node service. That compatibility is deliberate and costs nothing.
//
// What is NOT inherited from that service is its credentials. The signing secret
// is generated per installation (never a constant compiled into a binary, which
// would let anyone holding the source mint valid tokens), and the username and
// password are set by the operator.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrMalformed    = errors.New("auth: malformed token")
	ErrBadAlgorithm = errors.New("auth: unsupported algorithm")
	ErrSignature    = errors.New("auth: signature mismatch")
	ErrExpired      = errors.New("auth: token expired")
	ErrNoSecret     = errors.New("auth: secret is empty")
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

// secretBytes is the size of a generated signing secret. 32 bytes is the HMAC
// block size for SHA-256: shorter adds nothing, longer is hashed down anyway.
const secretBytes = 32

// NewSecret returns a fresh random signing secret, hex encoded.
//
// Every installation generates its own on first run. The predecessor shipped a
// constant in its source, which meant anyone who had seen that source could mint
// a token accepted by every deployment without knowing any password — the single
// worst property of the old scheme, and the reason a default is never provided
// here.
func NewSecret() (string, error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: generate signing secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// NewPassword returns a random password suitable for the exchange.
//
// Generated rather than chosen so installations do not end up sharing one, and
// long enough that the rate limiter is not the only thing standing between an
// attacker and a guess.
func NewPassword() (string, error) {
	b := make([]byte, 18) // 24 base64url characters, ~144 bits
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: generate password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
