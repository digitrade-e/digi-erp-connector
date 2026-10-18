// Package dataacl locks the connector's data directory down on Windows.
//
// %ProgramData%'s default ACL lets every local user read what is created
// under it. The data directory holds config.yaml (the auth credential and
// signing secret, the DB user), the DPAPI-sealed secrets and — once receipts
// are on — the receipt index and history: customers' names, phones, emails,
// card digits and confirmation codes.
//
// ProtectTree gives the directory a PROTECTED DACL (nothing inherited from
// %ProgramData%) granting Full Control to SYSTEM and BUILTIN\Administrators —
// plus the process's own account only when that account is neither (a service
// run under a dedicated non-admin account) — and READ_CONTROL alone to OWNER
// RIGHTS (S-1-3-4). Every file and folder beneath carries the same ACEs as
// inherited ones.
//
// OWNER RIGHTS: without that ACE the owner of an object — whoever created it,
// possibly a plain user, since Users may create folders in %ProgramData% —
// keeps an implicit READ_CONTROL and WRITE_DAC whatever the DACL says. With it
// the owner's implicit rights are exactly READ_CONTROL (reading who has
// access reveals nothing the DACL protects; icacls and Explorer need it).
//
// Everything is done through handles, never by path, because the daemon runs
// as SYSTEM and a path can be redirected under it: every entry is opened with
// FILE_FLAG_OPEN_REPARSE_POINT, refused if it is a reparse point (symlink,
// junction, mount point…), a file with more than one hard link, an object
// whose final path is not where the walk expects it, or owned by anyone
// outside SYSTEM, Administrators (the group or a direct member),
// TrustedInstaller and the process's own account — a planted entry is owned
// by its planter. Security is read with GetSecurityInfo and written with
// SetKernelObjectSecurity on that handle, which never propagates: the walk
// writes each child's inherited ACEs itself, parents before children, holding
// every ancestor's handle open without FILE_SHARE_DELETE so none can be
// renamed or replaced meanwhile. An entry is written only when its DACL
// differs from the template, so a steady-state start only reads.
//
// Elsewhere these functions do nothing (OpenLog and MoveAside work
// everywhere): on Linux the data directory is /etc/digi-erp-connector,
// root's, and files are written 0600.
package dataacl

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// Well-known SIDs.
const (
	SIDLocalSystem      = "S-1-5-18"     // NT AUTHORITY\SYSTEM — the service's account
	SIDAdministrators   = "S-1-5-32-544" // BUILTIN\Administrators — the elevated GUI, cutover-seed, the installer
	SIDOwnerRights      = "S-1-3-4"      // OWNER RIGHTS — limits the owner's implicit rights to READ_CONTROL
	SIDTrustedInstaller = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"
)

var (
	// ErrReparsePoint: the data directory or an entry under it is a symlink,
	// junction or other reparse point. Data written through it would land
	// outside the protected tree, so nothing is changed and the daemon stops.
	ErrReparsePoint = errors.New("dataacl: a symlink, junction or other reparse point is in the data directory")
	// ErrHardLink: a file under the data directory has more than one hard
	// link. Its security descriptor is shared with the other name, which may be
	// anywhere on the volume, so it is never written.
	ErrHardLink = errors.New("dataacl: a file with more than one hard link is in the data directory")
	// ErrUntrustedOwner: an entry is owned by someone outside SYSTEM,
	// Administrators, TrustedInstaller and this process's account — it was
	// planted, and its planter may still hold a handle to it.
	ErrUntrustedOwner = errors.New("dataacl: an entry in the data directory has an untrusted owner")
	// ErrOutsideTree: an entry's final path is not the one the walk expects.
	ErrOutsideTree = errors.New("dataacl: an entry resolves outside the data directory")
)

// Report is what ProtectTree did.
type Report struct {
	Entries    int      // entries under the directory (the directory itself not counted)
	Reset      int      // entries whose DACL differed from the template and was rewritten
	ResetPaths []string // their paths
	RootSet    bool     // the directory's own DACL was rewritten
	OwnerSet   bool     // the directory's owner was set to SYSTEM (only when running as SYSTEM)
	OwnerErr   error    // setting the owner failed; not fatal
	AdminsErr  error    // the Administrators members could not be listed; only the fixed owners were trusted
	Claimed    int      // entries whose owner was set to Administrators (TreeOptions.ClaimForAdministrators)
}

// TreeOptions tunes ProtectTree.
type TreeOptions struct {
	// Progress, if set, is called every progressEvery entries — the service
	// reports SERVICE_START_PENDING with a new checkpoint from it.
	Progress func()
	// ClaimForAdministrators makes BUILTIN\Administrators the owner of every
	// entry this process's own account owns (the elevated GUI, for what it
	// created): an admin token may always assign that owner, and the daemon
	// always trusts it — whether or not it can resolve how the operator is an
	// administrator (R3-5).
	ClaimForAdministrators bool
}

const progressEvery = 64

// testHookBeforeEntry, if set, runs before each entry under the root is
// opened (tests swap an entry for a junction there).
var testHookBeforeEntry func(path string)

var sidPattern = regexp.MustCompile(`^S-1-[0-9]+(-[0-9]+)+$`)

// accountFor is the extra SID the DACL grants: the process's own account, or
// none when SYSTEM and Administrators already cover it — the service as
// LocalSystem, the elevated GUI (its token's Administrators group is enabled).
// So the daemon and the elevated GUI write the very same DACL.
func accountFor(tokenUser string, tokenIsAdmin bool) string {
	if tokenUser == SIDLocalSystem || tokenUser == SIDAdministrators || tokenIsAdmin {
		return ""
	}
	return tokenUser
}

// trustedOwner reports whether an entry owned by owner may be protected:
// SYSTEM, Administrators, TrustedInstaller, the process's own account, a
// direct member of the local Administrators group, or — asked of indirect
// (may be nil) — a member through a nested group (R3-5: a domain admin in
// Administrators via Domain Admins, owning what it saved with UAC off). An
// owner whose membership cannot be resolved is untrusted, with the reason:
// fail closed. Anyone else planted it.
func trustedOwner(owner, tokenUser string, admins map[string]bool, indirect func(sid string) (bool, error)) (bool, error) {
	switch owner {
	case SIDLocalSystem, SIDAdministrators, SIDTrustedInstaller, tokenUser:
		return true, nil
	}
	if admins[owner] {
		return true, nil
	}
	if indirect == nil {
		return false, nil
	}
	ok, err := indirect(owner)
	if err != nil {
		return false, fmt.Errorf("could not resolve whether %s is an administrator: %w", owner, err)
	}
	return ok, nil
}

// longPathFor gives a Windows absolute path its \\?\ form (\\?\UNC\ for a
// share), with . and .. resolved (the prefix turns off the system's own
// normalisation), so a raw CreateFile works past MAX_PATH. Anything else is
// returned as is; win false returns p unchanged.
func longPathFor(p string, win bool) string {
	if !win {
		return p
	}
	p = strings.ReplaceAll(p, "/", `\`)
	if strings.HasPrefix(p, `\\?\`) {
		return p
	}
	var prefix, rest string
	keep := 1
	switch {
	case strings.HasPrefix(p, `\\`):
		prefix, rest, keep = `\\?\UNC\`, p[2:], 2
	case len(p) >= 3 && p[1] == ':' && p[2] == '\\':
		prefix, rest = `\\?\`, p
	default:
		return p
	}
	var out []string
	for _, seg := range strings.Split(rest, `\`) {
		switch seg {
		case "", ".":
			continue
		case "..":
			if len(out) > keep {
				out = out[:len(out)-1]
			}
			continue
		}
		out = append(out, seg)
	}
	return prefix + strings.Join(out, `\`)
}

func aces(account, flags string) string {
	sids := []string{SIDLocalSystem, SIDAdministrators}
	if account != "" && account != SIDLocalSystem && account != SIDAdministrators {
		sids = append(sids, account)
	}
	var b strings.Builder
	for _, sid := range sids {
		fmt.Fprintf(&b, "(A;%s;FA;;;%s)", flags, sid)
	}
	fmt.Fprintf(&b, "(A;%s;RC;;;%s)", flags, SIDOwnerRights)
	return b.String()
}

func checkAccount(account string) error {
	if account != "" && !sidPattern.MatchString(account) {
		return fmt.Errorf("dataacl: %q is not a SID string", account)
	}
	return nil
}

// SDDL is the data directory's own DACL: protected, Full Control for SYSTEM,
// Administrators and account (empty: none — see accountFor), READ_CONTROL for
// OWNER RIGHTS, inherited by files (OI) and folders (CI). With container false
// it is the same, protected, for a lone file. account must be empty or a SID
// string: anything else is refused rather than spliced into the descriptor.
func SDDL(account string, container bool) (string, error) {
	if err := checkAccount(account); err != nil {
		return "", err
	}
	flags := ""
	if container {
		flags = "OICI"
	}
	return "D:PAI" + aces(account, flags), nil
}

// ChildSDDL is what every entry under the directory carries: the directory's
// ACEs as inherited ones (ID), unprotected; folders keep OICI so their own
// children inherit them in turn.
func ChildSDDL(account string, container bool) (string, error) {
	if err := checkAccount(account); err != nil {
		return "", err
	}
	flags := "ID"
	if container {
		flags = "OICIID"
	}
	return "D:AI" + aces(account, flags), nil
}

// sameDACL compares two DACL renderings ("D:<flags>(ace)(ace)…"): the P flag
// and the ACEs must match; AI/AR (auto-inherit bookkeeping) are ignored.
func sameDACL(a, b string) bool {
	pa, xa := splitDACL(a)
	pb, xb := splitDACL(b)
	return pa == pb && xa == xb
}

func splitDACL(s string) (protected bool, aces string) {
	i := strings.Index(s, "D:")
	if i < 0 {
		return false, s
	}
	s = s[i+2:]
	if j := strings.Index(s, "S:"); j >= 0 {
		s = s[:j]
	}
	k := strings.IndexByte(s, '(')
	if k < 0 {
		return strings.Contains(s, "P"), ""
	}
	return strings.Contains(s[:k], "P"), s[k:]
}

// MoveAside renames path to "<path>.<UTC time>.old" so the next open creates a
// fresh file — a new file object no earlier handle points at. A missing path
// is ("", nil).
func MoveAside(path string) (string, error) {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	to := path + "." + time.Now().UTC().Format("20060102T150405.000000000Z") + ".old"
	if err := os.Rename(path, to); err != nil {
		return "", fmt.Errorf("dataacl: move %s aside: %w", path, err)
	}
	return to, nil
}
