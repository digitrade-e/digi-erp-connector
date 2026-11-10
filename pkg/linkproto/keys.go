package linkproto

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"io"
	"strings"

	"golang.org/x/crypto/nacl/box"
)

// BoxKeys are a box's two private keys, born on the box: Ed25519 to prove who
// it is and sign its answers, X25519 to open what digi-mcp seals to it.
type BoxKeys struct {
	Sign ed25519.PrivateKey
	Seal *[32]byte
}

// BoxPublic is what leaves the box: the halves the relay routes by and
// digi-mcp pins (plan 083 §8 A5).
type BoxPublic struct {
	Sign ed25519.PublicKey
	Seal [32]byte
}

// NewBoxKeys makes a box's key pair from rand (crypto/rand.Reader in production).
func NewBoxKeys(rand io.Reader) (BoxKeys, error) {
	_, signPriv, err := ed25519.GenerateKey(rand)
	if err != nil {
		return BoxKeys{}, err
	}
	_, sealPriv, err := box.GenerateKey(rand)
	if err != nil {
		return BoxKeys{}, err
	}
	return BoxKeys{Sign: signPriv, Seal: sealPriv}, nil
}

// Public returns the public halves.
func (k BoxKeys) Public() BoxPublic {
	var pub BoxPublic
	pub.Sign = k.Sign.Public().(ed25519.PublicKey)
	sealPub, _ := publicFromPrivate(k.Seal)
	pub.Seal = sealPub
	return pub
}

// Marshal encodes the private keys for the box's own store (DPAPI on Windows).
// The result is a secret.
func (k BoxKeys) Marshal() []byte {
	b := make([]byte, 0, ed25519.SeedSize+32)
	b = append(b, k.Sign.Seed()...)
	b = append(b, k.Seal[:]...)
	return b
}

// ParseBoxKeys reverses Marshal.
func ParseBoxKeys(b []byte) (BoxKeys, error) {
	if len(b) != ed25519.SeedSize+32 {
		return BoxKeys{}, errors.New("linkproto: box keys have the wrong length")
	}
	var seal [32]byte
	copy(seal[:], b[ed25519.SeedSize:])
	return BoxKeys{Sign: ed25519.NewKeyFromSeed(b[:ed25519.SeedSize]), Seal: &seal}, nil
}

// SignB64 and SealB64 are the public keys as they travel in JSON.
func (p BoxPublic) SignB64() string { return base64.StdEncoding.EncodeToString(p.Sign) }
func (p BoxPublic) SealB64() string { return base64.StdEncoding.EncodeToString(p.Seal[:]) }

// Equal reports whether both halves are the same.
func (p BoxPublic) Equal(o BoxPublic) bool {
	return p.Sign.Equal(o.Sign) && p.Seal == o.Seal
}

// Fingerprint is what a human compares: setup.exe shows it on the server, and
// digi-mcp shows the one the relay saw before a human confirms the pairing
// (plan 083 §8 A8). 100 bits of SHA-256 over both public keys, in five groups.
func (p BoxPublic) Fingerprint() string {
	h := sha256.New()
	h.Write([]byte("digi-link-fingerprint-v1\x00"))
	h.Write(p.Sign)
	h.Write(p.Seal[:])
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(h.Sum(nil))[:20]
	groups := make([]string, 0, 5)
	for i := 0; i < len(enc); i += 4 {
		groups = append(groups, enc[i:i+4])
	}
	return strings.Join(groups, "-")
}

// ParseBoxPublic decodes the two base64 public keys of a Hello.
func ParseBoxPublic(signB64, sealB64 string) (BoxPublic, error) {
	sign, err := base64.StdEncoding.DecodeString(signB64)
	if err != nil || len(sign) != ed25519.PublicKeySize {
		return BoxPublic{}, &Refusal{Code: CodeBadKey}
	}
	seal, err := base64.StdEncoding.DecodeString(sealB64)
	if err != nil || len(seal) != 32 {
		return BoxPublic{}, &Refusal{Code: CodeBadKey}
	}
	var p BoxPublic
	p.Sign = ed25519.PublicKey(sign)
	copy(p.Seal[:], seal)
	return p, nil
}
