package linkproto

import (
	"crypto/rand"
	"errors"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"
)

// Seal encrypts msg to a box's X25519 public key (an anonymous sealed box): only
// that box can open it — not the relay, not a TLS-inspecting proxy, not a log
// (plan 083 §3.3). digi-mcp seals a command's stdin when it carries a secret.
func Seal(to [32]byte, msg []byte) ([]byte, error) {
	return box.SealAnonymous(nil, msg, &to, rand.Reader)
}

// Open decrypts what Seal made for this box.
func Open(k BoxKeys, sealed []byte) ([]byte, error) {
	pub := k.Public().Seal
	out, ok := box.OpenAnonymous(nil, sealed, &pub, k.Seal)
	if !ok {
		return nil, errors.New("linkproto: sealed input does not open with this box's key")
	}
	return out, nil
}

func publicFromPrivate(priv *[32]byte) ([32]byte, error) {
	var pub [32]byte
	p, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		return pub, err
	}
	copy(pub[:], p)
	return pub, nil
}
