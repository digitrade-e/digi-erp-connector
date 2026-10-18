//go:build windows

package dataacl

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ProtectDir sets the protected data-directory DACL on dir. Windows
// propagates its inheritable ACEs to everything already under dir (an
// existing child's own explicit ACEs are kept); new children inherit them.
func ProtectDir(dir string) error { return protect(dir, true) }

// ProtectFile sets the protected DACL on one file.
func ProtectFile(path string) error { return protect(path, false) }

func tokenUserSID() (string, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("dataacl: read the process token user: %w", err)
	}
	return tu.User.Sid.String(), nil
}

func protect(path string, container bool) error {
	sid, err := tokenUserSID()
	if err != nil {
		return err
	}
	sddl, err := SDDL(sid, container)
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return fmt.Errorf("dataacl: parse %s: %w", sddl, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("dataacl: %w", err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("dataacl: set the protected DACL on %s: %w", path, err)
	}
	return nil
}

// ProtectTree is ProtectDir plus what it cannot do alone. First the whole tree
// is scanned without following links: dir itself or anything under it that is
// a symlink or junction is ErrReparsePoint, and nothing is changed. Then
// ProtectDir. Then every entry whose own DACL survived the propagation — a
// protected one, a NULL one, or one with explicit ACEs, all of which a child
// created before the first protected start (by anyone) may carry — is reset to
// the inherited DACL alone; an entry already inheriting only is just read.
// Finally, when running as SYSTEM, SYSTEM becomes the directory's owner (a
// failure there is reported, not returned).
func ProtectTree(dir string) (Report, error) {
	entries, err := scanTree(dir)
	if err != nil {
		return Report{}, err
	}
	if err := ProtectDir(dir); err != nil {
		return Report{}, err
	}
	rep := Report{Entries: len(entries)}
	for _, p := range entries {
		fi, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue // removed meanwhile
		}
		if err != nil {
			return rep, err
		}
		if refusedMode(fi.Mode()) { // planted between the scan and now
			return rep, fmt.Errorf("%w: %s", ErrReparsePoint, p)
		}
		own, err := hasOwnDACL(p)
		if err != nil {
			return rep, err
		}
		if !own {
			continue
		}
		if err := resetToInherited(p); err != nil {
			return rep, err
		}
		rep.Reset++
	}

	if sid, err := tokenUserSID(); err == nil && ownAsSystem(sid) {
		rep.OwnerErr = setOwnerSystem(dir)
		rep.OwnerSet = rep.OwnerErr == nil
	}
	return rep, nil
}

// hasOwnDACL reports whether path's DACL is anything but inherited ACEs.
func hasOwnDACL(path string) (bool, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, fmt.Errorf("dataacl: read the DACL of %s: %w", path, err)
	}
	ctl, _, err := sd.Control()
	if err != nil {
		return false, err
	}
	if ctl&windows.SE_DACL_PROTECTED != 0 {
		return true, nil
	}
	dacl, _, err := sd.DACL()
	if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) || (err == nil && dacl == nil) {
		return true, nil // no DACL / NULL DACL: everyone has full access
	}
	if err != nil {
		return false, err
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return false, fmt.Errorf("dataacl: read an ACE of %s: %w", path, err)
		}
		if ace.Header.AceFlags&windows.INHERITED_ACE == 0 {
			return true, nil
		}
	}
	return false, nil
}

// emptyACL is a valid ACL with no entries (revision 2, size 8) — never a NULL
// DACL, which would grant everyone everything.
var emptyACL = func() []byte {
	b := make([]byte, 8)
	b[0] = 2 // ACL_REVISION
	binary.LittleEndian.PutUint16(b[2:], 8)
	return b
}()

// resetToInherited drops path's own DACL: unprotected, no explicit ACEs, so it
// carries what it inherits from the protected directory and nothing else.
func resetToInherited(path string) error {
	acl := (*windows.ACL)(unsafe.Pointer(&emptyACL[0]))
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		return fmt.Errorf("dataacl: reset the DACL of %s to the inherited one: %w", path, err)
	}
	return nil
}

func setOwnerSystem(dir string) error {
	sys, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION,
		sys, nil, nil, nil); err != nil {
		return fmt.Errorf("dataacl: make SYSTEM the owner of %s: %w", dir, err)
	}
	return nil
}
