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

// Plan 087 review T2: a lock-down masks the DACLs already there and grants
// nothing — Users (could add files) end with read & execute; a subfolder
// keeps its own protected DACL, its Everyone Full capped at read & execute;
// the trusted account, which had nothing, gets nothing; the check output
// counts what changes beneath the folder too; and the recorded pre-image
// puts every entry back as it was.
func TestLockdown_MasksAndRestores(t *testing.T) {
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

	plan, err := PlanLockdown(herp, []string{me}, map[string]bool{".dll": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Entries) != 4 || plan.Entries[0].Rel != "" || len(plan.Executables) != 0 || !strings.HasSuffix(strings.ToLower(plan.FinalPath), `\herp`) {
		t.Fatalf("plan %+v", plan)
	}
	for _, p := range paths { // the plan wrote nothing
		if got := ownerAndDACL(t, p); got != before[p] {
			t.Fatalf("the plan changed %s", p)
		}
	}
	if p, ok := principal(plan, "S-1-5-32-545"); !ok || p.Before != "write" || p.After != "read" || p.Entries < 1 {
		t.Errorf("Users %+v (%v): want write → read", p, ok)
	}
	if p, ok := principal(plan, "S-1-1-0"); !ok || p.Before != "full" || p.After != "read" || p.Entries != 2 {
		t.Errorf("Everyone %+v (%v): want full → read on the subfolder and its file (2 entries)", p, ok)
	}
	if _, ok := principal(plan, me); ok {
		t.Errorf("the trusted account, which had no entry, shows: %+v", plan.Principals)
	}
	if plan.EntriesChanged != 4 {
		t.Errorf("entriesChanged %d, want 4", plan.EntriesChanged)
	}

	n, err := ApplyLockdown(herp, []string{me}, plan)
	if err != nil || n != 4 {
		t.Fatalf("ApplyLockdown = %d, %v", n, err)
	}
	for _, p := range []string{herp, sub} {
		d, ctl := daclOf(t, p)
		if ctl&windows.SE_DACL_PROTECTED == 0 {
			t.Errorf("%s: its DACL is no longer protected", p)
		}
		if strings.Contains(d, me) {
			t.Errorf("%s: an entry was added for the trusted account: %s", p, d)
		}
		who, err := FolderWriters(p)
		if err != nil || slices.Contains(names(who), users) || slices.ContainsFunc(who, func(w Writer) bool { return w.SID == "S-1-1-0" }) {
			t.Errorf("%s after the lock-down: writers %+v, %v", p, who, err)
		}
	}
	if d, _ := daclOf(t, sub); !strings.Contains(d, "0x1200a9;;;WD)") {
		t.Errorf("the subfolder's Everyone entry is not read & execute: %s", d)
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

// Review T2 on a real folder: a principal that could only add files gains no
// read, a conditional entry stays conditional, and the folder's inherited
// entries become explicit (its link to the parent cut).
func TestLockdown_GrantsNothing(t *testing.T) {
	top := filepath.Join(longTemp(t), "xampp")
	herp := filepath.Join(top, "herp")
	mkfile(t, filepath.Join(herp, "kupain.doc"))
	setDACL(t, top, "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x2;;;BU)", windows.PROTECTED_DACL_SECURITY_INFORMATION)
	setDACL(t, herp, "D:(XA;OICI;0x1301bf;;;WD;(Member_of {SID(BA)}))", 0) // inherits top's entries
	plan, err := PlanLockdown(herp, []string{"S-1-5-21-1-2-3-1105"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyLockdown(herp, []string{"S-1-5-21-1-2-3-1105"}, plan); err != nil {
		t.Fatal(err)
	}
	d, ctl := daclOf(t, herp)
	if ctl&windows.SE_DACL_PROTECTED == 0 || strings.Contains(d, "ID;") {
		t.Errorf("the folder is still linked to its parent: %s", d)
	}
	if strings.Contains(d, ";;;BU)") {
		t.Errorf("Users, who could only add files, kept an entry (they would gain read): %s", d)
	}
	if !strings.Contains(d, "XA;") || !strings.Contains(d, "0x1200a9;;;WD;(") {
		t.Errorf("the conditional entry is not a conditional read & execute: %s", d)
	}
	if strings.Contains(d, "S-1-5-21-1-2-3-1105") {
		t.Errorf("an entry was added for the trusted writer: %s", d)
	}
}

// Review T1: the plan names the files a lock-down must not take (executables).
func TestLockdown_NamesExecutables(t *testing.T) {
	herp := filepath.Join(longTemp(t), "herp")
	mkfile(t, filepath.Join(herp, "IMOVEIN_1.doc"))
	mkfile(t, filepath.Join(herp, "site.php"))
	mkfile(t, filepath.Join(herp, "lib", "wtsapi32.DLL"))
	plan, err := PlanLockdown(herp, nil, map[string]bool{".dll": true, ".exe": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Executables) != 1 || !strings.EqualFold(plan.Executables[0], filepath.Join("lib", "wtsapi32.DLL")) {
		t.Errorf("executables %v", plan.Executables)
	}
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
	if _, err := PlanLockdown(herp, nil, nil); !errors.Is(err, ErrReparsePoint) {
		t.Fatalf("a junction inside: %v, want ErrReparsePoint", err)
	}
	if ownerAndDACL(t, target) != beforeTarget || ownerAndDACL(t, herp) != beforeHerp {
		t.Fatal("a refused plan changed something")
	}
	if _, err := PlanLockdown(filepath.Join(herp, "zz-link"), nil, nil); !errors.Is(err, ErrReparsePoint) {
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
	if _, err := PlanLockdown(herp, nil, nil); !errors.Is(err, ErrTooManyEntries) {
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
	plan, err := PlanLockdown(herp, nil, nil)
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

// Review T3: the lock-down writes only the folder whose path the check
// judged. Moved after the check behind a junction of the old name, the same
// folder (same file ID) now resolves elsewhere: refused, nothing written.
func TestLockdown_RefusesAFolderThatResolvesElsewhere(t *testing.T) {
	root := longTemp(t)
	a := filepath.Join(root, "a")
	herp := filepath.Join(a, "herp")
	mkfile(t, filepath.Join(herp, "x.doc"))
	plan, err := PlanLockdown(herp, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := filepath.Join(root, "b")
	if err := os.Rename(a, b); err != nil {
		t.Fatal(err)
	}
	mklinkJ(t, a, b)
	before := ownerAndDACL(t, filepath.Join(b, "herp"))
	if _, err := ApplyLockdown(herp, nil, plan); !errors.Is(err, ErrOutsideTree) {
		t.Fatalf("ApplyLockdown through a junction: %v, want ErrOutsideTree", err)
	}
	if ownerAndDACL(t, filepath.Join(b, "herp")) != before {
		t.Error("the refused lock-down wrote")
	}
}

// Review T2 on long paths: the lock-down and its restore reach an entry
// deeper than MAX_PATH (every open is by its \\?\ form, the walk by handle):
// the deep file loses Users' write, and the restore puts it back as it was.
func TestLockdown_LongPaths(t *testing.T) {
	herp := filepath.Join(longTemp(t), "herp")
	deep := herp
	for len(deep) < 300 {
		deep = filepath.Join(deep, strings.Repeat("d", 40))
	}
	file := filepath.Join(deep, "KUPAIN_500001.doc")
	mkfile(t, file)
	setDACL(t, longPath(deep), "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1301bf;;;BU)", windows.PROTECTED_DACL_SECURITY_INFORMATION)
	setDACL(t, longPath(file), "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1301bf;;;BU)", windows.PROTECTED_DACL_SECURITY_INFORMATION)
	before := ownerAndDACL(t, longPath(file))
	plan, err := PlanLockdown(herp, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	last := plan.Entries[len(plan.Entries)-1]
	if !strings.HasSuffix(last.Rel, "KUPAIN_500001.doc") || len(herp)+1+len(last.Rel) <= 260 {
		t.Fatalf("the deepest entry %q (%d chars)", last.Rel, len(herp)+1+len(last.Rel))
	}
	if !strings.Contains(last.SDDL, "0x1301bf;;;BU)") {
		t.Fatalf("the plan read the deep file as %s (before: %s)", last.SDDL, before)
	}
	if _, err := ApplyLockdown(herp, nil, plan); err != nil {
		t.Fatal(err)
	}
	after := ownerAndDACL(t, longPath(file))
	if !strings.Contains(after, "0x1200a9;;;BU)") || strings.Contains(after, "0x1301bf;;;BU") {
		t.Errorf("the deep file after the lock-down: %s (before: %s)", after, before)
	}
	if _, skipped, err := RestoreFolder(herp, plan.Entries); err != nil || skipped != 0 {
		t.Fatalf("RestoreFolder: %d skipped, %v", skipped, err)
	}
	if got := ownerAndDACL(t, longPath(file)); !sameDACL(got, before) {
		t.Errorf("the deep file after the restore:\n got  %s\n want %s", got, before)
	}
}
