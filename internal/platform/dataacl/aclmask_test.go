package dataacl

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

// sidBytes is a SID string's binary form (test builder).
func sidBytes(t testing.TB, s string) []byte {
	t.Helper()
	parts := strings.Split(s, "-")
	if len(parts) < 3 || parts[0] != "S" || parts[1] != "1" {
		t.Fatalf("not a SID: %s", s)
	}
	auth, err := strconv.ParseUint(parts[2], 10, 48)
	if err != nil {
		t.Fatal(err)
	}
	b := []byte{1, byte(len(parts) - 3), 0, 0, 0, 0, 0, 0}
	for i := 7; i >= 2; i-- {
		b[i] = byte(auth)
		auth >>= 8
	}
	for _, p := range parts[3:] {
		v, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			t.Fatal(err)
		}
		b = binary.LittleEndian.AppendUint32(b, uint32(v))
	}
	return b
}

// testACE builds one raw ACE: guids is how many object GUIDs an object type
// carries (0-2); app is callback data after the SID.
func testACE(t testing.TB, typ, flags byte, mask uint32, sid string, guids int, app []byte) []byte {
	t.Helper()
	b := []byte{typ, flags, 0, 0}
	b = binary.LittleEndian.AppendUint32(b, mask)
	switch typ {
	case aceTypeAllowedObj, 0x6, aceTypeAllowedCBObj, 0xC:
		var f uint32
		if guids > 0 {
			f |= aceObjectTypeBit
		}
		if guids > 1 {
			f |= aceInheritedTypeBit
		}
		b = binary.LittleEndian.AppendUint32(b, f)
		for i := 0; i < guids; i++ {
			b = append(b, bytes.Repeat([]byte{byte(0xA0 + i)}, 16)...)
		}
	}
	b = append(b, sidBytes(t, sid)...)
	b = append(b, app...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	binary.LittleEndian.PutUint16(b[2:], uint16(len(b)))
	return b
}

func testACL(aces ...[]byte) []byte {
	body := bytes.Join(aces, nil)
	h := make([]byte, aclHeaderSize)
	h[0] = 4 // ACL_REVISION_DS: object ACEs allowed
	binary.LittleEndian.PutUint16(h[2:], uint16(aclHeaderSize+len(body)))
	binary.LittleEndian.PutUint16(h[4:], uint16(len(aces)))
	return append(h, body...)
}

const (
	sidCO       = "S-1-3-0" // CREATOR OWNER
	sidOW       = "S-1-3-4" // OWNER RIGHTS
	sidEveryone = "S-1-1-0"
	sidUsers    = "S-1-5-32-545"
	sidTrusted  = "S-1-5-21-1-2-3-1105"
	sidOther    = "S-1-5-21-1-2-3-1106"
)

var poolSIDs = []string{SIDLocalSystem, SIDAdministrators, SIDTrustedInstaller, sidCO, sidOW, sidEveryone, sidUsers, sidTrusted, sidOther}

func isCallback(typ byte) bool { return typ == 0x9 || typ == 0xA || typ == 0xB || typ == 0xC }
func isDeny(typ byte) bool     { return typ == aceTypeDenied || typ == 0x6 || typ == 0xA || typ == 0xC }

// accessFor walks a DACL as Windows does for a token asking for everything:
// a deny entry denies what is not granted yet, an allow entry grants what is
// not denied yet; a callback (conditional) entry counts only when its
// condition holds. view "" is the object; "file"/"dir" a new child, which
// inherits the entries marked OI / CI.
func accessFor(aces []ACE, token map[string]bool, condTrue bool, view string) uint32 {
	var allowed, denied uint32
	for _, a := range aces {
		switch view {
		case "":
			if a.InheritOnly() {
				continue
			}
		case "file":
			if a.Flags&0x1 == 0 { // OBJECT_INHERIT
				continue
			}
		case "dir":
			if a.Flags&0x2 == 0 { // CONTAINER_INHERIT
				continue
			}
		}
		if !token[a.SID] || (isCallback(a.Type) && !condTrue) {
			continue
		}
		m := MapGenericFile(a.Mask)
		switch {
		case isDeny(a.Type):
			denied |= m &^ allowed
		case a.Allow():
			allowed |= m &^ denied
		}
	}
	return allowed
}

// Review T2's property: for generated DACLs — generic, conditional, object,
// CREATOR OWNER, OWNER RIGHTS, inherit-only and inherited entries, deny
// entries among them — no token gains a right after MaskDACL, on the object
// or on a child it would create; no entry is added; deny entries and those
// of SYSTEM, Administrators and TrustedInstaller are byte for byte the same;
// every other allow entry is within its cap.
func TestMaskDACL_NeverGrants(t *testing.T) {
	rng := rand.New(rand.NewSource(87))
	types := []byte{aceTypeAllowed, aceTypeAllowed, aceTypeAllowed, aceTypeDenied, aceTypeAllowedObj, aceTypeAllowedCB, aceTypeAllowedCBObj, 0x6, 0xA}
	masks := []uint32{0x1, 0x2, 0x4, 0x6, 0x40, 0x10000, 0x40000, 0x80000, 0x100000, 0x120089, 0x1200a9, 0x1301bf, 0x1f01ff,
		genericRead, genericWrite, genericExecute, genericAll, genericAll | genericRead, 0x2 | genericRead, 0x02000000}
	flagsPool := []byte{0, 0x1, 0x2, 0x3, 0x3 | 0x8, 0x1 | 0x8, 0x2 | 0x8, 0x3 | 0x4, 0x10, 0x3 | 0x10, 0x3 | 0x8 | 0x10}
	cond := []byte("artx\x01\x02\x03\x04") // a conditional entry's data
	for round := 0; round < 3000; round++ {
		var raws [][]byte
		for n := rng.Intn(9); n >= 0; n-- {
			typ := types[rng.Intn(len(types))]
			var app []byte
			if isCallback(typ) {
				app = cond
			}
			raws = append(raws, testACE(t, typ, flagsPool[rng.Intn(len(flagsPool))], masks[rng.Intn(len(masks))]|masks[rng.Intn(len(masks))]*uint32(rng.Intn(2)),
				poolSIDs[rng.Intn(len(poolSIDs))], rng.Intn(3), app))
		}
		acl := testACL(raws...)
		trusted := map[string]bool{}
		if rng.Intn(2) == 0 {
			trusted[sidTrusted] = true
		}
		explicit := rng.Intn(2) == 0
		out, _, err := MaskDACL(acl, trusted, explicit)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		before, err := ParseACL(acl)
		if err != nil {
			t.Fatal(err)
		}
		after, err := ParseACL(out)
		if err != nil {
			t.Fatalf("round %d: the result does not parse: %v", round, err)
		}
		if len(after) > len(before) {
			t.Fatalf("round %d: %d entries became %d", round, len(before), len(after))
		}
		// Walk both in step: each entry before is dropped (an allow left with
		// nothing beyond SYNCHRONIZE) or is the next entry after — a deny or
		// exempt one byte for byte, any other allow within its cap and its
		// old rights, its type and flags kept.
		k := 0
		for _, b := range before {
			if b.Allow() && !lockdownExempt(b.SID) && (MapGenericFile(b.Mask)&LockdownCap(b.SID, trusted))&^maskSynchronize == 0 {
				continue
			}
			if k == len(after) {
				t.Fatalf("round %d: entry %+v is missing after", round, b)
			}
			a := after[k]
			k++
			want := append([]byte(nil), b.raw...)
			if explicit {
				want[1] &^= aceFlagInherited
			}
			if !b.Allow() || lockdownExempt(b.SID) {
				if !bytes.Equal(a.raw, want) {
					t.Fatalf("round %d: a deny or exempt entry changed: %x → %x", round, b.raw, a.raw)
				}
				continue
			}
			binary.LittleEndian.PutUint32(want[4:], MapGenericFile(b.Mask)&LockdownCap(b.SID, trusted))
			if !bytes.Equal(a.raw, want) || a.Mask&^LockdownCap(a.SID, trusted) != 0 || a.Mask&^MapGenericFile(b.Mask) != 0 {
				t.Fatalf("round %d: %+v from %+v is not the entry capped", round, a, b)
			}
		}
		if k != len(after) {
			t.Fatalf("round %d: %d entries after, %d accounted for: one was added", round, len(after), k)
		}
		// No token gains anything, on the object or a new child.
		for k := 0; k < 12; k++ {
			token := map[string]bool{}
			for _, s := range poolSIDs {
				if rng.Intn(3) == 0 {
					token[s] = true
				}
			}
			for _, c := range []bool{true, false} {
				for _, view := range []string{"", "file", "dir"} {
					if gained := accessFor(after, token, c, view) &^ accessFor(before, token, c, view); gained != 0 {
						t.Fatalf("round %d: token %v (condition %v, view %q) gained %#x\n before %+v\n after  %+v", round, token, c, view, gained, before, after)
					}
				}
			}
		}
	}
}

// The review's cases: a trusted writer with no entry gets none (nothing is
// added); a drop-folder account that could only add files gains no read; a
// conditional entry stays conditional; Users' Modify becomes read & execute.
func TestMaskDACL_Cases(t *testing.T) {
	cond := []byte("artx\x01\x02\x03\x04")
	acl := testACL(
		testACE(t, aceTypeAllowed, 0x3, fileAllAccess, SIDLocalSystem, 0, nil),
		testACE(t, aceTypeAllowed, 0x3, MaskModify, sidUsers, 0, nil),
		testACE(t, aceTypeAllowed, 0x3|0x10, 0x2, sidOther, 0, nil), // could only add files (inherited)
		testACE(t, aceTypeAllowedCB, 0x3, MaskModify, sidEveryone, 0, cond),
		testACE(t, aceTypeAllowed, 0x3|0x8, genericAll, sidCO, 0, nil),
	)
	out, changed, err := MaskDACL(acl, map[string]bool{sidTrusted: true}, true)
	if err != nil || !changed {
		t.Fatalf("MaskDACL: %v %v", changed, err)
	}
	after, _ := ParseACL(out)
	got := map[string]ACE{}
	for _, a := range after {
		got[a.SID] = a
		if a.Flags&aceFlagInherited != 0 {
			t.Errorf("%s still inherited on the folder (its link to the parent is cut)", a.SID)
		}
	}
	if _, ok := got[sidTrusted]; ok {
		t.Error("an entry was added for the trusted writer")
	}
	if _, ok := got[sidOther]; ok {
		t.Error("an add-files-only entry gained read: it must be dropped")
	}
	if a := got[sidUsers]; a.Mask != MaskReadExecute {
		t.Errorf("Users %#x, want read & execute", a.Mask)
	}
	if a := got[sidEveryone]; a.Type != aceTypeAllowedCB || a.Mask != MaskReadExecute || !bytes.HasSuffix(bytes.TrimRight(a.raw, "\x00"), bytes.TrimRight(cond, "\x00")) {
		t.Errorf("the conditional entry %+v: want callback type, read & execute, its condition kept", a)
	}
	if a := got[sidCO]; a.Mask != MaskReadExecute || a.Flags != 0x3|0x8 {
		t.Errorf("CREATOR OWNER %+v: want an inherit-only read & execute", a)
	}
	if a := got[SIDLocalSystem]; a.Mask != fileAllAccess {
		t.Errorf("SYSTEM %#x changed", a.Mask)
	}
	// A trusted writer's own Full Control is capped at Modify, no more.
	out, _, _ = MaskDACL(testACL(testACE(t, aceTypeAllowed, 0x3, genericAll, sidTrusted, 0, nil)), map[string]bool{sidTrusted: true}, false)
	if after, _ := ParseACL(out); len(after) != 1 || after[0].Mask != MaskModify {
		t.Errorf("trusted Full: %+v, want Modify", after)
	}
}
