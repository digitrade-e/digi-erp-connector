//go:build windows

package dataacl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The rights a lock-down grants (icacls' M and RX).
const (
	maskModify      = 0x1301bf // FILE_GENERIC_READ | FILE_GENERIC_WRITE | FILE_GENERIC_EXECUTE | DELETE
	maskReadExecute = 0x1200a9 // FILE_GENERIC_READ | FILE_GENERIC_EXECUTE
)

// lockdownLimit is MaxLockdownEntries; tests lower it.
var lockdownLimit = MaxLockdownEntries

// enableRestorePrivileges enables, best effort, the privileges a lock-down and
// its restore need on entries whose DACL keeps even SYSTEM out (a planted
// file): opening for WRITE_DAC/WRITE_OWNER whatever the DACL (backup and
// restore semantics; the opens carry FILE_FLAG_BACKUP_SEMANTICS), and setting
// the owner a pre-image recorded. The provision process runs one command.
func enableRestorePrivileges() {
	var tok windows.Token
	if windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &tok) != nil {
		return
	}
	defer tok.Close()
	for _, name := range []string{"SeBackupPrivilege", "SeRestorePrivilege", "SeTakeOwnershipPrivilege"} {
		var luid windows.LUID
		p, err := windows.UTF16PtrFromString(name)
		if err != nil || windows.LookupPrivilegeValue(nil, p, &luid) != nil {
			continue
		}
		tp := windows.Tokenprivileges{PrivilegeCount: 1}
		tp.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
		_ = windows.AdjustTokenPrivileges(tok, false, &tp, 0, nil, nil)
	}
}

// fileIDOf is an entry's identity as recorded: volume serial and file index.
func fileIDOf(info windows.ByHandleFileInformation) string {
	return fmt.Sprintf("%08x-%08x%08x", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow)
}

// readSD reads an opened entry's owner and DACL.
func readSD(path string, n node) (*windows.SECURITY_DESCRIPTOR, error) {
	sd, err := windows.GetSecurityInfo(n.h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, fmt.Errorf("dataacl: read the security of %s: %w", path, err)
	}
	return sd, nil
}

// walkFolder visits every entry under dir (whose handle the caller holds and
// whose final path is rootFinal), parents before children, by handle: each
// entry opened for READ_CONTROL|WRITE_DAC|WRITE_OWNER without following it —
// a reparse point or a multiply-linked file refuses the walk — and checked to
// be where the listing said (its final path under rootFinal).
func walkFolder(dir, rootFinal string, visit func(path, rel string, n node) error) error {
	var walk func(path, rel string) error
	walk = func(path, rel string) error {
		var f *os.File
		if err := retryShare(func() (e error) { f, e = os.Open(path); return e }); err != nil {
			return err
		}
		names, err := f.Readdirnames(-1)
		f.Close()
		if err != nil {
			return err
		}
		sort.Strings(names)
		for _, name := range names {
			p, r := filepath.Join(path, name), filepath.Join(rel, name)
			n, err := open(p, windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER)
			if errors.Is(err, os.ErrNotExist) {
				continue // removed meanwhile
			}
			if err != nil {
				return err
			}
			final, err := finalPath(n.h)
			if err == nil && !strings.EqualFold(final, rootFinal+`\`+r) {
				err = fmt.Errorf("%w: %s is %s", ErrOutsideTree, p, final)
			}
			if err == nil {
				err = visit(p, r, n)
			}
			isDir := n.dir()
			windows.CloseHandle(n.h)
			if err != nil {
				return err
			}
			if isDir {
				if err := walk(p, r); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(dir, "")
}

// lockdownACLs computes the lock-down's DACLs from the folder's current one:
// the folder's (protected), and what its files and folders inherit.
type lockdownACLs struct {
	folder, file, dir string
	principals        []PrincipalRights
}

func buildLockdown(sd *windows.SECURITY_DESCRIPTOR, trusted []string) (lockdownACLs, error) {
	dacl, _, err := sd.DACL()
	if err != nil {
		return lockdownACLs{}, err
	}
	type rights struct {
		allow, deny       uint32
		allowed, isDenied bool
	}
	per := map[string]*rights{}
	var order []string
	get := func(sid string) *rights {
		if r, ok := per[sid]; ok {
			return r
		}
		per[sid] = &rights{}
		order = append(order, sid)
		return per[sid]
	}
	type denyACE struct {
		sid  string
		mask uint32
	}
	var denies []denyACE
	seenDeny := map[denyACE]bool{}
	if dacl == nil { // no DACL: every right to everyone
		r := get("S-1-1-0")
		r.allowed, r.allow = true, windows.GENERIC_ALL
	} else {
		for i := uint32(0); i < uint32(dacl.AceCount); i++ {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(dacl, i, &ace); err != nil {
				return lockdownACLs{}, fmt.Errorf("entry %d: %w", i, err)
			}
			effective := ace.Header.AceFlags&windows.INHERIT_ONLY_ACE == 0
			if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
				sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
				d := denyACE{sid, uint32(ace.Mask)}
				if !seenDeny[d] {
					seenDeny[d] = true
					denies = append(denies, d)
				}
				r := get(sid)
				r.isDenied = true
				if effective {
					r.deny |= uint32(ace.Mask)
				}
				continue
			}
			s, err := allowedSID(ace)
			if err != nil {
				return lockdownACLs{}, fmt.Errorf("entry %d: %w", i, err)
			}
			if s == nil {
				continue // audit, label, …
			}
			r := get(s.String())
			r.allowed = true
			if effective {
				r.allow |= uint32(ace.Mask)
			}
		}
	}
	trustedSet := map[string]bool{}
	var trustedOrder []string
	for _, s := range trusted {
		if !trustedSet[s] && s != SIDLocalSystem && s != SIDAdministrators {
			trustedSet[s] = true
			trustedOrder = append(trustedOrder, s)
		}
	}
	var others []string
	for _, s := range order {
		if per[s].allowed && s != SIDLocalSystem && s != SIDAdministrators && !trustedSet[s] {
			others = append(others, s)
		}
	}
	build := func(prefix, flags string) string {
		var b strings.Builder
		b.WriteString(prefix)
		for _, d := range denies {
			fmt.Fprintf(&b, "(D;%s;0x%x;;;%s)", flags, d.mask, d.sid)
		}
		fmt.Fprintf(&b, "(A;%s;FA;;;%s)(A;%s;FA;;;%s)", flags, SIDLocalSystem, flags, SIDAdministrators)
		for _, s := range trustedOrder {
			fmt.Fprintf(&b, "(A;%s;0x%x;;;%s)", flags, maskModify, s)
		}
		for _, s := range others {
			fmt.Fprintf(&b, "(A;%s;0x%x;;;%s)", flags, maskReadExecute, s)
		}
		return b.String()
	}
	out := lockdownACLs{folder: build("D:PAI", "OICI"), file: build("D:AI", "ID"), dir: build("D:AI", "OICIID")}

	after := map[string]uint32{SIDLocalSystem: windows.GENERIC_ALL, SIDAdministrators: windows.GENERIC_ALL}
	for _, s := range trustedOrder {
		after[s] = maskModify
	}
	for _, s := range others {
		after[s] = maskReadExecute
	}
	names := append([]string{SIDLocalSystem, SIDAdministrators}, trustedOrder...)
	for _, s := range order {
		if s != SIDLocalSystem && s != SIDAdministrators && !trustedSet[s] {
			names = append(names, s)
		}
	}
	for _, s := range names {
		r := per[s]
		var before uint32
		denied, deny := false, uint32(0)
		if r != nil {
			before, denied, deny = r.allow, r.isDenied, r.deny
		}
		p := PrincipalRights{SID: s, Name: sidName(s), Before: rightsWord(before), After: rightsWord(after[s])}
		if denied {
			p.Before += " (deny " + rightsWord(deny) + ")"
			p.After += " (deny " + rightsWord(deny) + ")"
		}
		out.principals = append(out.principals, p)
	}
	return out, nil
}

// rightsWord says what a mask lets its holder do with a folder.
func rightsWord(mask uint32) string {
	const fullAccess = 0x1f01ff
	switch {
	case mask == 0:
		return "none"
	case mask&windows.GENERIC_ALL != 0 || mask&fullAccess == fullAccess:
		return "full"
	case mask&windows.DELETE != 0 && (mask&windows.FILE_WRITE_DATA != 0 || mask&windows.GENERIC_WRITE != 0):
		return "modify"
	case mask&(dirWrite|windows.DELETE) != 0:
		return "write"
	case mask&(windows.FILE_READ_DATA|windows.FILE_EXECUTE|windows.GENERIC_READ|windows.GENERIC_EXECUTE) != 0:
		return "read"
	}
	return "special"
}

// sidName is a SID string's account name, or the SID.
func sidName(s string) string {
	sid, err := windows.StringToSid(s)
	if err != nil {
		return s
	}
	return accountName(sid)
}

// PlanLockdown reads what a lock-down of dir would do, writing nothing: the
// security of dir and every entry beneath (refusing a reparse point or a
// multiply-linked file anywhere, and more than MaxLockdownEntries entries),
// the folder's principals before and after, and who can still rename dir
// away through a folder above (trusted SIDs left out).
func PlanLockdown(dir string, trusted []string) (LockdownPlan, error) {
	enableRestorePrivileges()
	root, err := open(dir, windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER)
	if err != nil {
		return LockdownPlan{}, err
	}
	defer windows.CloseHandle(root.h)
	if !root.dir() {
		return LockdownPlan{}, fmt.Errorf("dataacl: %s is not a folder", dir)
	}
	rootFinal, err := finalPath(root.h)
	if err != nil {
		return LockdownPlan{}, err
	}
	sd, err := readSD(dir, root)
	if err != nil {
		return LockdownPlan{}, err
	}
	acls, err := buildLockdown(sd, trusted)
	if err != nil {
		return LockdownPlan{}, fmt.Errorf("dataacl: read the DACL of %s: %w", dir, err)
	}
	plan := LockdownPlan{Entries: []FolderEntry{{Rel: "", FileID: fileIDOf(root.info), SDDL: sd.String()}}, Principals: acls.principals,
		ParentWriters: []Writer{}, FolderDACL: acls.folder, FileDACL: acls.file, DirDACL: acls.dir}
	err = walkFolder(dir, rootFinal, func(path, rel string, n node) error {
		if len(plan.Entries) >= lockdownLimit {
			return fmt.Errorf("%w: %s holds more than %d", ErrTooManyEntries, dir, lockdownLimit)
		}
		esd, err := readSD(path, n)
		if err != nil {
			return err
		}
		plan.Entries = append(plan.Entries, FolderEntry{Rel: rel, FileID: fileIDOf(n.info), SDDL: esd.String()})
		return nil
	})
	if err != nil {
		return LockdownPlan{}, err
	}
	if chain := folderWritersChain(dir); len(chain) > 1 {
		who, err := writers(chain[1:])
		if err != nil {
			return LockdownPlan{}, err
		}
		skip := map[string]bool{}
		for _, s := range trusted {
			skip[s] = true
		}
		for _, w := range who {
			if !skip[w.SID] {
				plan.ParentWriters = append(plan.ParentWriters, w)
			}
		}
	}
	return plan, nil
}

// ApplyLockdown writes plan (PlanLockdown's, for the same dir and trusted
// SIDs): the folder's protected DACL and Administrators as its owner, then
// every entry beneath made to inherit it (explicit entries dropped, an owner
// that is not trusted handed to Administrators). The folder must still be the
// object the plan read. It answers how many entries it wrote.
func ApplyLockdown(dir string, trusted []string, plan LockdownPlan) (int, error) {
	if len(plan.Entries) == 0 {
		return 0, errors.New("dataacl: an empty lock-down plan")
	}
	enableRestorePrivileges()
	t, err := newTrust()
	if err != nil {
		return 0, err
	}
	trustedSet := map[string]bool{}
	for _, s := range trusted {
		trustedSet[s] = true
	}
	root, err := open(dir, windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(root.h)
	if fileIDOf(root.info) != plan.Entries[0].FileID {
		return 0, fmt.Errorf("%w: %s is not the folder the check read", ErrOutsideTree, dir)
	}
	rootFinal, err := finalPath(root.h)
	if err != nil {
		return 0, err
	}
	var sds [3]*windows.SECURITY_DESCRIPTOR
	for i, s := range []string{plan.FolderDACL, plan.FileDACL, plan.DirDACL} {
		if sds[i], err = windows.SecurityDescriptorFromString(s); err != nil {
			return 0, fmt.Errorf("dataacl: the lock-down's DACL %q: %w", s, err)
		}
	}
	if err := write(dir, root, sds[0], true); err != nil {
		return 0, err
	}
	if err := setOwner(root.h, t.baSID); err != nil {
		return 1, fmt.Errorf("dataacl: make Administrators the owner of %s: %w", dir, err)
	}
	written := 1
	err = walkFolder(dir, rootFinal, func(path, _ string, n node) error {
		if written >= 2*lockdownLimit {
			return fmt.Errorf("%w: %s grew past %d during the lock-down", ErrTooManyEntries, dir, 2*lockdownLimit)
		}
		want := sds[1]
		if n.dir() {
			want = sds[2]
		}
		if err := write(path, n, want, false); err != nil {
			return err
		}
		esd, err := readSD(path, n)
		if err != nil {
			return err
		}
		owner, _, err := esd.Owner()
		if err != nil {
			return err
		}
		if o := owner.String(); !trustedSet[o] {
			if ok, _ := trustedOwner(o, t.tokenUser, t.admins, t.indirectFn); !ok {
				if err := setOwner(n.h, t.baSID); err != nil {
					return fmt.Errorf("dataacl: make Administrators the owner of %s: %w", path, err)
				}
			}
		}
		written++
		return nil
	})
	return written, err
}

// RestoreFolder puts back the security a lock-down recorded for dir: each
// entry that is still the same object (file ID) where it was, by handle,
// owner and DACL as recorded; the others are skipped (gone, replaced,
// become a reparse point). The folder itself (entries[0]) must still be the
// object recorded, and is restored last.
func RestoreFolder(dir string, entries []FolderEntry) (restored, skipped int, err error) {
	if len(entries) == 0 || entries[0].Rel != "" {
		return 0, 0, errors.New("dataacl: a pre-image without its folder")
	}
	enableRestorePrivileges()
	root, err := open(dir, windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER)
	if err != nil {
		return 0, 0, err
	}
	defer windows.CloseHandle(root.h)
	if fileIDOf(root.info) != entries[0].FileID {
		return 0, 0, fmt.Errorf("%w: %s is not the folder the pre-image recorded", ErrOutsideTree, dir)
	}
	rootFinal, err := finalPath(root.h)
	if err != nil {
		return 0, 0, err
	}
	for _, e := range entries[1:] {
		path := filepath.Join(dir, e.Rel)
		n, err := open(path, windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER)
		if err != nil {
			skipped++ // gone, or a reparse point / hard link now: never written through
			continue
		}
		final, ferr := finalPath(n.h)
		if ferr != nil || !strings.EqualFold(final, rootFinal+`\`+e.Rel) || fileIDOf(n.info) != e.FileID {
			windows.CloseHandle(n.h)
			skipped++
			continue
		}
		err = restoreSD(path, n, e.SDDL)
		windows.CloseHandle(n.h)
		if err != nil {
			return restored, skipped, err
		}
		restored++
	}
	if err := restoreSD(dir, root, entries[0].SDDL); err != nil {
		return restored, skipped, err
	}
	return restored + 1, skipped, nil
}

// restoreSD sets an entry's owner and DACL to a recorded "O:…D:…".
func restoreSD(path string, n node, sddl string) error {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return fmt.Errorf("dataacl: the recorded security of %s: %w", path, err)
	}
	control, _, err := sd.Control()
	if err != nil {
		return err
	}
	info := windows.SECURITY_INFORMATION(windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	if control&windows.SE_DACL_PROTECTED != 0 {
		info = windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	if err := windows.SetKernelObjectSecurity(n.h, info, sd); err != nil {
		return fmt.Errorf("dataacl: restore the security of %s: %w", path, err)
	}
	return nil
}
