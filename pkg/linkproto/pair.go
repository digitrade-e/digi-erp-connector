package linkproto

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
)

// A pairing code is what a human types into setup.exe: ten symbols of
// Crockford's base32 (50 bits), shown as XXXXX-XXXXX. It is single use and
// short lived (the relay's rule, plan 083 §8 A8), and the relay stores only its
// hash.
const (
	pairAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	pairLen      = 10
)

// NewPairCode makes a code from r (crypto/rand.Reader in production).
func NewPairCode(r io.Reader) (string, error) {
	b := make([]byte, pairLen)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	out := make([]byte, 0, pairLen+1)
	for i, x := range b {
		if i == pairLen/2 {
			out = append(out, '-')
		}
		out = append(out, pairAlphabet[int(x)%len(pairAlphabet)]) // 256 % 32 == 0: no bias
	}
	return string(out), nil
}

// NormalizePairCode accepts what a person types: any case, spaces or dashes,
// O for 0 and I or L for 1.
func NormalizePairCode(s string) (string, error) {
	var b strings.Builder
	for _, c := range strings.ToUpper(s) {
		switch {
		case c == '-' || c == ' ':
			continue
		case c == 'O':
			c = '0'
		case c == 'I' || c == 'L':
			c = '1'
		}
		if !strings.ContainsRune(pairAlphabet, c) {
			return "", refuse(CodeBadPairCode)
		}
		b.WriteRune(c)
	}
	if b.Len() != pairLen {
		return "", refuse(CodeBadPairCode)
	}
	return b.String(), nil
}

// PairCodeHash is what the relay stores and looks a code up by.
func PairCodeHash(normalized string) string {
	s := sha256.Sum256([]byte("digi-link-pair-v1\x00" + normalized))
	return hex.EncodeToString(s[:])
}
