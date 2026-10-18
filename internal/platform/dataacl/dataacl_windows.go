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
	"unsafe"

	"golang.org/x/sys/windows"
)

// Every open: never follow a reparse point, open directories too, and share
// read/write but not delete — so while a handle is held nobody can rename or
// replace that entry (or, for a directory, any of the walk's ancestors).
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

// open opens path by handle without following it and refuses a reparse
// point and a multiply-linked file. A missing path wraps os.ErrNotExist.
func open(path string, access uint32) (node, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return node{}, err
	}
	h, err := windows.CreateFile(p, access, openShare, nil, windows.OPEN_EXISTING, openFlags, 0)
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
	tokenUser string
	account   string // the extra SID in the templates ("" = none)
	admins    map[string]bool
	adminsErr error
	root      *windows.SECURITY_DESCRIPTOR
	childDir  *windows.SECURITY_DESCRIPTOR
	childFile *windows.SECURITY_DESCRIPTOR
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
	t.admins, t.adminsErr = localAdministrators(ba)

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
)

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
// want.
func (t *trust) check(path string, n node, want *windows.SECURITY_DESCRIPTOR) (bool, error) {
	sd, err := windows.GetSecurityInfo(n.h, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, fmt.Errorf("dataacl: read the security of %s: %w", path, err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return false, fmt.Errorf("dataacl: read the owner of %s: %w", path, err)
	}
	if o := owner.String(); !trustedOwner(o, t.tokenUser, t.admins) {
		return false, fmt.Errorf("%w: %s is owned by %s", ErrUntrustedOwner, path, o)
	}
	return !sameDACL(sd.String(), want.String()), nil
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
	differs, err := t.check(dir, root, t.root)
	if err != nil {
		return w.rep, err
	}
	if differs {
		if err := write(dir, root, t.root, true); err != nil {
			return w.rep, err
		}
		w.rep.RootSet = true
	}
	if err := w.children(dir, ""); err != nil {
		return w.rep, err
	}
	if t.tokenUser == SIDLocalSystem {
		w.rep.OwnerSet, w.rep.OwnerErr = setOwnerSystem(dir, w.rootFinal)
	}
	return w.rep, nil
}

// children walks the entries of the directory at path (whose handle the
// caller holds), parents before children, in name order.
func (w *walker) children(path, rel string) error {
	f, err := os.Open(path)
	if err != nil {
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
	differs, err := w.t.check(path, n, want)
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
	differs, err := t.check(path, n, t.childFile)
	if err != nil || !differs {
		return err
	}
	return write(path, n, t.childFile, false)
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
	differs, err := t.check(path, n, t.childFile)
	if errors.Is(err, ErrUntrustedOwner) {
		return false, nil
	}
	return err == nil && !differs, err
}

// OpenLog opens path for appending (creating it), never through a reparse
// point and never into a multiply-linked file: an append there would write,
// as SYSTEM, into whatever the link points at.
func OpenLog(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.FILE_APPEND_DATA|windows.SYNCHRONIZE, openShare, nil,
		windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
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
