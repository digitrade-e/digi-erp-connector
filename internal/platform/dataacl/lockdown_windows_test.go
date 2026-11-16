//go:build windows

package dataacl

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// ownerAndDACL is a path's "O:…D:…" as a restore must leave it.
func ownerAndDACL(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return sd.String()
}

// longTemp is t.TempDir() by its long name (the CI runner's is RUNNER~1).
func longTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func principal(plan LockdownPlan, sid string) (PrincipalRights, bool) {
	for _, p := range plan.Principals {
		if p.SID == sid {
			return p, true
		}
	}
	return PrincipalRights{}, false
}

// Plan 087 review R1c: a lock-down gives the folder a protected DACL —
// SYSTEM and Administrators full, the trusted writer modify, Users (who could
// add files) read & execute — and Administrators as its owner; every entry
// beneath inherits exactly that (a subfolder's own Everyone entry is
// dropped); and the recorded pre-image puts every entry back as it was.
func TestLockdown_LocksDownAndRestores(t *testing.T) {
	herp := filepath.Join(longTemp(t), "herp")
	sub := filepath.Join(herp, "sub")
	mkfile(t, filepath.Join(herp, "b.txt"))
	mkfile(t, filepath.Join(sub, "a.txt"))
	setDACL(t, herp, "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;BU)(A;OICI;0x2;;;BU)", windows.PROTECTED_DACL_SECURITY_INFORMATION)
	setDACL(t, sub, "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;WD)", windows.PROTECTED_DACL_SECURITY_INFORMATION)
	paths := []string{herp, filepath.Join(herp, "b.txt"), sub, filepath.Join(sub, "a.txt")}
	before := map[string]string{}
	for _, p := range paths {
		before[p] = ownerAndDACL(t, p)
	}
	me := currentUserSID(t)
	users := wellKnownName(t, windows.WinBuiltinUsersSid)

	plan, err := PlanLockdown(herp, []string{me})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Entries) != 4 || plan.Entries[0].Rel != "" {
		t.Fatalf("entries %+v", plan.Entries)
	}
	for _, p := range paths { // the plan wrote nothing
		if got := ownerAndDACL(t, p); got != before[p] {
			t.Fatalf("the plan changed %s", p)
		}
	}
	if p, ok := principal(plan, "S-1-5-32-545"); !ok || p.Before != "write" || p.After != "read" {
		t.Errorf("Users %+v (%v): want write → read", p, ok)
	}
	if p, ok := principal(plan, me); !ok || p.After != "modify" {
		t.Errorf("the trusted writer %+v (%v): want modify", p, ok)
	}
	if p, ok := principal(plan, SIDLocalSystem); !ok || p.After != "full" {
		t.Errorf("SYSTEM %+v", p)
	}

	n, err := ApplyLockdown(herp, []string{me}, plan)
	if err != nil || n != 4 {
		t.Fatalf("ApplyLockdown = %d, %v", n, err)
	}
	if _, ctl := daclOf(t, herp); ctl&windows.SE_DACL_PROTECTED == 0 {
		t.Error("the folder's DACL is not protected")
	}
	if o := ownerOf(t, herp); o != SIDAdministrators {
		t.Errorf("the folder's owner is %s", o)
	}
	for _, p := range []string{herp, sub} {
		who, err := FolderWriters(p)
		if err != nil || slices.Contains(names(who), users) || slices.ContainsFunc(who, func(w Writer) bool { return w.SID == "S-1-1-0" }) {
			t.Errorf("%s after the lock-down: writers %+v, %v", p, who, err)
		}
	}
	if d, _ := daclOf(t, sub); strings.Contains(d, ";;;WD)") && !strings.Contains(d, "0x1200a9;;;WD)") {
		t.Errorf("the subfolder kept its own Everyone entry: %s", d)
	}

	restored, skipped, err := RestoreFolder(herp, plan.Entries)
	if err != nil || restored != 4 || skipped != 0 {
		t.Fatalf("RestoreFolder = %d restored, %d skipped, %v", restored, skipped, err)
	}
	for _, p := range paths {
		got := ownerAndDACL(t, p)
		if ownerPart(got) != ownerPart(before[p]) || !sameDACL(got, before[p]) {
			t.Errorf("%s after the restore:\n got  %s\n want %s", p, got, before[p])
		}
	}
}

// ownerPart is the "O:…" of an SDDL.
func ownerPart(sddl string) string {
	if i := strings.Index(sddl, "D:"); i >= 0 {
		return sddl[:i]
	}
	return sddl
}

// A reparse point anywhere beneath refuses the lock-down before anything is
// written — its target is never touched; and a lock-down walks a bounded
// tree.
func TestLockdown_RefusesAJunctionAndTooManyEntries(t *testing.T) {
	herp := filepath.Join(longTemp(t), "herp")
	target := longTemp(t)
	mkfile(t, filepath.Join(target, "outside.txt"))
	mkfile(t, filepath.Join(herp, "a.txt"))
	mklinkJ(t, filepath.Join(herp, "zz-link"), target)
	beforeTarget, beforeHerp := ownerAndDACL(t, target), ownerAndDACL(t, herp)
	if _, err := PlanLockdown(herp, nil); !errors.Is(err, ErrReparsePoint) {
		t.Fatalf("a junction inside: %v, want ErrReparsePoint", err)
	}
	if ownerAndDACL(t, target) != beforeTarget || ownerAndDACL(t, herp) != beforeHerp {
		t.Fatal("a refused plan changed something")
	}
	if _, err := PlanLockdown(filepath.Join(herp, "zz-link"), nil); !errors.Is(err, ErrReparsePoint) {
		t.Fatalf("the folder itself a junction: %v, want ErrReparsePoint", err)
	}
	if err := os.Remove(filepath.Join(herp, "zz-link")); err != nil {
		t.Fatal(err)
	}

	old := lockdownLimit
	t.Cleanup(func() { lockdownLimit = old })
	lockdownLimit = 3
	for _, n := range []string{"b.txt", "c.txt", "d.txt"} {
		mkfile(t, filepath.Join(herp, n))
	}
	if _, err := PlanLockdown(herp, nil); !errors.Is(err, ErrTooManyEntries) {
		t.Fatalf("five entries with a bound of three: %v, want ErrTooManyEntries", err)
	}
}

// Folders above are never changed; a lock-down reports who can still rename
// the folder away through one (delete-child on its parent).
func TestLockdown_ReportsParentWriters(t *testing.T) {
	top := filepath.Join(longTemp(t), "xampp")
	herp := filepath.Join(top, "herp")
	if err := os.MkdirAll(herp, 0o700); err != nil {
		t.Fatal(err)
	}
	setDACL(t, top, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;OW)(A;;0x1200a9;;;BU)(A;;0x40;;;BU)", windows.PROTECTED_DACL_SECURITY_INFORMATION)
	plan, err := PlanLockdown(herp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(plan.ParentWriters, func(w Writer) bool { return w.SID == "S-1-5-32-545" }) {
		t.Errorf("parent writers %+v: want Users (delete-child on the parent)", plan.ParentWriters)
	}
	topBefore := ownerAndDACL(t, top)
	if _, err := ApplyLockdown(herp, nil, plan); err != nil {
		t.Fatal(err)
	}
	if ownerAndDACL(t, top) != topBefore {
		t.Error("the lock-down changed the parent")
	}
}
