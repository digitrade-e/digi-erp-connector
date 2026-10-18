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
// already there or created later — carries the same three ACEs.
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
)

var sidPattern = regexp.MustCompile(`^S-1-[0-9]+(-[0-9]+)+$`)

// SDDL is the DACL ProtectDir (container) or ProtectFile (!container)
// applies: protected, Full Control for SYSTEM, Administrators and accountSID
// (the process token's user; granted once when it is one of the other two).
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
	return b.String(), nil
}
