//go:build windows

package dataacl

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
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

// daclBytes is an SD's DACL as raw bytes; a NULL DACL (every right to
// everyone) comes back as the explicit DACL it amounts to — SYSTEM and
// Administrators Full, Everyone Full — so masking it lowers Everyone and
// grants no one anything new.
func daclBytes(sd *windows.SECURITY_DESCRIPTOR) ([]byte, error) {
	dacl, _, err := sd.DACL()
	if err != nil {
		return nil, err
	}
	if dacl == nil {
		nsd, err := windows.SecurityDescriptorFromString("D:(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;WD)")
		if err != nil {
			return nil, err
		}
		if dacl, _, err = nsd.DACL(); err != nil {
			return nil, err
		}
	}
	head := unsafe.Slice((*byte)(unsafe.Pointer(dacl)), aclHeaderSize)
	size := int(binary.LittleEndian.Uint16(head[2:])) // AclSize (unexported in x/sys)
	return append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(dacl)), size)...), nil
}

// writeDACL sets a raw DACL through the handle, protected or not; no
// propagation.
func writeDACL(path string, h windows.Handle, acl []byte, protected bool) error {
	sd, err := windows.NewSecurityDescriptor()
	if err != nil {
		return err
	}
	if err := sd.SetDACL((*windows.ACL)(unsafe.Pointer(&acl[0])), true, false); err != nil {
		return err
	}
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	if protected {
		info = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	err = windows.SetKernelObjectSecurity(h, info, sd)
	runtime.KeepAlive(acl)
	if err != nil {
		return fmt.Errorf("dataacl: set the DACL of %s: %w", path, err)
	}
	return nil
}

// sidName is a SID string's account name, or the SID.
func sidName(s string) string {
	sid, err := windows.StringToSid(s)
	if err != nil {
		return s
	}
	return accountName(sid)
}

// lockdownEntry is one entry as the lock-down would leave it.
type lockdownEntry struct {
	before, after []byte
	owner         string
	ownerTrusted  bool
	protected     bool
	changed       bool
}

// planEntry reads an opened entry and masks its DACL (root: the folder,
// whose link to its parent is cut).
func planEntry(path string, n node, trusted map[string]bool, t *trust, root bool) (lockdownEntry, *windows.SECURITY_DESCRIPTOR, error) {
	sd, err := readSD(path, n)
	if err != nil {
		return lockdownEntry{}, nil, err
	}
	before, err := daclBytes(sd)
	if err != nil {
		return lockdownEntry{}, nil, fmt.Errorf("dataacl: read the DACL of %s: %w", path, err)
	}
	after, changed, err := MaskDACL(before, trusted, root)
	if err != nil {
		return lockdownEntry{}, nil, fmt.Errorf("dataacl: the DACL of %s: %w", path, err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return lockdownEntry{}, nil, fmt.Errorf("dataacl: read the owner of %s: %w", path, err)
	}
	control, _, err := sd.Control()
	if err != nil {
		return lockdownEntry{}, nil, err
	}
	e := lockdownEntry{before: before, after: after, owner: owner.String(), protected: control&windows.SE_DACL_PROTECTED != 0}
	e.ownerTrusted, _ = trustedOwner(e.owner, t.tokenUser, t.admins, t.indirectFn)
	e.changed = changed || !e.ownerTrusted || (root && !e.protected)
	return e, sd, nil
}

// summary gathers what a lock-down takes from whom, across the tree.
type summary struct {
	order       []string
	before      map[string]uint32
	after       map[string]uint32
	entries     map[string]int
	owners      map[string]int
	ownersOrder []string
	changed     int
}

func newSummary() *summary {
	return &summary{before: map[string]uint32{}, after: map[string]uint32{}, entries: map[string]int{}, owners: map[string]int{}}
}

func (s *summary) add(e lockdownEntry) {
	if e.changed {
		s.changed++
	}
	if !e.ownerTrusted {
		if _, ok := s.owners[e.owner]; !ok {
			s.ownersOrder = append(s.ownersOrder, e.owner)
		}
		s.owners[e.owner]++
	}
	b, err1 := ParseACL(e.before)
	a, err2 := ParseACL(e.after)
	if err1 != nil || err2 != nil {
		return
	}
	seen := map[string]bool{}
	for _, ace := range b {
		if !ace.Allow() || ace.InheritOnly() || ace.SID == "" || seen[ace.SID] {
			continue
		}
		seen[ace.SID] = true
		if _, ok := s.before[ace.SID]; !ok {
			s.order = append(s.order, ace.SID)
		}
		was, now := GrantedTo(b, ace.SID), GrantedTo(a, ace.SID)
		s.before[ace.SID] |= was
		s.after[ace.SID] |= now
		if was != now {
			s.entries[ace.SID]++
		}
	}
}

func (s *summary) principals() []PrincipalRights {
	out := make([]PrincipalRights, 0, len(s.order))
	for _, sid := range s.order {
		out = append(out, PrincipalRights{SID: sid, Name: sidName(sid), Before: RightsWord(s.before[sid]), After: RightsWord(s.after[sid]), Entries: s.entries[sid]})
	}
	// Who loses something first, then by name: the approval form's order.
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].Entries > 0) != (out[j].Entries > 0) {
			return out[i].Entries > 0
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (s *summary) ownerChanges() []OwnerChange {
	out := make([]OwnerChange, 0, len(s.ownersOrder))
	for _, sid := range s.ownersOrder {
		out = append(out, OwnerChange{SID: sid, Name: sidName(sid), Entries: s.owners[sid]})
	}
	return out
}

// PlanLockdown reads what a lock-down of dir would do, writing nothing (plan
// 087 review T2: masking, never a grant): the security of dir and of every
// entry beneath (refusing a reparse point or a multiply-linked file
// anywhere, and more than MaxLockdownEntries entries), per principal the
// strongest rights it has anywhere in the tree before and after and on how
// many entries they change, the owners handed to Administrators, the files
// whose extension is in execExts (review T1), the folder's final path
// (review T3), and who can still rename dir away through a folder above
// (trusted SIDs left out).
func PlanLockdown(dir string, trusted []string, execExts map[string]bool) (LockdownPlan, error) {
	enableRestorePrivileges()
	t, err := newTrust()
	if err != nil {
		return LockdownPlan{}, err
	}
	trustedSet := map[string]bool{}
	for _, s := range trusted {
		trustedSet[s] = true
	}
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
	sum := newSummary()
	re, sd, err := planEntry(dir, root, trustedSet, t, true)
	if err != nil {
		return LockdownPlan{}, err
	}
	sum.add(re)
	plan := LockdownPlan{Entries: []FolderEntry{{Rel: "", FileID: fileIDOf(root.info), SDDL: sd.String()}}, ParentWriters: []Writer{},
		Executables: []string{}, FinalPath: rootFinal}
	err = walkFolder(dir, rootFinal, func(path, rel string, n node) error {
		if len(plan.Entries) >= lockdownLimit {
			return fmt.Errorf("%w: %s holds more than %d", ErrTooManyEntries, dir, lockdownLimit)
		}
		e, esd, err := planEntry(path, n, trustedSet, t, false)
		if err != nil {
			return err
		}
		sum.add(e)
		plan.Entries = append(plan.Entries, FolderEntry{Rel: rel, FileID: fileIDOf(n.info), SDDL: esd.String()})
		if !n.dir() && execExts[strings.ToLower(filepath.Ext(rel))] && len(plan.Executables) < 20 {
			plan.Executables = append(plan.Executables, rel)
		}
		return nil
	})
	if err != nil {
		return LockdownPlan{}, err
	}
	plan.Principals, plan.Owners, plan.EntriesChanged = sum.principals(), sum.ownerChanges(), sum.changed
	if chain := folderWritersChain(dir); len(chain) > 1 {
		who, err := writers(chain[1:])
		if err != nil {
			return LockdownPlan{}, err
		}
		for _, w := range who {
			if !trustedSet[w.SID] {
				plan.ParentWriters = append(plan.ParentWriters, w)
			}
		}
	}
	return plan, nil
}

// ApplyLockdown masks dir and every entry beneath, as PlanLockdown read it
// (review T2): each DACL lowered in place — nothing added, nothing raised —
// the folder's cut from its parent (protected, inherited entries kept as
// explicit), every other entry's protection left as it was; an owner that is
// not trusted handed to Administrators. The folder must still be the object
// the plan read. It answers how many entries it wrote.
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
	if !strings.EqualFold(rootFinal, plan.FinalPath) { // the path judged (review T3)
		return 0, fmt.Errorf("%w: %s resolves to %s, not %s as the check read", ErrOutsideTree, dir, rootFinal, plan.FinalPath)
	}
	apply := func(path string, n node, isRoot bool) error {
		e, _, err := planEntry(path, n, trustedSet, t, isRoot)
		if err != nil {
			return err
		}
		if !e.changed {
			return nil
		}
		if err := writeDACL(path, n.h, e.after, isRoot || e.protected); err != nil {
			return err
		}
		if !e.ownerTrusted {
			if err := setOwner(n.h, t.baSID); err != nil {
				return fmt.Errorf("dataacl: make Administrators the owner of %s: %w", path, err)
			}
		}
		return nil
	}
	if err := apply(dir, root, true); err != nil {
		return 0, err
	}
	written := 1
	err = walkFolder(dir, rootFinal, func(path, _ string, n node) error {
		if written >= 2*lockdownLimit {
			return fmt.Errorf("%w: %s grew past %d during the lock-down", ErrTooManyEntries, dir, 2*lockdownLimit)
		}
		if err := apply(path, n, false); err != nil {
			return err
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
