package link

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"

	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// The shell keys this release trusts (plan 094). Apart from the team keys on purpose: a team key — whose private half
// sits in a store several people can read — can never run, enable or widen the shell, and a shell key runs nothing
// but the shell. They are compiled in like the team keys, with their own epoch floor (Store.MinShellEpochStrict), so
// a release retires a leaked one and a downgrade cannot bring it back.
//
// shell-1 is the key in use, its private half only in its holder's own keychain (never a shared store); shell-2 is
// the spare, its private half offline, so a lost or leaked shell-1 is retired by the next release without a gap.
//
// An empty Pub is a key not set yet: it is skipped, and with no key set no shell command verifies.
var shellKeys = []struct {
	ID    string
	Epoch int
	Pub   string // base64 Ed25519 public key
}{
	// shell-1: made by Ilya on 2026-11-22 in his own terminal; its private half is digi-mcp's local keychain slot
	// link/prod/shell-1 only (never a shared store).
	{ID: "shell-1", Epoch: 1, Pub: "q13mDiBzz9cr/hFDGd7NP9mW/lb0s1BYH05taN3wU3M="},
	// shell-2: not made yet (no offline medium on 2026-11-22). Until it is, a lost or leaked shell-1 has no spare: the
	// brake is each box's own `digi-erp-connector-link shell off` (or link.yaml admin: false), then a release that
	// raises CompiledMinShellEpoch with a new key.
	{ID: "shell-2", Epoch: 2, Pub: ""}, // TODO(plan 094): the offline spare's public half
}

// CompiledMinShellEpoch is the lowest shell-key epoch this release accepts.
const CompiledMinShellEpoch = 1

// ShellKeyring is the compiled-in shell keyring, keys not set yet left out.
func ShellKeyring() (linkproto.Keyring, error) {
	var kr linkproto.Keyring
	for _, k := range shellKeys {
		if k.Pub == "" {
			continue
		}
		pub, err := base64.StdEncoding.DecodeString(k.Pub)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("link: shell key %s is not an Ed25519 public key", k.ID)
		}
		kr = append(kr, linkproto.TeamKey{ID: k.ID, Epoch: k.Epoch, Pub: pub})
	}
	return kr, nil
}
