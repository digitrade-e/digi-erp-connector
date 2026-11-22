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
	{ID: "shell-1", Epoch: 1, Pub: ""}, // TODO(plan 094): Ilya's shell-1 public half
	{ID: "shell-2", Epoch: 2, Pub: ""}, // TODO(plan 094): Ilya's shell-2 public half (the offline spare)
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
