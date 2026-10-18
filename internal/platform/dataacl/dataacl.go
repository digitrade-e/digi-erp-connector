// Package dataacl locks the connector's data directory down on Windows.
//
// %ProgramData%'s default ACL lets every local user read what is created
// under it. The data directory holds config.yaml (the auth credential and
// signing secret, the DB user), the DPAPI-sealed secrets and — once receipts
// are on — the receipt index and history: customers' names, phones, emails,
// card digits and confirmation codes. ProtectDir gives it a PROTECTED DACL
// (nothing inherited from %ProgramData%) granting Full Control to exactly
// SYSTEM, BUILTIN\Administrators and the account the process runs as, with
// container and object inheritance, so every file and folder under it —
// already there or created later — carries the same ACEs.
//
// A fourth ACE names OWNER RIGHTS (S-1-3-4) with READ_CONTROL only. Without it
// the owner of an object — whoever created the directory, possibly a plain
// user, since Users may create folders in %ProgramData% — keeps an implicit
// READ_CONTROL and WRITE_DAC whatever the DACL says, so could simply grant
// itself access again. With it the owner's implicit rights are exactly what the
// ACE grants. READ_CONTROL is kept (reading who has access reveals nothing the
// DACL protects, and tools such as icacls and Explorer need it to show the
// owner its own object); WRITE_DAC and every data right are not.
//
// ProtectTree (the daemon's start) also refuses a data directory that is, or
// contains, a symlink or junction, resets every entry under it that carries a
// DACL of its own to the inherited one — so an ACL planted before the first
// protected start, or added by hand, does not survive — and, when running as
// SYSTEM, makes SYSTEM the directory's owner (WRITE_OWNER is part of SYSTEM's
// Full Control ACE). Run under any other account it leaves the owner alone:
// that account may not be allowed to assign SYSTEM as owner, and with the
// OWNER RIGHTS ACE the owner carries no power here anyway.
//
// Elsewhere both functions do nothing: on Linux the data directory is
// /etc/digi-erp-connector, root's, and files are written 0600.
package dataacl

import (
	"fmt"
	"regexp"
	"strings"
)

// Well-known SIDs granted on every data directory.
const (
	SIDLocalSystem    = "S-1-5-18"     // NT AUTHORITY\SYSTEM — the service's account
	SIDAdministrators = "S-1-5-32-544" // BUILTIN\Administrators — the elevated GUI, cutover-seed, the installer
	SIDOwnerRights    = "S-1-3-4"      // OWNER RIGHTS — limits the owner's implicit rights to READ_CONTROL
)

var sidPattern = regexp.MustCompile(`^S-1-[0-9]+(-[0-9]+)+$`)

// SDDL is the DACL ProtectDir (container) or ProtectFile (!container)
// applies: protected, Full Control for SYSTEM, Administrators and accountSID
// (the process token's user; granted once when it is one of the other two),
// and READ_CONTROL alone for OWNER RIGHTS.
// accountSID must be a SID string ("S-1-5-21-…"): anything else is refused
// rather than spliced into the descriptor.
func SDDL(accountSID string, container bool) (string, error) {
	if !sidPattern.MatchString(accountSID) {
		return "", fmt.Errorf("dataacl: %q is not a SID string", accountSID)
	}
	flags := ""
	if container {
		flags = "OICI" // inherited by files (OI) and folders (CI) beneath
	}
	sids := []string{SIDLocalSystem, SIDAdministrators}
	if accountSID != SIDLocalSystem && accountSID != SIDAdministrators {
		sids = append(sids, accountSID)
	}
	var b strings.Builder
	b.WriteString("D:P")
	for _, sid := range sids {
		fmt.Fprintf(&b, "(A;%s;FA;;;%s)", flags, sid)
	}
	fmt.Fprintf(&b, "(A;%s;RC;;;%s)", flags, SIDOwnerRights)
	return b.String(), nil
}
