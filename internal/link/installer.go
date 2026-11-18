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

// secretInstallerSetup is one-way within a pairing: "done" once an installer's apply succeeded on this box, after which
// no installer writes here again, whatever config.yaml holds later (a team restore can remove it; plan 090 review).
// Reset to "no" at every redemption, with the binding.
const secretInstallerSetup = "link_installer_setup"

// InstallerSetupDone reports whether an installer's first setup succeeded on this box. An unreadable marker counts as
// done: the caller refuses.
func (s *Store) InstallerSetupDone() (bool, error) {
	b, err := s.Secrets.Get(secretInstallerSetup)
	if errors.Is(err, ErrNoSecret) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	switch string(b) {
	case "no":
		return false, nil
	case "done":
		return true, nil
	}
	return true, errors.New("link: the installer setup marker does not parse")
}

// MarkInstallerSetupDone records that an installer's first setup ran (written before it runs).
func (s *Store) MarkInstallerSetupDone() error {
	return s.Secrets.Set(secretInstallerSetup, []byte("done"))
}

// ReopenInstallerSetup takes the mark back after an installer's apply failed: the installer may run it again (the
// API-credential checks still stand).
func (s *Store) ReopenInstallerSetup() error {
	return s.Secrets.Set(secretInstallerSetup, []byte("no"))
}

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
	if err := s.Secrets.Set(secretInstallerSetup, []byte("no")); err != nil { // a new pairing, no setup yet
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
// test against the client's SQL servers (plan 090 review). They stay open after the first setup, so the installer can
// verify it; the relay ends them when the team adopts the box.
var installerReads = map[string]bool{
	"version":     true,
	CommandHashes: true,
	"link-status": true,
	"state":       true,
	"verify":      true,
	"discover":    true,
}

// installerWrites are the first setup (plan 090 D3, connector part B): apply and its check, only while config.yaml
// holds no API credential — the first apply that succeeds sets one, and every later write is the team's. Their input
// is sealed (MustBeSealed), and provision runs them with --installer: a short list of fields, the database sql-setup
// recorded, no administrator (provision.ApplyAsInstaller), refused again under its run lock on a box that has a
// credential by then.
var installerWrites = map[string]bool{
	"apply-check": true,
	"apply":       true,
}

// CodeInstallerConfigured refuses an installer's write on a box that holds an API credential (set up already).
const CodeInstallerConfigured = "installer_box_configured"

// installerBinding refuses an installer on a box not bound to it; "" when it is. verify calls it right after the
// signature, before any other refusal, so an installer learns nothing about a box that is not its own (not even that
// the client turned commands off).
func (l *Link) installerBinding(cert *linkproto.InstallerCert) string {
	bound, err := l.cfg.Store.Installer()
	switch {
	case err != nil:
		return "state_unreadable"
	case bound == "" || bound != cert.ID:
		return linkproto.CodeInstallerNotBound
	}
	return ""
}

// checkInstaller is what an installer's verified command must also pass once its stdin arrived; "" for a team command.
func (l *Link) checkInstaller(cert *linkproto.InstallerCert, env linkproto.Envelope, stdin []byte) string {
	if cert == nil {
		return ""
	}
	if r := l.installerBinding(cert); r != "" {
		return r
	}
	switch {
	case !l.cfg.DomainRoleKnown:
		return linkproto.CodeInstallerRoleUnknown
	case l.cfg.DomainRole >= 4:
		return linkproto.CodeInstallerDC
	case installerWrites[env.Command]:
		if !env.Sealed { // checkInput refused it already (MustBeSealed); kept so this gate stands on its own
			return CodeInputNotSealed
		}
		if set, err := l.apiCredentialSet(); err != nil || set {
			return CodeInstallerConfigured
		}
		if done, err := l.cfg.Store.InstallerSetupDone(); err != nil || done {
			return CodeInstallerConfigured // the first setup succeeded here already: once, whatever config.yaml says now
		}
		return ""
	case !installerReads[env.Command]:
		return linkproto.CodeInstallerNotAllowed
	case env.Sealed || !emptyInput(stdin):
		return linkproto.CodeInstallerInput
	}
	return ""
}

func (l *Link) apiCredentialSet() (bool, error) {
	if l.cfg.APICredentialSet == nil {
		return true, nil
	}
	return l.cfg.APICredentialSet()
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
