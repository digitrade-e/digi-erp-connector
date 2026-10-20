//go:build windows

package dataacl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Every open: never follow a reparse point, open directories too. The share
// mode (read/write, not delete) matters only for opens that ask for data
// access (listing a folder, appending to a log): handles opened for
// READ_CONTROL|WRITE_DAC alone take no part in share checks, so they do not
// stop anyone renaming the entry. The walk's safety comes from elsewhere:
// each entry is inspected through its own handle (reparse attribute, link
// count, final path under the root, owner) and its DACL written through that
// same handle; a second handle (the claim) is checked to be the same object
// (volume + file index); and a folder with open handles beneath it cannot be
// renamed (pinned by TestProtectTree_HoldsAncestorsOpen).
const (
	openFlags = windows.FILE_FLAG_OPEN_REPARSE_POINT | windows.FILE_FLAG_BACKUP_SEMANTICS
	openShare = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE
)

// node is one opened entry.
type node struct {
	h    windows.Handle
	info windows.ByHandleFileInformation
}

func (n node) dir() bool {
	return n.info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
}

// A sharing violation — another process holding a file without sharing the
// data access asked for — usually passes, so opens that ask for data access
// (listing a directory, appending to a log) retry shareRetries times,
// shareRetryDelay apart, before failing. The walk's per-entry opens ask only
// for security rights, which Windows never subjects to sharing.
var (
	shareRetries    = 3
	shareRetryDelay = 250 * time.Millisecond
)

func retryShare(f func() error) error {
	for attempt := 0; ; attempt++ {
		err := f()
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) || attempt >= shareRetries {
			return err
		}
		time.Sleep(shareRetryDelay)
	}
}

// longPath is longPathFor on Windows: raw CreateFile calls get the \\?\ form.
func longPath(p string) string { return longPathFor(p, true) }

// open opens path by handle without following it and refuses a reparse
// point and a multiply-linked file. A missing path wraps os.ErrNotExist.
func open(path string, access uint32) (node, error) {
	p, err := windows.UTF16PtrFromString(longPath(path))
	if err != nil {
		return node{}, err
	}
	var h windows.Handle
	err = retryShare(func() (e error) {
		h, e = windows.CreateFile(p, access, openShare, nil, windows.OPEN_EXISTING, openFlags, 0)
		return e
	})
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return node{}, &os.PathError{Op: "open", Path: path, Err: os.ErrNotExist}
		}
		return node{}, fmt.Errorf("dataacl: open %s to check its security: %w", path, err)
	}
	n := node{h: h}
	if err := windows.GetFileInformationByHandle(h, &n.info); err != nil {
		windows.CloseHandle(h)
		return node{}, fmt.Errorf("dataacl: inspect %s: %w", path, err)
	}
	if err := refuseLinks(path, n.info); err != nil {
		windows.CloseHandle(h)
		return node{}, err
	}
	return n, nil
}

func refuseLinks(path string, info windows.ByHandleFileInformation) error {
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("%w: %s", ErrReparsePoint, path)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 && info.NumberOfLinks > 1 {
		return fmt.Errorf("%w: %s (%d links)", ErrHardLink, path, info.NumberOfLinks)
	}
	return nil
}

func finalPath(h windows.Handle) (string, error) {
	buf := make([]uint16, 512)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return "", err
		}
		if int(n) < len(buf) {
			return windows.UTF16ToString(buf[:n]), nil
		}
		buf = make([]uint16, n+1)
	}
}

// trust is who the walk trusts and what it writes, computed once.
type trust struct {
	tokenUser  string
	account    string // the extra SID in the templates ("" = none)
	admins     map[string]bool
	adminsErr  error
	adminsName string          // the local Administrators group's (localized) name
	indirect   map[string]bool // owners resolved as members through a nested group
	indirectFn func(sid string) (bool, error)
	claim      bool // TreeOptions.ClaimForAdministrators
	baSID      *windows.SID
	root       *windows.SECURITY_DESCRIPTOR
	childDir   *windows.SECURITY_DESCRIPTOR
	childFile  *windows.SECURITY_DESCRIPTOR
}

func newTrust() (*trust, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("dataacl: read the process token user: %w", err)
	}
	t := &trust{tokenUser: tu.User.Sid.String()}
	ba, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, err
	}
	// Token(0): the thread's token, or a duplicate of the process's — what
	// CheckTokenMembership needs.
	isAdmin, err := windows.Token(0).IsMember(ba)
	if err != nil {
		return nil, fmt.Errorf("dataacl: check Administrators membership: %w", err)
	}
	t.account = accountFor(t.tokenUser, isAdmin)
	t.baSID = ba
	t.indirect = map[string]bool{}
	if name, _, _, err := ba.LookupAccount(""); err == nil {
		t.adminsName = name
	}
	t.admins, t.adminsErr = localAdministrators(ba)
	t.indirectFn = t.isIndirectAdministrator
	if testIndirectOverride != nil {
		t.indirectFn = testIndirectOverride
	}

	for _, x := range []struct {
		dst       **windows.SECURITY_DESCRIPTOR
		child     bool
		container bool
	}{{&t.root, false, true}, {&t.childDir, true, true}, {&t.childFile, true, false}} {
		var s string
		if x.child {
			s, err = ChildSDDL(t.account, x.container)
		} else {
			s, err = SDDL(t.account, x.container)
		}
		if err != nil {
			return nil, err
		}
		if *x.dst, err = windows.SecurityDescriptorFromString(s); err != nil {
			return nil, fmt.Errorf("dataacl: parse %s: %w", s, err)
		}
	}
	return t, nil
}

var (
	netapi32                    = windows.NewLazySystemDLL("netapi32.dll")
	procNetLocalGroupGetMembers = netapi32.NewProc("NetLocalGroupGetMembers")
	procNetUserGetLocalGroups   = netapi32.NewProc("NetUserGetLocalGroups")
)

// isIndirectAdministrator resolves whether the user sid belongs to the local
// Administrators group through a nested group (NetUserGetLocalGroups with
// LG_INCLUDE_INDIRECT — for a domain user that asks the domain). Not a user,
// or not a member: false. Unresolvable: an error (the caller fails closed).
func (t *trust) isIndirectAdministrator(sidStr string) (bool, error) {
	if ok, seen := t.indirect[sidStr]; seen {
		return ok, nil
	}
	if t.adminsName == "" {
		return false, errors.New("the Administrators group's name is unknown")
	}
	sid, err := windows.StringToSid(sidStr)
	if err != nil {
		return false, err
	}
	account, domain, use, err := sid.LookupAccount("")
	if errors.Is(err, windows.ERROR_NONE_MAPPED) {
		return false, fmt.Errorf("%s resolves to no account (a deleted user, or one of an unreachable domain): %w", sidStr, err)
	}
	if err != nil {
		return false, fmt.Errorf("look up %s: %w", sidStr, err)
	}
	if use != windows.SidTypeUser {
		t.indirect[sidStr] = false
		return false, nil
	}
	user, err := windows.UTF16PtrFromString(domain + `\` + account)
	if err != nil {
		return false, err
	}
	var (
		buf         *byte
		read, total uint32
	)
	const (
		levelNamesOnly    = 0 // LOCALGROUP_USERS_INFO_0: one LPWSTR each
		lgIncludeIndirect = 1
	)
	r, _, _ := procNetUserGetLocalGroups.Call(0, uintptr(unsafe.Pointer(user)), levelNamesOnly, lgIncludeIndirect,
		uintptr(unsafe.Pointer(&buf)), uintptr(^uint32(0)), uintptr(unsafe.Pointer(&read)), uintptr(unsafe.Pointer(&total)))
	if r != 0 {
		return false, fmt.Errorf("list the local groups of %s\\%s: %w", domain, account, syscall.Errno(r))
	}
	defer windows.NetApiBufferFree(buf)
	ok := false
	if read > 0 {
		for _, name := range unsafe.Slice((**uint16)(unsafe.Pointer(buf)), read) {
			if strings.EqualFold(windows.UTF16PtrToString(name), t.adminsName) {
				ok = true
				break
			}
		}
	}
	t.indirect[sidStr] = ok
	return ok, nil
}

// localAdministrators lists the direct members of the local Administrators
// group by SID. Nested groups are not expanded.
func localAdministrators(ba *windows.SID) (map[string]bool, error) {
	name, _, _, err := ba.LookupAccount("")
	if err != nil {
		return map[string]bool{}, err
	}
	group, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return map[string]bool{}, err
	}
	var (
		buf          *byte
		read, total  uint32
		resume       uintptr
		maxPreferred = ^uint32(0)
	)
	const levelSIDsOnly = 0 // LOCALGROUP_MEMBERS_INFO_0: one PSID each
	r, _, _ := procNetLocalGroupGetMembers.Call(0, uintptr(unsafe.Pointer(group)), levelSIDsOnly,
		uintptr(unsafe.Pointer(&buf)), uintptr(maxPreferred),
		uintptr(unsafe.Pointer(&read)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&resume)))
	if r != 0 {
		return map[string]bool{}, fmt.Errorf("dataacl: list the members of %s: %w", name, syscall.Errno(r))
	}
	defer windows.NetApiBufferFree(buf)
	out := make(map[string]bool, read)
	if read > 0 {
		for _, sid := range unsafe.Slice((**windows.SID)(unsafe.Pointer(buf)), read) {
			out[sid.String()] = true
		}
	}
	return out, nil
}

// check refuses an untrusted owner and reports whether n's DACL differs from
// want, and whether its owner is to be handed to Administrators (needsClaim).
func (t *trust) check(path string, n node, want *windows.SECURITY_DESCRIPTOR) (differs, claim bool, err error) {
	sd, err := windows.GetSecurityInfo(n.h, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, false, fmt.Errorf("dataacl: read the security of %s: %w", path, err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return false, false, fmt.Errorf("dataacl: read the owner of %s: %w", path, err)
	}
	o := owner.String()
	ok, why := trustedOwner(o, t.tokenUser, t.admins, t.indirectFn)
	if !ok {
		if why != nil {
			return false, false, fmt.Errorf("%w: %s is owned by %s (%v)", ErrUntrustedOwner, path, o, why)
		}
		return false, false, fmt.Errorf("%w: %s is owned by %s", ErrUntrustedOwner, path, o)
	}
	return !sameDACL(sd.String(), want.String()), needsClaim(o, t.tokenUser, t.claim), nil
}

// claimOwner hands path to BUILTIN\Administrators through a second handle
// opened for WRITE_OWNER (granted by the DACL just written) and checked to be
// the very object the walk inspected (same volume and file index).
func (t *trust) claimOwner(path string, n node) error {
	m, err := open(path, windows.READ_CONTROL|windows.WRITE_OWNER)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(m.h)
	if m.info.VolumeSerialNumber != n.info.VolumeSerialNumber ||
		m.info.FileIndexHigh != n.info.FileIndexHigh || m.info.FileIndexLow != n.info.FileIndexLow {
		return fmt.Errorf("%w: %s changed during the walk", ErrOutsideTree, path)
	}
	if err := setOwner(m.h, t.baSID); err != nil {
		return fmt.Errorf("dataacl: make Administrators the owner of %s: %w", path, err)
	}
	return nil
}

func setOwner(h windows.Handle, owner *windows.SID) error {
	sd, err := windows.NewSecurityDescriptor()
	if err != nil {
		return err
	}
	if err := sd.SetOwner(owner, false); err != nil {
		return err
	}
	return windows.SetKernelObjectSecurity(h, windows.OWNER_SECURITY_INFORMATION, sd)
}

// write sets want as n's DACL through its handle; no propagation.
func write(path string, n node, want *windows.SECURITY_DESCRIPTOR, protected bool) error {
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	if protected {
		info = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	if err := windows.SetKernelObjectSecurity(n.h, info, want); err != nil {
		return fmt.Errorf("dataacl: set the DACL of %s: %w", path, err)
	}
	return nil
}

type walker struct {
	t         *trust
	opt       TreeOptions
	rootFinal string
	rep       Report
}

// ProtectTree protects dir and everything under it (see the package doc). On
// an error nothing more is changed; what was already written stays (it is the
// target state). It never follows a link and never propagates by path.
func ProtectTree(dir string, opt TreeOptions) (Report, error) {
	t, err := newTrust()
	if err != nil {
		return Report{}, err
	}
	t.claim = opt.ClaimForAdministrators
	w := &walker{t: t, opt: opt}
	w.rep.AdminsErr = t.adminsErr

	root, err := open(dir, windows.READ_CONTROL|windows.WRITE_DAC)
	if err != nil {
		return w.rep, err
	}
	defer windows.CloseHandle(root.h)
	if !root.dir() {
		return w.rep, fmt.Errorf("dataacl: %s is not a directory", dir)
	}
	if w.rootFinal, err = finalPath(root.h); err != nil {
		return w.rep, err
	}
	differs, claim, err := t.check(dir, root, t.root)
	if err != nil {
		return w.rep, err
	}
	if differs {
		if err := write(dir, root, t.root, true); err != nil {
			return w.rep, err
		}
		w.rep.RootSet = true
	}
	if claim {
		if err := w.claim(dir, root); err != nil {
			return w.rep, err
		}
	}
	if err := w.children(dir, ""); err != nil {
		return w.rep, err
	}
	if t.tokenUser == SIDLocalSystem {
		w.rep.OwnerSet, w.rep.OwnerErr = setOwnerSystem(dir, w.rootFinal)
	}
	return w.rep, nil
}

// claim hands an entry to Administrators. An entry that turned out to be
// another object is a refusal (the walk stops); any other failure is
// recorded, not fatal — the owner is trusted either way.
func (w *walker) claim(path string, n node) error {
	if testHookBeforeClaim != nil {
		testHookBeforeClaim(path)
	}
	if err := w.t.claimOwner(path, n); err != nil {
		if errors.Is(err, ErrOutsideTree) {
			return err
		}
		if w.rep.ClaimErr == nil {
			w.rep.ClaimErr = err
		}
		return nil
	}
	w.rep.Claimed++
	return nil
}

// children walks the entries of the directory at path (whose handle the
// caller holds), parents before children, in name order.
func (w *walker) children(path, rel string) error {
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
		if err := w.entry(filepath.Join(path, name), filepath.Join(rel, name)); err != nil {
			return err
		}
	}
	return nil
}

func (w *walker) entry(path, rel string) error {
	if testHookBeforeEntry != nil {
		testHookBeforeEntry(path)
	}
	n, err := open(path, windows.READ_CONTROL|windows.WRITE_DAC)
	if errors.Is(err, os.ErrNotExist) {
		return nil // removed meanwhile
	}
	if err != nil {
		return err
	}
	defer windows.CloseHandle(n.h)

	final, err := finalPath(n.h)
	if err != nil {
		return err
	}
	if !strings.EqualFold(final, w.rootFinal+`\`+rel) {
		return fmt.Errorf("%w: %s is %s", ErrOutsideTree, path, final)
	}
	want := w.t.childFile
	if n.dir() {
		want = w.t.childDir
	}
	differs, claim, err := w.t.check(path, n, want)
	if err != nil {
		return err
	}
	if differs {
		if err := write(path, n, want, false); err != nil {
			return err
		}
		w.rep.Reset++
		w.rep.ResetPaths = append(w.rep.ResetPaths, path)
	}
	if claim {
		if err := w.claim(path, n); err != nil {
			return err
		}
	}
	w.rep.Entries++
	if w.opt.Progress != nil && w.rep.Entries%progressEvery == 0 {
		w.opt.Progress()
	}
	if n.dir() {
		return w.children(path, rel)
	}
	return nil
}

// setOwnerSystem makes SYSTEM the directory's owner (only called when running
// as SYSTEM: WRITE_OWNER comes with SYSTEM's Full Control ACE, written just
// before, and SYSTEM may assign itself). A second handle, checked like the
// first: same object, still no reparse point.
func setOwnerSystem(dir, rootFinal string) (bool, error) {
	n, err := open(dir, windows.READ_CONTROL|windows.WRITE_OWNER)
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(n.h)
	if final, err := finalPath(n.h); err != nil || !strings.EqualFold(final, rootFinal) {
		return false, fmt.Errorf("%w: %s changed during the walk", ErrOutsideTree, dir)
	}
	sd, err := windows.GetSecurityInfo(n.h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	if o, _, err := sd.Owner(); err == nil && o.String() == SIDLocalSystem {
		return false, nil
	}
	want, err := windows.SecurityDescriptorFromString("O:SY")
	if err != nil {
		return false, err
	}
	if err := windows.SetKernelObjectSecurity(n.h, windows.OWNER_SECURITY_INFORMATION, want); err != nil {
		return false, fmt.Errorf("dataacl: make SYSTEM the owner of %s: %w", dir, err)
	}
	return true, nil
}

// ProtectFile gives one file under the data directory the inherited DACL
// every entry there carries (ChildSDDL), by handle and with the same refusals
// as ProtectTree: a reparse point, a multiply-linked file, an untrusted owner.
// It is a no-op when the DACL already matches.
func ProtectFile(path string) error {
	t, err := newTrust()
	if err != nil {
		return err
	}
	n, err := open(path, windows.READ_CONTROL|windows.WRITE_DAC)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(n.h)
	if n.dir() {
		return fmt.Errorf("dataacl: %s is a directory", path)
	}
	differs, _, err := t.check(path, n, t.childFile)
	if err != nil || !differs {
		return err
	}
	return write(path, n, t.childFile, false)
}

// fileDeleteChild is FILE_DELETE_CHILD, which golang.org/x/sys does not name.
const fileDeleteChild = 0x40

// fileWrite and dirWrite are the rights that let someone change what a file
// runs: on the file, its content, its ACL or owner, or deleting it; on its
// folder, adding an entry (FILE_ADD_FILE and FILE_ADD_SUBDIRECTORY are the
// bits of FILE_WRITE_DATA and FILE_APPEND_DATA — and cmd.exe looks in the
// current folder first for a command a BAT names) or deleting one.
const (
	fileWrite = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.DELETE | windows.WRITE_DAC |
		windows.WRITE_OWNER | windows.GENERIC_WRITE | windows.GENERIC_ALL
	dirWrite = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | fileDeleteChild | windows.WRITE_DAC |
		windows.WRITE_OWNER | windows.GENERIC_WRITE | windows.GENERIC_ALL
)

// Writers names who, besides SYSTEM, the Administrators, TrustedInstaller and
// this process's own account, can change what path runs: an owner outside
// them, and every account an allow entry gives a write-class right on the
// file (fileWrite) or on its folder (dirWrite). OWNER RIGHTS and CREATOR
// OWNER are left out: the owner is judged itself, and CREATOR OWNER only
// shapes what a folder's new entries inherit. The connector runs its
// importer as LocalSystem for every order, so a file someone else can change
// is their code run as SYSTEM (plan 068's panel, 2026-10-20). Empty: only the
// administrators control it.
func Writers(path string) ([]string, error) {
	t, err := newTrust()
	if err != nil {
		return nil, err
	}
	out := []string{}
	seen := map[string]bool{}
	for _, o := range []struct {
		path string
		mask uint32
	}{{path, fileWrite}, {filepath.Dir(path), dirWrite}} {
		sd, err := windows.GetNamedSecurityInfo(longPath(o.path), windows.SE_FILE_OBJECT,
			windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			return nil, fmt.Errorf("dataacl: read the security of %s: %w", o.path, err)
		}
		owner, _, err := sd.Owner()
		if err != nil {
			return nil, fmt.Errorf("dataacl: read the owner of %s: %w", o.path, err)
		}
		who := []*windows.SID{owner} // an owner can always rewrite the DACL
		dacl, _, err := sd.DACL()
		if err != nil {
			return nil, fmt.Errorf("dataacl: read the DACL of %s: %w", o.path, err)
		}
		if dacl == nil { // no DACL at all: every right to everyone
			if !seen["S-1-1-0"] {
				seen["S-1-1-0"] = true
				out = append(out, "Everyone (no DACL on "+o.path+")")
			}
			continue
		}
		for i := uint32(0); i < uint32(dacl.AceCount); i++ {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(dacl, i, &ace); err != nil {
				return nil, fmt.Errorf("dataacl: read entry %d of %s: %w", i, o.path, err)
			}
			if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 ||
				uint32(ace.Mask)&o.mask == 0 {
				continue
			}
			who = append(who, (*windows.SID)(unsafe.Pointer(&ace.SidStart)))
		}
		for _, sid := range who {
			s := sid.String()
			switch s {
			case "S-1-3-0", "S-1-3-4": // CREATOR OWNER, OWNER RIGHTS
				continue
			}
			if seen[s] {
				continue
			}
			if ok, _ := trustedOwner(s, t.tokenUser, t.admins, t.indirectFn); ok {
				continue
			}
			seen[s] = true
			out = append(out, accountName(sid))
		}
	}
	return out, nil
}

// accountName is a SID as DOMAIN\name, or the SID itself when it names no
// account.
func accountName(sid *windows.SID) string {
	account, domain, _, err := sid.LookupAccount("")
	switch {
	case err != nil:
		return sid.String()
	case domain == "":
		return account
	}
	return domain + `\` + account
}

// FileTrusted reports whether path is a file this process may keep appending
// to as it is: absent (it will be created fresh), or a single-link,
// non-reparse file with a trusted owner and exactly the inherited DACL.
// Anything else should be moved aside first (MoveAside).
func FileTrusted(path string) (bool, error) {
	t, err := newTrust()
	if err != nil {
		return false, err
	}
	n, err := open(path, windows.READ_CONTROL)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if errors.Is(err, ErrReparsePoint) || errors.Is(err, ErrHardLink) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(n.h)
	differs, _, err := t.check(path, n, t.childFile)
	if errors.Is(err, ErrUntrustedOwner) {
		return false, nil
	}
	return err == nil && !differs, err
}

// OpenLog opens path for appending (creating it), never through a reparse
// point and never into a multiply-linked file: an append there would write,
// as SYSTEM, into whatever the link points at.
func OpenLog(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(longPath(path))
	if err != nil {
		return nil, err
	}
	var h windows.Handle
	err = retryShare(func() (e error) {
		h, e = windows.CreateFile(p, windows.FILE_APPEND_DATA|windows.SYNCHRONIZE, openShare, nil,
			windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		return e
	})
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	if err := refuseLinks(path, info); err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("dataacl: %s is a directory", path)
	}
	return os.NewFile(uintptr(h), path), nil
}

// FileID identifies the file object at path (volume serial and file index),
// by handle, never through a link: two IDs are equal only for the very same
// object, whatever it is called. A missing path wraps os.ErrNotExist.
func FileID(path string) (string, error) {
	n, err := open(path, windows.READ_CONTROL)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(n.h)
	return fmt.Sprintf("%08x-%08x%08x", n.info.VolumeSerialNumber, n.info.FileIndexHigh, n.info.FileIndexLow), nil
}
