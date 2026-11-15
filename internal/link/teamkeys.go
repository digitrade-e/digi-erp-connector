package link

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"

	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// The team keys this release trusts (plan 083 D3: digi-mcp's one team key, plus
// the next one for rotation). They are compiled in, so the relay cannot change
// them: a key is added or retired only by a reviewed commit and a release.
//
// CompiledMinEpoch is the lowest epoch this release accepts; a box also keeps
// the highest epoch it has seen (Store.MinEpoch), so a downgrade cannot bring
// a retired key back (§8 A17).
//
// team-1 was made by Ilya on 2026-11-15 in his own terminal; its private half
// is digi-mcp's Keychain slot link/prod/team-1 (plan 083 D3). The relay's helm
// values carry the same public key (connector-link teamKeys).
var teamKeys = []struct {
	ID    string
	Epoch int
	Pub   string // base64 Ed25519 public key
}{
	{ID: "team-1", Epoch: 1, Pub: "KGQUy5wa5R7V54GWKUcZGuWcPNqIQRTggZuSFH699Wg="},
}

const CompiledMinEpoch = 1

// TeamKeyring is the compiled-in keyring.
func TeamKeyring() (linkproto.Keyring, error) {
	var kr linkproto.Keyring
	for _, k := range teamKeys {
		pub, err := base64.StdEncoding.DecodeString(k.Pub)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("link: team key %s is not an Ed25519 public key", k.ID)
		}
		kr = append(kr, linkproto.TeamKey{ID: k.ID, Epoch: k.Epoch, Pub: pub})
	}
	return kr, nil
}
