package linkproto

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"regexp"
)

// Challenge is the relay's first message on a new box connection.
type Challenge struct {
	V     int    `json:"v"`
	Nonce string `json:"nonce"` // base64 of 32 random bytes
	Relay string `json:"relay"` // the relay instance's id: binds the signature to this connection
}

// NewChallenge makes a fresh challenge for relay instance relayID.
func NewChallenge(r io.Reader, relayID string) (Challenge, error) {
	n := make([]byte, 32)
	if _, err := io.ReadFull(r, n); err != nil {
		return Challenge{}, err
	}
	return Challenge{V: Version, Nonce: base64.StdEncoding.EncodeToString(n), Relay: relayID}, nil
}

// Hello is the box's answer. A paired box names its id; a box pairing for the
// first time names no id and carries the code a human typed (PairCode).
type Hello struct {
	V        int    `json:"v"`
	Box      string `json:"box,omitempty"`
	PairCode string `json:"pairCode,omitempty"`
	Sign     string `json:"sign"`  // base64 Ed25519 public key
	Seal     string `json:"seal"`  // base64 X25519 public key
	Build    string `json:"build"` // the link's version, for the registry
	Host     string `json:"host,omitempty"`
	Domain   string `json:"domain,omitempty"`
	// DomainRole is Win32_ComputerSystem.DomainRole (4 or 5: a domain
	// controller); shown to the human who confirms a pairing.
	DomainRole int    `json:"domainRole,omitempty"`
	Sig        string `json:"sig"`
}

// Welcome is the relay's verdict on a Hello.
type Welcome struct {
	V     int    `json:"v"`
	OK    bool   `json:"ok"`
	Code  string `json:"code,omitempty"`  // a refusal code, or CodePending after a redeemed pair code
	Box   string `json:"box,omitempty"`   // the box id (assigned at pairing)
	Epoch int64  `json:"epoch,omitempty"` // this connection's epoch, for the liveness check
	Relay string `json:"relay,omitempty"`
}

var boxIDShape = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,62}$`)

// ValidBoxID reports whether id has the shape of a box id (it is also a URL
// path segment: /c/<boxId>/...).
func ValidBoxID(id string) bool { return boxIDShape.MatchString(id) }

func helloBytes(h Hello, ch Challenge) []byte {
	parts := []string{"digi-link-hello-v1", ch.Nonce, ch.Relay, h.Box, h.PairCode, h.Sign, h.Seal, h.Build, h.Host, h.Domain}
	var b []byte
	for _, p := range parts {
		b = append(b, p...)
		b = append(b, 0)
	}
	return append(b, byte(h.DomainRole))
}

// SignHello fills h's version, public keys and signature for challenge ch.
func SignHello(k BoxKeys, h *Hello, ch Challenge) {
	pub := k.Public()
	h.V = Version
	h.Sign = pub.SignB64()
	h.Seal = pub.SealB64()
	h.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(k.Sign, helloBytes(*h, ch)))
}

// VerifyHello checks that h answers ch and was signed by the key it names. It
// does not decide whether that key is the box's pinned one: the caller does.
func VerifyHello(h Hello, ch Challenge) (BoxPublic, error) {
	if h.V != Version {
		return BoxPublic{}, refuse(CodeVersion)
	}
	if (h.Box == "") == (h.PairCode == "") || (h.Box != "" && !ValidBoxID(h.Box)) {
		return BoxPublic{}, refuse(CodeMalformed)
	}
	pub, err := ParseBoxPublic(h.Sign, h.Seal)
	if err != nil {
		return BoxPublic{}, err
	}
	sig, err := base64.StdEncoding.DecodeString(h.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize || !ed25519.Verify(pub.Sign, helloBytes(h, ch), sig) {
		return BoxPublic{}, refuse(CodeBadSignature)
	}
	return pub, nil
}

// RandomNonce returns base64 of n random bytes from crypto/rand.
func RandomNonce(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return base64.StdEncoding.EncodeToString(b)
}
