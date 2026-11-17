package dataacl

import (
	"bytes"
	"errors"
	"math/rand"
	"testing"
)

// carryAsPlanned is planEntry's carry-over with the token simulator standing
// in for AuthZ (on the object, as AuthZ judges it): the masked DACL, plus one
// carried entry for sidTrusted when the masking took write it had.
func carryAsPlanned(t *testing.T, acl []byte, tok map[string]bool, root, folder bool) (before, after []ACE, carried bool) {
	t.Helper()
	trusted := map[string]bool{sidTrusted: true}
	masked, _, err := MaskDACL(acl, trusted, root)
	if err != nil {
		t.Fatal(err)
	}
	before, _ = ParseACL(acl)
	out := masked
	notUser := func(string) (bool, error) { return false, nil } // every other SID a group it may belong to
	if CarryCapable(before, tok, tok) && !DeniesUnaccounted(before, tok, notUser) {
		maskedACEs, _ := ParseACL(masked)
		want := accessFor(before, tok, true, "") & MaskModify
		if (want&^accessFor(maskedACEs, tok, true, ""))&^maskSynchronize != 0 {
			ace, err := CarryACE(sidTrusted, want, CarryFlags(before, tok, tok, want, folder))
			if err != nil {
				t.Fatal(err)
			}
			if out, err = InsertCarried(masked, [][]byte{ace}); err != nil {
				t.Fatal(err)
			}
			carried = true
		}
	}
	after, err = ParseACL(out)
	if err != nil {
		t.Fatalf("the carried DACL does not parse: %v", err)
	}
	return before, after, carried
}

// v1.0.32's property: with carry-over, a trusted writer's effective rights
// never grow — on the entry, and (a folder) on any file or folder it hands
// its entries down to — whatever the conditions; every other token as with
// the plain masking. Generated DACLs: generic, conditional, object, creator
// and owner SIDs, inherit-only, no-propagate, inherited and deny entries.
func TestCarry_NeverGrows(t *testing.T) {
	rng := rand.New(rand.NewSource(1032))
	types := []byte{aceTypeAllowed, aceTypeAllowed, aceTypeAllowed, aceTypeDenied, aceTypeAllowedObj, aceTypeAllowedCB, aceTypeAllowedCBObj, 0x6, 0xA}
	masks := []uint32{0x1, 0x2, 0x4, 0x6, 0x40, 0x10000, 0x40000, 0x100000, 0x120089, 0x1200a9, 0x1301bf, 0x1f01ff, genericRead, genericWrite, genericAll}
	flagsPool := []byte{0, 0x1, 0x2, 0x3, 0x3 | 0x8, 0x1 | 0x8, 0x3 | 0x4, 0x1 | 0x4, 0x10, 0x3 | 0x10, 0x3 | 0x4 | 0x10}
	groups := []string{sidEveryone, sidUsers, sidOther}
	carriedRounds := 0
	for round := 0; round < 4000; round++ {
		var raws [][]byte
		for n := rng.Intn(8); n >= 0; n-- {
			typ := types[rng.Intn(len(types))]
			var app []byte
			if isCallback(typ) {
				app = []byte("artx\x01\x02\x03\x04")
			}
			raws = append(raws, testACE(t, typ, flagsPool[rng.Intn(len(flagsPool))], masks[rng.Intn(len(masks))], poolSIDs[rng.Intn(len(poolSIDs))], rng.Intn(3), app))
		}
		acl := testACL(raws...)
		tok := map[string]bool{sidTrusted: true}
		for _, g := range groups {
			if rng.Intn(2) == 0 {
				tok[g] = true
			}
		}
		root, folder := rng.Intn(2) == 0, rng.Intn(3) != 0
		before, after, carried := carryAsPlanned(t, acl, tok, root, folder)
		if carried {
			carriedRounds++
		}
		views := []string{""}
		if folder {
			views = append(views, "file", "dir")
		}
		for _, c := range []bool{true, false} {
			for _, v := range views {
				if gained := accessFor(after, tok, c, v) &^ accessFor(before, tok, c, v); gained != 0 {
					t.Fatalf("round %d: the trusted writer (condition %v, view %q) gained %#x\n before %+v\n after  %+v", round, c, v, gained, before, after)
				}
			}
			for k := 0; k < 6; k++ { // anyone else: never more than before either
				other := map[string]bool{}
				for _, s := range poolSIDs {
					if s != sidTrusted && rng.Intn(3) == 0 {
						other[s] = true
					}
				}
				for _, v := range views {
					if gained := accessFor(after, other, c, v) &^ accessFor(before, other, c, v); gained != 0 {
						t.Fatalf("round %d: token %v (condition %v, view %q) gained %#x", round, other, c, v, gained)
					}
				}
			}
		}
	}
	if carriedRounds < 100 {
		t.Fatalf("only %d rounds carried anything: the generator misses the case", carriedRounds)
	}
}

// The WizSched case: the scheduler wrote only through Users (Modify, handed
// down to files and folders); the lock-down masks Users to read & execute and
// gives the scheduler back exactly Modify, inheritable as before — placed
// after the explicit denies.
func TestCarry_TheSchedulerCase(t *testing.T) {
	acl := testACL(
		testACE(t, aceTypeDenied, 0x0, 0x40000, "S-1-5-32-546", 0, nil), // Guests: a builtin alias AuthZ accounts for
		testACE(t, aceTypeAllowed, 0x3, fileAllAccess, SIDLocalSystem, 0, nil),
		testACE(t, aceTypeAllowed, 0x3, MaskModify, sidUsers, 0, nil),
	)
	tok := map[string]bool{sidTrusted: true, sidUsers: true, sidEveryone: true}
	_, after, carried := carryAsPlanned(t, acl, tok, true, true)
	if !carried || len(after) != 4 || after[0].Type != aceTypeDenied || after[1].SID != sidTrusted || after[1].Mask != MaskModify || after[1].Flags != 0x3 {
		t.Fatalf("after %+v", after)
	}
	if a := after[3]; a.SID != sidUsers || a.Mask != MaskReadExecute {
		t.Errorf("Users %+v", a)
	}
	// Handed down only to files before: the carried entry is OI only.
	acl = testACL(testACE(t, aceTypeAllowed, 0x1, MaskModify, sidUsers, 0, nil), testACE(t, aceTypeAllowed, 0x0, MaskModify, sidUsers, 0, nil))
	if _, after, _ = carryAsPlanned(t, acl, tok, false, true); after[0].SID != sidTrusted || after[0].Flags != 0x1 {
		t.Errorf("files only: %+v", after)
	}
	// A conditional entry for the writer: nothing carried.
	acl = testACL(testACE(t, aceTypeAllowedCB, 0x3, MaskModify, sidUsers, 0, []byte("artx\x01\x02\x03\x04")))
	if _, _, carried := carryAsPlanned(t, acl, tok, false, true); carried {
		t.Error("carried over a conditional entry")
	}
	// Generic rights are mapped before AuthZ sees them.
	m, err := MapGenericACL(testACL(testACE(t, aceTypeAllowed, 0x3, genericAll, sidUsers, 0, nil)))
	if a, _ := ParseACL(m); err != nil || a[0].Mask != fileAllAccess {
		t.Errorf("mapped %+v %v", a, err)
	}
	if b, _ := CarryACE("S-1-5-21-1-2-3-1105", MaskModify, 0x3); !bytes.Equal(b[:8], []byte{0, 3, 36, 0, 0xbf, 0x01, 0x13, 0}) {
		t.Errorf("ACE %x", b)
	}
}

// Review C-1: a deny AuthZ's view of the account cannot account for —
// logon-type or session SIDs, groups it may have missed — stops the carry on
// that entry; builtin aliases, the machine's own accounts, creator SIDs,
// another user, and the context's own SIDs do not; a failed lookup does.
func TestCarry_DeniesUnaccounted(t *testing.T) {
	deny := map[string]bool{sidTrusted: true, sidUsers: true}
	users := map[string]bool{"S-1-5-21-1-2-3-2001": true}
	isUser := func(s string) (bool, error) {
		if s == "S-1-5-21-1-2-3-9999" {
			return false, errors.New("the domain controller does not answer")
		}
		return users[s], nil
	}
	for sid, want := range map[string]bool{
		"S-1-5-4":             true,  // INTERACTIVE: not in an AuthZ context
		"S-1-5-3":             true,  // BATCH: the scheduler's real logon
		"S-1-5-5-0-123456":    true,  // a logon session
		"S-1-5-21-1-2-3-3001": true,  // a domain group it may have missed
		"S-1-5-21-1-2-3-9999": true,  // a lookup that fails
		"S-1-5-80-1-2-3-4-5":  true,  // a service SID
		"S-1-5-32-546":        false, // Guests: a builtin alias, resolved locally
		"S-1-5-18":            false,
		"S-1-3-0":             false,
		"S-1-5-21-1-2-3-2001": false, // another user
		sidUsers:              false, // in the context
	} {
		aces, _ := ParseACL(testACL(testACE(t, aceTypeDenied, 0x3, 0x2, sid, 0, nil), testACE(t, aceTypeAllowed, 0x3, MaskModify, sidUsers, 0, nil)))
		if got := DeniesUnaccounted(aces, deny, isUser); got != want {
			t.Errorf("a deny for %s: unaccounted %v, want %v", sid, got, want)
		}
	}
	aces, _ := ParseACL(testACL(testACE(t, aceTypeAllowed, 0x3, MaskModify, sidUsers, 0, nil)))
	if DeniesUnaccounted(aces, deny, isUser) {
		t.Error("no deny at all: unaccounted")
	}
}
