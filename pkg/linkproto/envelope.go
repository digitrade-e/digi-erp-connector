package linkproto

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"time"
)

// Bounds on a signed request's life (plan 083 §3.3, §8 A17).
const (
	MaxLifetime  = 5 * time.Minute
	MaxClockSkew = 2 * time.Minute
)

// TeamKey is one of digi-mcp's signing keys (plan 083 D3: one team key, plus
// the next one for rotation). A box's release compiles its Keyring in; the
// relay reads the same public keys from its config.
type TeamKey struct {
	ID    string
	Epoch int
	Pub   ed25519.PublicKey
}

// Keyring is the set of team keys a verifier trusts.
type Keyring []TeamKey

func (kr Keyring) find(id string) (TeamKey, bool) {
	for _, k := range kr {
		if k.ID == id {
			return k, true
		}
	}
	return TeamKey{}, false
}

// Signed carries a signed JSON body: the signature covers the exact bytes of
// Body, so nothing is re-encoded between signer and verifier.
//
// Cert is set only on what an installer signed (plan 090 D1): its team-signed
// certificate, and Key is then the installer's id. Every team-key check refuses
// a Signed carrying one; a box before plan 090 ignores the field and finds no
// team key of that id (key_unknown).
type Signed struct {
	Key  string          `json:"key"`
	Body json.RawMessage `json:"body"`
	Sig  string          `json:"sig"`
	Cert *Signed         `json:"cert,omitempty"`
}

// Envelope is one command for one box (plan 083 §3.3).
type Envelope struct {
	V           int      `json:"v"`
	Box         string   `json:"box"`
	Command     string   `json:"command"`        // one of the box's fixed commands
	Args        []string `json:"args,omitempty"` // shape-checked by the box per command
	StdinSHA256 string   `json:"stdinSha256"`    // of the stdin as it crosses (sealed bytes when Sealed)
	Sealed      bool     `json:"sealed,omitempty"`
	Nonce       string   `json:"nonce"`
	IssuedAt    int64    `json:"iat"`
	ExpiresAt   int64    `json:"exp"`
	ChangeSet   string   `json:"changeSet,omitempty"`
	By          string   `json:"by,omitempty"` // the person, as digi-mcp knows them; not authenticated
}

// AdminRequest is a signed call to the relay's own operations: pairing and the
// box list. It binds the HTTP body by its hash.
type AdminRequest struct {
	V          int    `json:"v"`
	Op         string `json:"op"`
	BodySHA256 string `json:"bodySha256"`
	Nonce      string `json:"nonce"`
	IssuedAt   int64  `json:"iat"`
	ExpiresAt  int64  `json:"exp"`
	By         string `json:"by,omitempty"`
}

// The relay's admin operations.
const (
	OpPairCreate  = "pair.create"
	OpPairConfirm = "pair.confirm"
	OpBoxesList   = "boxes.list"
)

const (
	domainEnvelope = "digi-link-envelope-v1\x00"
	domainAdmin    = "digi-link-admin-v1\x00"
	domainAnswer   = "digi-link-answer-v1\x00"
)

var (
	nonceShape   = regexp.MustCompile(`^[A-Za-z0-9+/]{22,44}={0,2}$`)
	sha256Shape  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	commandShape = regexp.MustCompile(`^[a-z][a-z-]{1,30}$`)
)

// SHA256Hex is the digest form every "…Sha256" field carries.
func SHA256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func sign(priv ed25519.PrivateKey, keyID, domain string, v any) (Signed, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return Signed{}, err
	}
	sig := ed25519.Sign(priv, append([]byte(domain), body...))
	return Signed{Key: keyID, Body: body, Sig: base64.StdEncoding.EncodeToString(sig)}, nil
}

func (kr Keyring) verify(s Signed, domain string, minEpoch int) (TeamKey, error) {
	if s.Cert != nil {
		return TeamKey{}, refuse(CodeMalformed) // an installer's signature never passes as a team key's (plan 090)
	}
	k, ok := kr.find(s.Key)
	if !ok {
		return TeamKey{}, refuse(CodeKeyUnknown)
	}
	if k.Epoch < minEpoch {
		return TeamKey{}, refuse(CodeKeyRevoked)
	}
	sig, err := base64.StdEncoding.DecodeString(s.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize || !ed25519.Verify(k.Pub, append([]byte(domain), s.Body...), sig) {
		return TeamKey{}, refuse(CodeBadSignature)
	}
	return k, nil
}

func checkTimes(iat, exp int64, now time.Time) error {
	issued, expires := time.Unix(iat, 0), time.Unix(exp, 0)
	switch {
	case issued.After(now.Add(MaxClockSkew)):
		return refuse(CodeFromFuture)
	case !expires.After(now):
		return refuse(CodeExpired)
	case expires.Sub(issued) > MaxLifetime:
		return refuse(CodeLifetime)
	}
	return nil
}

// SignEnvelope signs e with a team key (digi-mcp's side).
func SignEnvelope(priv ed25519.PrivateKey, keyID string, e Envelope) (Signed, error) {
	e.V = Version
	return sign(priv, keyID, domainEnvelope, e)
}

// VerifyEnvelope checks a command for box at time now: a trusted key at or above
// minEpoch, the signature, the shape, the box id and the times. The nonce's
// novelty is the caller's to check (the box keeps the nonces it has seen until
// they expire).
func (kr Keyring) VerifyEnvelope(s Signed, now time.Time, box string, minEpoch int) (Envelope, TeamKey, error) {
	k, err := kr.verify(s, domainEnvelope, minEpoch)
	if err != nil {
		return Envelope{}, TeamKey{}, err
	}
	e, err := decodeEnvelope(s.Body, now, box)
	if err != nil {
		return Envelope{}, TeamKey{}, err
	}
	return e, k, nil
}

// decodeEnvelope checks a verified envelope body: strict decoding, the version, the shapes, the box id and the times.
func decodeEnvelope(body []byte, now time.Time, box string) (Envelope, error) {
	var e Envelope
	if err := strictUnmarshal(body, &e); err != nil {
		return Envelope{}, err
	}
	switch {
	case e.V != Version:
		return Envelope{}, refuse(CodeVersion)
	case !commandShape.MatchString(e.Command), !nonceShape.MatchString(e.Nonce), !sha256Shape.MatchString(e.StdinSHA256):
		return Envelope{}, refuse(CodeMalformed)
	case e.Box != box:
		return Envelope{}, refuse(CodeWrongBox)
	}
	if err := checkTimes(e.IssuedAt, e.ExpiresAt, now); err != nil {
		return Envelope{}, err
	}
	return e, nil
}

// SignAdmin signs a request to the relay's operation op over body.
func SignAdmin(priv ed25519.PrivateKey, keyID string, r AdminRequest, body []byte) (Signed, error) {
	r.V = Version
	r.BodySHA256 = SHA256Hex(body)
	return sign(priv, keyID, domainAdmin, r)
}

// VerifyAdmin checks a request to op carrying body.
func (kr Keyring) VerifyAdmin(s Signed, now time.Time, op string, body []byte, minEpoch int) (AdminRequest, TeamKey, error) {
	k, err := kr.verify(s, domainAdmin, minEpoch)
	if err != nil {
		return AdminRequest{}, TeamKey{}, err
	}
	r, err := decodeAdmin(s.Body, now, op, body)
	if err != nil {
		return AdminRequest{}, TeamKey{}, err
	}
	return r, k, nil
}

// decodeAdmin checks a verified admin request body: strict decoding, the version, the nonce's shape, the operation,
// the HTTP body's digest and the times.
func decodeAdmin(raw []byte, now time.Time, op string, body []byte) (AdminRequest, error) {
	var r AdminRequest
	if err := strictUnmarshal(raw, &r); err != nil {
		return AdminRequest{}, err
	}
	switch {
	case r.V != Version:
		return AdminRequest{}, refuse(CodeVersion)
	case !nonceShape.MatchString(r.Nonce):
		return AdminRequest{}, refuse(CodeMalformed)
	case r.Op != op:
		return AdminRequest{}, refuse(CodeWrongOp)
	case r.BodySHA256 != SHA256Hex(body):
		return AdminRequest{}, refuse(CodeBodyMismatch)
	}
	if err := checkTimes(r.IssuedAt, r.ExpiresAt, now); err != nil {
		return AdminRequest{}, err
	}
	return r, nil
}

// Answer is the box's signed account of one command (plan 083 §8 A11): the relay
// carries it and cannot forge it.
type Answer struct {
	V     int    `json:"v"`
	Box   string `json:"box"`
	Nonce string `json:"nonce"` // the envelope's, once its signature verified; "" for a refusal before that
	// Env is the SHA-256 of the exact envelope bytes the box verified (EnvelopeDigest): the answer is bound to that
	// command, not only to its nonce, so a relay cannot pass off the answer to another delivery as this one's.
	Env          string `json:"env,omitempty"`
	Exit         int    `json:"exit"`
	StdoutSHA256 string `json:"stdoutSha256"`
	StdoutLen    int64  `json:"stdoutLen"`
	Refusal      string `json:"refusal,omitempty"` // the box refused the envelope; nothing ran
	Started      int64  `json:"started,omitempty"`
	Finished     int64  `json:"finished,omitempty"`
}

// SignedAnswer is an Answer signed by the box's own key.
type SignedAnswer struct {
	Body json.RawMessage `json:"body"`
	Sig  string          `json:"sig"`
}

// SignAnswer signs a with the box's key.
func SignAnswer(k BoxKeys, a Answer) (SignedAnswer, error) {
	a.V = Version
	body, err := json.Marshal(a)
	if err != nil {
		return SignedAnswer{}, err
	}
	sig := ed25519.Sign(k.Sign, append([]byte(domainAnswer), body...))
	return SignedAnswer{Body: body, Sig: base64.StdEncoding.EncodeToString(sig)}, nil
}

// EnvelopeDigest is the digest an answer binds: the exact signed envelope body.
func EnvelopeDigest(s Signed) string { return SHA256Hex(s.Body) }

// VerifyAnswer checks an answer against the box's pinned key, its id, the
// nonce and the digest (EnvelopeDigest) of the envelope it answers; stdout is
// checked against its digest. An answer the box gave before it verified the
// envelope names neither, and is refused here: it says nothing about this
// command.
func VerifyAnswer(pub BoxPublic, s SignedAnswer, box, nonce, env string, stdout []byte) (Answer, error) {
	sig, err := base64.StdEncoding.DecodeString(s.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize || !ed25519.Verify(pub.Sign, append([]byte(domainAnswer), s.Body...), sig) {
		return Answer{}, refuse(CodeBadSignature)
	}
	var a Answer
	if err := strictUnmarshal(s.Body, &a); err != nil {
		return Answer{}, err
	}
	switch {
	case a.V != Version:
		return Answer{}, refuse(CodeVersion)
	case a.Box != box:
		return Answer{}, refuse(CodeWrongBox)
	case a.Nonce != nonce || a.Env == "" || a.Env != env:
		return Answer{}, refuse(CodeWrongNonce)
	case a.StdoutSHA256 != SHA256Hex(stdout) || a.StdoutLen != int64(len(stdout)):
		return Answer{}, refuse(CodeBodyMismatch)
	}
	return a, nil
}

// strictUnmarshal refuses unknown fields and trailing data.
func strictUnmarshal(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return refuse(CodeMalformed)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return refuse(CodeMalformed) // trailing data
	}
	return nil
}
