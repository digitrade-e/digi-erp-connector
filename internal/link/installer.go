package link

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// Installers (Digitrade plan 090 D3). A project manager's key, certified by a team key, may send commands only to a
// box bound to that installer: the box records, when its pairing code is redeemed, which installer made the code
// (the relay's Welcome). A box paired from a team code, or before this release, is bound to none and refuses every
// installer, forever — re-pairing makes a new box id. Within that, an installer may only read, with no input; the
// relay ends even that once the team adopts the box (plan 090 D2).

// secretInstaller holds the installer a box is bound to, or installerNone. Kept in the secrets store (DPAPI) beside
// the box's keys, and written again at every redemption, so no binding outlives the pairing it came from.
const secretInstaller = "link_installer"

// installerNone is the stored value for a box no installer may command (a code the team made, or an old relay).
const installerNone = "none"

// Installer is the installer this box is bound to; "" when none. An unreadable binding is an error, never "none":
// the caller refuses.
func (s *Store) Installer() (string, error) {
	b, err := s.Secrets.Get(secretInstaller)
	if errors.Is(err, ErrNoSecret) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	id := string(b)
	switch {
	case id == installerNone:
		return "", nil
	case linkproto.ValidInstallerID(id):
		return id, nil
	}
	return "", errors.New("link: the stored installer binding does not parse")
}

// BindInstaller records who made the pairing code this box just redeemed: an installer's id, or "" for the team. It
// overwrites any earlier binding and reads it back.
func (s *Store) BindInstaller(id string) error {
	v := id
	if v == "" {
		v = installerNone
	} else if !linkproto.ValidInstallerID(id) {
		return fmt.Errorf("link: %q is not an installer id", id)
	}
	if err := s.Secrets.Set(secretInstaller, []byte(v)); err != nil {
		return err
	}
	back, err := s.Installer()
	if err != nil || back != id {
		return errors.New("link: the installer binding did not read back")
	}
	return nil
}

// installerReads are the commands an installer may send a box bound to it: reads that change nothing. Each takes an
// empty input, so none can carry a credential: discover then reads no database, and cannot be turned into a password
// test against the client's SQL servers (plan 090 review).
var installerReads = map[string]bool{
	"version":     true,
	CommandHashes: true,
	"link-status": true,
	"state":       true,
	"verify":      true,
	"discover":    true,
}

// checkInstaller is what an installer's verified command must also pass; "" for a team command. The binding comes
// first, so an installer learns nothing about a box that is not its own.
func (l *Link) checkInstaller(cert *linkproto.InstallerCert, env linkproto.Envelope, stdin []byte) string {
	if cert == nil {
		return ""
	}
	bound, err := l.cfg.Store.Installer()
	switch {
	case err != nil:
		return "state_unreadable"
	case bound == "" || bound != cert.ID:
		return linkproto.CodeInstallerNotBound
	case !l.cfg.DomainRoleKnown:
		return linkproto.CodeInstallerRoleUnknown
	case l.cfg.DomainRole >= 4:
		return linkproto.CodeInstallerDC
	case !installerReads[env.Command]:
		return linkproto.CodeInstallerNotAllowed
	case env.Sealed || !emptyInput(stdin):
		return linkproto.CodeInstallerInput
	}
	return ""
}

// emptyInput: nothing, white space, or an empty JSON object.
func emptyInput(stdin []byte) bool {
	t := bytes.TrimSpace(stdin)
	if len(t) == 0 {
		return true
	}
	if t[0] != '{' || t[len(t)-1] != '}' {
		return false
	}
	return len(bytes.TrimSpace(t[1:len(t)-1])) == 0
}
