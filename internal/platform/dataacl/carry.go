package dataacl

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Connector v1.0.32 (plan 087/088 follow-up): carry-over at a lock-down. A
// trusted writer (documents.trustedWriters — at BFL the ERP's scheduler)
// that wrote into a folder only through a group (Users) lost its write when
// the lock-down masked the group (trusted_writer_no_write). With carryOver
// the lock-down adds, per such writer per entry, ONE explicit allow entry
// whose rights are exactly what that account effectively had on the entry
// before — AuthZ on the entry's old descriptor, the account's groups
// resolved — intersected with Modify. So its effective rights never grow:
//
//   - on the entry: the carried rights are a part of what it had, and every
//     other entry is masked as before (MaskDACL never grants);
//   - on what a folder hands down: the entry is inheritable to files (OI)
//     or folders (CI) only when, before, the account would have been handed
//     at least those rights there too (inheritedCover: a lower bound — an
//     allow it may not get counts for nothing, a deny it may get counts);
//     a file's entry is never inheritable;
//   - an entry whose old descriptor holds a conditional (callback) entry for
//     the account gets nothing carried (its rights depend on claims AuthZ
//     cannot see here).
//
// The carried entry sits after the entry's leading explicit denies (the
// canonical place); since its rights are a part of what the account had,
// its place cannot add any.

// CarryCapable reports whether a carried entry may be computed for an
// account on an entry whose DACL is aces: no conditional (callback) entry
// applies to it (allow and deny are the account's SIDs for allows and for
// denies: its user SID and its groups).
func CarryCapable(aces []ACE, allow, deny map[string]bool) bool {
	for _, a := range aces {
		if isCallbackType(a.Type) && (allow[a.SID] || deny[a.SID]) {
			return false
		}
	}
	return true
}

// DeniesUnaccounted reports whether aces hold a deny entry for a SID the
// account's AuthZ context cannot account for (review C-1): AuthZ's view of
// an account is not its real token — no logon-type or session SIDs
// (INTERACTIVE, BATCH, SERVICE, NETWORK, LOCAL, the logon SID, …), and
// domain groups it could not read go missing silently — so a deny naming
// such a SID is invisible to it, and carrying what AuthZ reports would grant
// what the deny takes. A SID counts as accounted for when the context holds
// it (deny), when it is a builtin alias (S-1-5-32-*, resolved locally), the
// machine's own accounts (SYSTEM, LOCAL SERVICE, NETWORK SERVICE), a
// creator or owner SID (they match nobody on the entry itself, AuthZ judges
// OWNER RIGHTS, inheritedCover counts creator denies), or another user
// account (isUser: a user is nobody's member). Anything else — or a lookup
// that fails — is unaccounted: carry nothing on that entry.
func DeniesUnaccounted(aces []ACE, deny map[string]bool, isUser func(sid string) (bool, error)) bool {
	for _, a := range aces {
		if !isDenyType(a.Type) || a.SID == "" || deny[a.SID] {
			continue
		}
		switch {
		case strings.HasPrefix(a.SID, "S-1-5-32-"), a.SID == SIDLocalSystem, a.SID == "S-1-5-19", a.SID == "S-1-5-20", strings.HasPrefix(a.SID, "S-1-3-"):
			continue
		}
		if isUser != nil {
			if user, err := isUser(a.SID); err == nil && user {
				continue
			}
		}
		return true
	}
	return false
}

func isCallbackType(t byte) bool { return t == 0x9 || t == 0xA || t == 0xB || t == 0xC }

func isDenyType(t byte) bool { return t == aceTypeDenied || t == 0x6 || t == 0xA || t == 0xC }

// hasObjectType reports whether an object entry carries an object type GUID
// (then it applies to that property or child class, not to the object).
func hasObjectType(a ACE) bool {
	switch a.Type {
	case aceTypeAllowedObj, 0x6, aceTypeAllowedCBObj, 0xC:
		return len(a.raw) >= 12 && binary.LittleEndian.Uint32(a.raw[8:])&aceObjectTypeBit != 0
	}
	return false
}

// SIDs whose meaning on a new child is not the account's own: CREATOR OWNER,
// CREATOR GROUP and OWNER RIGHTS become the creator or the owner.
func creatorSID(sid string) bool {
	return sid == "S-1-3-0" || sid == "S-1-3-1" || sid == "S-1-3-4"
}

// inheritedCover is a lower bound of the rights an account (allow: its SIDs
// for allows, deny: for denies) is handed, from aces, on every new file
// (files) or every new folder (!files) beneath, at every depth. In order; an
// allow counts only when it surely applies at every depth (it propagates —
// no NO_PROPAGATE_INHERIT —, it is not conditional, has no object type, and
// names no creator or owner SID), a deny whenever it may apply at some depth
// (NO_PROPAGATE_INHERIT ones too: they reach the first level).
func inheritedCover(aces []ACE, allow, deny map[string]bool, files bool) uint32 {
	var allowed, denied uint32
	for _, a := range aces {
		reaches := a.Flags&0x2 != 0 // CONTAINER_INHERIT: the folders beneath
		if files {
			reaches = a.Flags&0x1 != 0 // OBJECT_INHERIT: the files beneath (through folders too)
		}
		if !reaches {
			continue
		}
		m := MapGenericFile(a.Mask)
		switch {
		case isDenyType(a.Type):
			if deny[a.SID] || creatorSID(a.SID) {
				denied |= m &^ allowed
			}
		case a.Allow():
			if a.Flags&0x4 == 0 && allow[a.SID] && !isCallbackType(a.Type) && !hasObjectType(a) && !creatorSID(a.SID) {
				allowed |= m &^ denied
			}
		}
	}
	return allowed
}

// CarryFlags are the inheritance flags of a carried entry with rights on a
// folder whose old DACL is aces: OBJECT_INHERIT when every new file beneath
// would have been handed at least rights, CONTAINER_INHERIT likewise for
// every new folder. A file's carried entry has none.
func CarryFlags(aces []ACE, allow, deny map[string]bool, rights uint32, folder bool) byte {
	if !folder {
		return 0
	}
	var flags byte
	if rights&^inheritedCover(aces, allow, deny, true) == 0 {
		flags |= 0x1
	}
	if rights&^inheritedCover(aces, allow, deny, false) == 0 {
		flags |= 0x2
	}
	return flags
}

// writeBits are the rights that let an account change what is in a file or
// folder (write data / add file, append / add folder, its attributes and
// extended attributes, DELETE).
const writeBits = 0x2 | 0x4 | 0x10 | 0x100 | 0x10000

// CarryRights is the carried entry for an account that effectively had had
// on an entry whose old DACL is aces (review item 5, the scheduler's case):
// had ∩ Modify, inheritable as CarryFlags allows. On a folder whose old DACL
// handed the account its write down to files — the group's entry was
// inheritable — but where had holds a little more that was not handed down
// (an entry for the folder alone), the rights are narrowed to what was
// handed down, so the entry stays inheritable to files: otherwise the
// scheduler could not rename or delete the files written there after the
// lock-down (Modify holds no delete-child). The folder keeps every write bit
// it had. Never more than had, nor than what was handed down.
func CarryRights(aces []ACE, allow, deny map[string]bool, had uint32, folder bool) (uint32, byte) {
	r := had & MaskModify
	flags := CarryFlags(aces, allow, deny, r, folder)
	if !folder || flags&0x1 != 0 {
		return r, flags
	}
	files := inheritedCover(aces, allow, deny, true)
	if narrowed := r & files; r&writeBits&^files == 0 && narrowed&writeBits != 0 {
		return narrowed, CarryFlags(aces, allow, deny, narrowed, true)
	}
	return r, flags
}

// sidBinary is a SID string's binary form.
func sidBinary(s string) ([]byte, error) {
	parts := strings.Split(s, "-")
	if len(parts) < 3 || parts[0] != "S" || parts[1] != "1" || len(parts)-3 > 15 {
		return nil, fmt.Errorf("dataacl: %q is not a SID", s)
	}
	auth, err := strconv.ParseUint(parts[2], 10, 48)
	if err != nil {
		return nil, fmt.Errorf("dataacl: %q is not a SID", s)
	}
	b := []byte{1, byte(len(parts) - 3), 0, 0, 0, 0, 0, 0}
	for i := 7; i >= 2; i-- {
		b[i] = byte(auth)
		auth >>= 8
	}
	for _, p := range parts[3:] {
		v, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("dataacl: %q is not a SID", s)
		}
		b = binary.LittleEndian.AppendUint32(b, uint32(v))
	}
	return b, nil
}

// CarryACE is one explicit allow entry for sid with rights and flags.
func CarryACE(sid string, rights uint32, flags byte) ([]byte, error) {
	sb, err := sidBinary(sid)
	if err != nil {
		return nil, err
	}
	b := []byte{aceTypeAllowed, flags, 0, 0}
	b = binary.LittleEndian.AppendUint32(b, rights)
	b = append(b, sb...)
	binary.LittleEndian.PutUint16(b[2:], uint16(len(b)))
	return b, nil
}

// InsertCarried puts aces into acl after its leading explicit deny entries
// (the canonical place of an explicit allow).
func InsertCarried(acl []byte, aces [][]byte) ([]byte, error) {
	if len(aces) == 0 {
		return acl, nil
	}
	parsed, err := ParseACL(acl)
	if err != nil {
		return nil, err
	}
	at := aclHeaderSize
	for _, a := range parsed {
		if !isDenyType(a.Type) || a.Flags&aceFlagInherited != 0 {
			break
		}
		at += len(a.raw)
	}
	size := int(binary.LittleEndian.Uint16(acl[2:]))
	var add []byte
	for _, a := range aces {
		add = append(add, a...)
	}
	if size+len(add) > 0xFFFF {
		return nil, errors.New("dataacl: the DACL would grow past 64 KiB")
	}
	out := make([]byte, 0, size+len(add))
	out = append(out, acl[:at]...)
	out = append(out, add...)
	out = append(out, acl[at:size]...)
	binary.LittleEndian.PutUint16(out[2:], uint16(size+len(add)))
	binary.LittleEndian.PutUint16(out[4:], uint16(len(parsed)+len(aces)))
	return out, nil
}

// MapGenericACL is acl with every entry's generic rights mapped to the file
// ones (AuthZ does not map them: an entry's GENERIC_ALL would count for
// nothing).
func MapGenericACL(acl []byte) ([]byte, error) {
	parsed, err := ParseACL(acl)
	if err != nil {
		return nil, err
	}
	out := append([]byte(nil), acl...)
	off := aclHeaderSize
	for _, a := range parsed {
		binary.LittleEndian.PutUint32(out[off+4:], MapGenericFile(a.Mask))
		off += len(a.raw)
	}
	return out, nil
}

// CarriedRights is one account's carried entries across a lock-down's tree:
// the strongest rights carried, on how many entries, and on how many none
// was computed (a conditional entry for it).
type CarriedRights struct {
	SID     string `json:"sid"`
	Name    string `json:"name"`
	Rights  string `json:"rights"`
	Entries int    `json:"entries"`
	Skipped int    `json:"skipped,omitempty"`
	// NotInheritable: folders whose carried entry is not handed down to new
	// files (the old DACL did not hand the account its write down): it may
	// not rename or delete what is written there after the lock-down.
	NotInheritable int `json:"notInheritable,omitempty"`
}

// CarryFailure is an account for which no carry-over was possible: its
// groups could not be resolved (a domain controller that does not answer).
type CarryFailure struct {
	SID   string `json:"sid"`
	Name  string `json:"name"`
	Error string `json:"error"`
}
