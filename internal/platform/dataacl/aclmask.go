package dataacl

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// Plan 087 review T2: a lock-down never grants anything, to anyone, on any
// entry. It masks the DACL each entry already has — the folder's and every
// entry's beneath, explicit and inherited entries alike — and adds nothing:
//
//   - an allow entry for SYSTEM, Administrators or TrustedInstaller is left
//     as it is;
//   - one for a trusted writer keeps at most Modify (MaskModify);
//   - one for anyone else — CREATOR OWNER and OWNER RIGHTS included — keeps
//     at most read & execute (MaskReadExecute);
//   - generic rights are mapped to the file-specific ones first, and an
//     entry left with nothing beyond SYNCHRONIZE is dropped;
//   - every entry keeps its type (a conditional, callback or object entry
//     keeps its condition and object types), its flags and its place; deny
//     entries, and every entry that is not an allow, stay byte for byte.
//
// MaskDACL works on a DACL's raw bytes (the Windows ACL layout, the same on
// every platform), so its property test — no principal ends with a right it
// did not have — runs everywhere.

// The rights a lock-down leaves (icacls' M and RX), and SYNCHRONIZE.
const (
	MaskModify      uint32 = 0x1301bf // FILE_GENERIC_READ | FILE_GENERIC_WRITE | FILE_GENERIC_EXECUTE | DELETE
	MaskReadExecute uint32 = 0x1200a9 // FILE_GENERIC_READ | FILE_GENERIC_EXECUTE
	maskSynchronize uint32 = 0x100000
)

// The generic rights and what they map to for a file or a folder.
const (
	genericRead    uint32 = 0x80000000
	genericWrite   uint32 = 0x40000000
	genericExecute uint32 = 0x20000000
	genericAll     uint32 = 0x10000000
	fileGenRead    uint32 = 0x120089
	fileGenWrite   uint32 = 0x120116
	fileGenExecute uint32 = 0x1200a0
	fileAllAccess  uint32 = 0x1f01ff
)

// MapGenericFile maps a mask's generic rights to the file-specific ones.
func MapGenericFile(m uint32) uint32 {
	if m&genericRead != 0 {
		m |= fileGenRead
	}
	if m&genericWrite != 0 {
		m |= fileGenWrite
	}
	if m&genericExecute != 0 {
		m |= fileGenExecute
	}
	if m&genericAll != 0 {
		m |= fileAllAccess
	}
	return m &^ (genericRead | genericWrite | genericExecute | genericAll)
}

// ACE types and flags (winnt.h).
const (
	aceTypeAllowed      = 0x0
	aceTypeDenied       = 0x1
	aceTypeAllowedObj   = 0x5
	aceTypeAllowedCB    = 0x9
	aceTypeAllowedCBObj = 0xB
	aceFlagInheritOnly  = 0x08
	aceFlagInherited    = 0x10
	aceObjectTypeBit    = 0x1
	aceInheritedTypeBit = 0x2
	aclHeaderSize       = 8
)

// ACE is one entry of a raw DACL, as MaskDACL and the summaries read it.
type ACE struct {
	Type  byte
	Flags byte
	Mask  uint32
	// SID is the grantee ("" when the entry carries none MaskDACL reads:
	// audit, label, …).
	SID string
	raw []byte
}

// Allow reports whether the entry grants (plain, object, callback or
// callback-object allow).
func (a ACE) Allow() bool {
	switch a.Type {
	case aceTypeAllowed, aceTypeAllowedObj, aceTypeAllowedCB, aceTypeAllowedCBObj:
		return true
	}
	return false
}

// InheritOnly reports whether the entry applies only to what inherits it.
func (a ACE) InheritOnly() bool { return a.Flags&aceFlagInheritOnly != 0 }

// sidAt is the SID string at b and its length.
func sidAt(b []byte) (string, int, error) {
	if len(b) < 8 || b[0] != 1 || int(b[1]) > 15 || len(b) < 8+4*int(b[1]) {
		return "", 0, errors.New("dataacl: a malformed SID in an ACE")
	}
	var auth uint64
	for _, c := range b[2:8] {
		auth = auth<<8 | uint64(c)
	}
	var s strings.Builder
	if auth < 1<<32 {
		fmt.Fprintf(&s, "S-1-%d", auth)
	} else {
		fmt.Fprintf(&s, "S-1-0x%012X", auth)
	}
	n := int(b[1])
	for i := 0; i < n; i++ {
		fmt.Fprintf(&s, "-%d", binary.LittleEndian.Uint32(b[8+4*i:]))
	}
	return s.String(), 8 + 4*n, nil
}

// ParseACL splits a raw DACL into its entries.
func ParseACL(acl []byte) ([]ACE, error) {
	if len(acl) < aclHeaderSize {
		return nil, errors.New("dataacl: an ACL shorter than its header")
	}
	size := int(binary.LittleEndian.Uint16(acl[2:]))
	count := int(binary.LittleEndian.Uint16(acl[4:]))
	if size < aclHeaderSize || size > len(acl) {
		return nil, errors.New("dataacl: an ACL whose size does not fit")
	}
	out := make([]ACE, 0, count)
	off := aclHeaderSize
	for i := 0; i < count; i++ {
		if off+4 > size {
			return nil, fmt.Errorf("dataacl: ACE %d runs past the ACL", i)
		}
		n := int(binary.LittleEndian.Uint16(acl[off+2:]))
		if n < 8 || off+n > size {
			return nil, fmt.Errorf("dataacl: ACE %d has a bad size %d", i, n)
		}
		raw := acl[off : off+n]
		a := ACE{Type: raw[0], Flags: raw[1], Mask: binary.LittleEndian.Uint32(raw[4:]), raw: raw}
		sidOff := -1
		switch a.Type {
		case aceTypeAllowed, aceTypeDenied, aceTypeAllowedCB, 0xA /* denied callback */ :
			sidOff = 8
		case aceTypeAllowedObj, 0x6 /* denied object */, aceTypeAllowedCBObj, 0xC /* denied callback object */ :
			if n < 12 {
				return nil, fmt.Errorf("dataacl: object ACE %d is too short", i)
			}
			flags := binary.LittleEndian.Uint32(raw[8:])
			sidOff = 12
			if flags&aceObjectTypeBit != 0 {
				sidOff += 16
			}
			if flags&aceInheritedTypeBit != 0 {
				sidOff += 16
			}
		case 0x4: // compound allow (reserved, never on a file)
			return nil, errors.New("dataacl: a compound ACE (reserved): not read")
		}
		if sidOff >= 0 {
			if sidOff > n {
				return nil, fmt.Errorf("dataacl: ACE %d is too short for its SID", i)
			}
			s, _, err := sidAt(raw[sidOff:])
			if err != nil {
				return nil, fmt.Errorf("ACE %d: %w", i, err)
			}
			a.SID = s
		}
		out = append(out, a)
		off += n
	}
	return out, nil
}

// lockdownExempt are left as they are: SYSTEM, Administrators,
// TrustedInstaller.
func lockdownExempt(sid string) bool {
	switch sid {
	case SIDLocalSystem, SIDAdministrators, SIDTrustedInstaller:
		return true
	}
	return false
}

// LockdownCap is the most a lock-down leaves an allow entry for sid: none to
// cut (0xFFFFFFFF) for SYSTEM, Administrators and TrustedInstaller; Modify
// for a trusted writer; read & execute for anyone else.
func LockdownCap(sid string, trusted map[string]bool) uint32 {
	switch {
	case lockdownExempt(sid):
		return 0xFFFFFFFF
	case trusted[sid]:
		return MaskModify
	}
	return MaskReadExecute
}

// MaskDACL is a DACL lowered for a lock-down (see above) — never raised: the
// result has the same entries in the same order, each allow entry's mask
// mapped and capped (LockdownCap), allow entries left with nothing beyond
// SYNCHRONIZE dropped, everything else unchanged. explicit (the folder
// itself, whose link to its parent is cut) turns inherited entries into
// explicit ones. changed reports any difference.
func MaskDACL(acl []byte, trusted map[string]bool, explicit bool) (out []byte, changed bool, err error) {
	aces, err := ParseACL(acl)
	if err != nil {
		return nil, false, err
	}
	body := make([]byte, 0, len(acl))
	kept := 0
	for _, a := range aces {
		raw := append([]byte(nil), a.raw...)
		if a.Allow() && !lockdownExempt(a.SID) {
			m := MapGenericFile(a.Mask) & LockdownCap(a.SID, trusted)
			if m&^maskSynchronize == 0 {
				changed = true
				continue // nothing left to grant
			}
			if m != a.Mask {
				binary.LittleEndian.PutUint32(raw[4:], m)
				changed = true
			}
		}
		if explicit && raw[1]&aceFlagInherited != 0 {
			raw[1] &^= aceFlagInherited
			changed = true
		}
		body = append(body, raw...)
		kept++
	}
	out = make([]byte, aclHeaderSize, aclHeaderSize+len(body))
	copy(out, acl[:aclHeaderSize])
	binary.LittleEndian.PutUint16(out[2:], uint16(aclHeaderSize+len(body)))
	binary.LittleEndian.PutUint16(out[4:], uint16(kept))
	return append(out, body...), changed, nil
}

// GrantedTo is what the entries of acl that apply to the object itself (not
// inherit-only) allow sid, generic rights mapped — the union of its allow
// entries' masks, deny entries aside (the summaries read what is granted).
func GrantedTo(aces []ACE, sid string) uint32 {
	var m uint32
	for _, a := range aces {
		if a.Allow() && !a.InheritOnly() && a.SID == sid {
			m |= MapGenericFile(a.Mask)
		}
	}
	return m
}

// RightsWord says what a mask lets its holder do with a file or a folder:
// "full", "modify", "write", "read", "special" or "none".
func RightsWord(mask uint32) string {
	mask = MapGenericFile(mask)
	const writeBits = 0x2 | 0x4 | 0x40 | 0x10000 | 0x40000 | 0x80000 // write data, append, delete child, DELETE, WRITE_DAC, WRITE_OWNER
	switch {
	case mask&^maskSynchronize == 0:
		return "none"
	case mask&fileAllAccess == fileAllAccess:
		return "full"
	case mask&0x10000 != 0 && mask&0x2 != 0:
		return "modify"
	case mask&writeBits != 0:
		return "write"
	case mask&(0x1|0x20) != 0:
		return "read"
	}
	return "special"
}
