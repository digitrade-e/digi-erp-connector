//go:build windows

package dataacl

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func daclOf(t *testing.T, path string) (string, windows.SECURITY_DESCRIPTOR_CONTROL) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo(%s): %v", path, err)
	}
	ctl, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	return sd.String(), ctl
}

// fullSD is owner, group and DACL — to show an object outside the data
// directory was not touched at all.
func fullSD(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo(%s): %v", path, err)
	}
	return sd.String()
}

func currentUserSID(t *testing.T) string {
	t.Helper()
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return tu.User.Sid.String()
}

func mkfile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// assertOnlyOurs checks every ACE in sddl names SYSTEM, Administrators, the
// current user or OWNER RIGHTS (READ_CONTROL only), and that the last is there.
func assertOnlyOurs(t *testing.T, path, sddl, user string) {
	t.Helper()
	i := strings.Index(sddl, "D:")
	if i < 0 {
		t.Fatalf("%s: no DACL in %s", path, sddl)
	}
	dacl := sddl[i+2:]
	if j := strings.Index(dacl, "S:"); j >= 0 {
		dacl = dacl[:j]
	}
	aces := strings.Split(strings.TrimLeft(dacl, "PAIRCU"), "(")
	n, sawOW := 0, false
	for _, ace := range aces {
		ace = strings.TrimSuffix(ace, ")")
		if ace == "" {
			continue
		}
		n++
		parts := strings.Split(ace, ";")
		if len(parts) < 6 || parts[0] != "A" {
			t.Errorf("%s: unexpected ACE %q in %s", path, ace, sddl)
			continue
		}
		// The SID may come back as an alias (SY, BA, OW, …): resolve it.
		sid := parts[5]
		if s, err := windows.StringToSid(sid); err == nil {
			sid = s.String()
		}
		switch sid {
		case SIDLocalSystem, SIDAdministrators, user:
		case SIDOwnerRights:
			sawOW = true
			if r := parts[2]; r != "RC" && !strings.EqualFold(r, "0x20000") {
				t.Errorf("%s: OWNER RIGHTS granted %q in %s; want READ_CONTROL only", path, r, sddl)
			}
		default:
			t.Errorf("%s: ACE for %s in %s; want only SYSTEM, Administrators, %s, OWNER RIGHTS", path, parts[5], sddl, user)
		}
	}
	if n == 0 {
		t.Errorf("%s: empty DACL %s", path, sddl)
	}
	if !sawOW {
		t.Errorf("%s: no OWNER RIGHTS ACE in %s", path, sddl)
	}
}

func setDACL(t *testing.T, path, sddl string, info windows.SECURITY_INFORMATION) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatalf("parse %s: %v", sddl, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|info, nil, nil, dacl, nil); err != nil {
		t.Fatalf("set %s on %s: %v", sddl, path, err)
	}
}

// onlyInherited fails unless path's DACL is unprotected, every ACE is
// inherited, and only ours.
func onlyInherited(t *testing.T, path string) {
	t.Helper()
	sddl, ctl := daclOf(t, path)
	if ctl&windows.SE_DACL_PROTECTED != 0 {
		t.Errorf("%s: still protected: %s", path, sddl)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount == 0 {
		t.Fatalf("%s: DACL %v (%v); want inherited ACEs", path, dacl, err)
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceFlags&windows.INHERITED_ACE == 0 {
			t.Errorf("%s: an explicit ACE survived: %s", path, sddl)
		}
	}
	assertOnlyOurs(t, path, sddl, currentUserSID(t))
}

func mklinkJ(t *testing.T, link, target string) {
	t.Helper()
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		// Fatal, not Skip: on the Windows CI a junction needs no privilege, and a
		// skipped test here would prove nothing.
		t.Fatalf("mklink /J: %v %s", err, out)
	}
}

func protectTree(t *testing.T, dir string) Report {
	t.Helper()
	rep, err := ProtectTree(dir, TreeOptions{})
	if err != nil {
		t.Fatalf("ProtectTree: %v", err)
	}
	return rep
}

// The directory: protected and only ours; an entry that was there and one
// created afterwards both carry just the inherited ACEs; a second run writes
// nothing at all.
func TestProtectTree_AppliesAndInherits(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	before := filepath.Join(dir, "receipts", "index.json")
	mkfile(t, before)

	rep := protectTree(t, dir)
	if !rep.RootSet || rep.Entries != 2 || rep.Reset != 2 {
		t.Errorf("first run %+v; want the root and both entries written", rep)
	}
	after := filepath.Join(dir, "config.yaml")
	mkfile(t, after)

	sddl, ctl := daclOf(t, dir)
	if ctl&windows.SE_DACL_PROTECTED == 0 {
		t.Errorf("%s: the data directory's DACL is not protected", sddl)
	}
	assertOnlyOurs(t, dir, sddl, currentUserSID(t))
	for _, p := range []string{filepath.Dir(before), before, after} {
		onlyInherited(t, p)
	}

	rep = protectTree(t, dir)
	if rep.RootSet || rep.Reset != 0 || rep.Entries != 3 {
		t.Errorf("steady state %+v; want 3 entries read and nothing written", rep)
	}
}

func TestProtectTree_MissingIsAnError(t *testing.T) {
	if _, err := ProtectTree(filepath.Join(t.TempDir(), "nope"), TreeOptions{}); err == nil {
		t.Error("ProtectTree on a missing directory = nil; want an error")
	}
}

// Planted before the first protected start: a folder with a protected DACL
// granting Everyone, its child, a file with an explicit Everyone ACE and a
// file with a NULL DACL (everyone, everything). All end inherited-only.
func TestProtectTree_ResetsPlantedACLs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	receipts := filepath.Join(dir, "receipts")
	child := filepath.Join(receipts, "index")
	explicit := filepath.Join(dir, "planted.json")
	nullDACL := filepath.Join(dir, "null.json")
	if err := os.MkdirAll(receipts, 0o700); err != nil {
		t.Fatal(err)
	}
	mkfile(t, explicit)
	mkfile(t, nullDACL)
	user := currentUserSID(t)
	setDACL(t, receipts, "D:P(A;OICI;FA;;;WD)(A;OICI;FA;;;"+user+")", windows.PROTECTED_DACL_SECURITY_INFORMATION)
	if err := os.MkdirAll(child, 0o700); err != nil { // inherits Everyone from the planted folder
		t.Fatal(err)
	}
	setDACL(t, explicit, "D:(A;;FA;;;WD)", windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	if err := windows.SetNamedSecurityInfo(nullDACL, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, nil, nil); err != nil {
		t.Fatalf("plant a NULL DACL: %v", err)
	}
	t.Logf("NULL-DACL plant reads back as %s", func() string { s, _ := daclOf(t, nullDACL); return s }())

	rep := protectTree(t, dir)
	if rep.Entries != 4 || rep.Reset != 4 {
		t.Errorf("report %+v; want all 4 planted entries rewritten", rep)
	}
	for _, p := range []string{receipts, child, explicit, nullDACL} {
		onlyInherited(t, p)
	}
	if rep = protectTree(t, dir); rep.Reset != 0 {
		t.Errorf("second run reset %d; want 0", rep.Reset)
	}
}

// A junction inside the data directory, or the data directory being one,
// stops ProtectTree; the target is never touched.
func TestProtectTree_RefusesAJunction(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	mkfile(t, filepath.Join(target, "outside.txt"))
	before := fullSD(t, target)
	beforeFile := fullSD(t, filepath.Join(target, "outside.txt"))
	mklinkJ(t, filepath.Join(dir, "history"), target)

	if _, err := ProtectTree(dir, TreeOptions{}); !errors.Is(err, ErrReparsePoint) {
		t.Fatalf("ProtectTree with a junction inside = %v; want ErrReparsePoint", err)
	}
	if after := fullSD(t, target); after != before {
		t.Errorf("the junction's target changed:\n before %s\n after  %s", before, after)
	}
	if after := fullSD(t, filepath.Join(target, "outside.txt")); after != beforeFile {
		t.Errorf("a file behind the junction changed")
	}

	root := filepath.Join(t.TempDir(), "data-link")
	mklinkJ(t, root, target)
	if _, err := ProtectTree(root, TreeOptions{}); !errors.Is(err, ErrReparsePoint) {
		t.Fatalf("ProtectTree on a junction = %v; want ErrReparsePoint", err)
	}
	if after := fullSD(t, target); after != before {
		t.Errorf("a junctioned data directory's target changed")
	}
}

// R2-1/R2-4: an entry scanned as a folder and swapped for a junction while
// the walk runs is refused when it is opened — the open never follows it.
func TestProtectTree_RefusesAJunctionSwappedInMidRun(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	receipts := filepath.Join(dir, "receipts")
	mkfile(t, filepath.Join(receipts, "a.json"))
	target := t.TempDir()
	mkfile(t, filepath.Join(target, "outside.txt"))
	before := fullSD(t, target)
	beforeFile := fullSD(t, filepath.Join(target, "outside.txt"))

	swapped := false
	testHookBeforeEntry = func(p string) {
		if p != receipts || swapped {
			return
		}
		swapped = true
		if err := os.Rename(receipts, receipts+".moved"); err != nil {
			t.Errorf("swap: %v", err)
			return
		}
		mklinkJ(t, receipts, target)
	}
	t.Cleanup(func() { testHookBeforeEntry = nil })

	_, err := ProtectTree(dir, TreeOptions{})
	if !swapped {
		t.Fatal("the hook never ran")
	}
	if !errors.Is(err, ErrReparsePoint) {
		t.Fatalf("ProtectTree = %v; want ErrReparsePoint", err)
	}
	if after := fullSD(t, target); after != before {
		t.Errorf("the swapped-in junction's target changed:\n before %s\n after  %s", before, after)
	}
	if after := fullSD(t, filepath.Join(target, "outside.txt")); after != beforeFile {
		t.Errorf("a file behind the swapped-in junction changed")
	}
}

// A hard link shares its security descriptor with the other name: ProtectTree
// refuses it and the file outside keeps its security byte for byte.
func TestProtectTree_RefusesAHardLink(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	mkfile(t, outside)
	before := fullSD(t, outside)
	if err := os.Link(outside, filepath.Join(dir, "linked.json")); err != nil {
		t.Fatalf("hard link: %v", err)
	}
	if _, err := ProtectTree(dir, TreeOptions{}); !errors.Is(err, ErrHardLink) {
		t.Fatalf("ProtectTree with a hard link inside = %v; want ErrHardLink", err)
	}
	if after := fullSD(t, outside); after != before {
		t.Errorf("the hard link's other name changed:\n before %s\n after  %s", before, after)
	}
	if err := ProtectFile(filepath.Join(dir, "linked.json")); !errors.Is(err, ErrHardLink) {
		t.Errorf("ProtectFile on a hard link = %v; want ErrHardLink", err)
	}
	if ok, err := FileTrusted(filepath.Join(dir, "linked.json")); ok || err != nil {
		t.Errorf("FileTrusted on a hard link = %v, %v; want false", ok, err)
	}
}

// enablePrivilege turns a privilege of this (admin) process on until the test
// ends; false if it is not held.
func enablePrivilege(t *testing.T, name string) bool {
	t.Helper()
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &tok); err != nil {
		return false
	}
	var luid windows.LUID
	n, _ := windows.UTF16PtrFromString(name)
	if err := windows.LookupPrivilegeValue(nil, n, &luid); err != nil {
		tok.Close()
		return false
	}
	set := func(attr uint32) error {
		tp := windows.Tokenprivileges{PrivilegeCount: 1, Privileges: [1]windows.LUIDAndAttributes{{Luid: luid, Attributes: attr}}}
		return windows.AdjustTokenPrivileges(tok, false, &tp, 0, nil, nil)
	}
	if err := set(windows.SE_PRIVILEGE_ENABLED); err != nil {
		tok.Close()
		return false
	}
	t.Cleanup(func() { _ = set(0); tok.Close() })
	return true
}

// A planted entry is owned by its planter. Here: a file owned by
// BUILTIN\Users (S-1-5-32-545), not an administrator — set with
// SeRestorePrivilege, which the CI runner (an administrator) holds.
func TestProtectTree_RefusesAnUntrustedOwner(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	planted := filepath.Join(dir, "receipts", "receiptNumber.json")
	mkfile(t, planted)
	if !enablePrivilege(t, "SeRestorePrivilege") {
		t.Skip("SeRestorePrivilege not held: an arbitrary owner cannot be set here")
	}
	users, err := windows.StringToSid("S-1-5-32-545")
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(planted, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, users, nil, nil, nil); err != nil {
		t.Skipf("cannot make Users the owner here: %v", err)
	}
	if _, err := ProtectTree(dir, TreeOptions{}); !errors.Is(err, ErrUntrustedOwner) {
		t.Fatalf("ProtectTree over a file owned by Users = %v; want ErrUntrustedOwner", err)
	}
	if ok, err := FileTrusted(planted); ok || err != nil {
		t.Errorf("FileTrusted on a Users-owned file = %v, %v; want false", ok, err)
	}
	if err := ProtectFile(planted); !errors.Is(err, ErrUntrustedOwner) {
		t.Errorf("ProtectFile on a Users-owned file = %v; want ErrUntrustedOwner", err)
	}
}

// Not SYSTEM (the CI runner): the owner is left as it is.
func TestProtectTree_LeavesTheOwnerWhenNotSystem(t *testing.T) {
	if currentUserSID(t) == SIDLocalSystem {
		t.Skip("running as SYSTEM")
	}
	dir := t.TempDir()
	owner := func() string {
		sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		o, _, err := sd.Owner()
		if err != nil {
			t.Fatal(err)
		}
		return o.String()
	}
	was := owner()
	rep := protectTree(t, dir)
	if rep.OwnerSet || rep.OwnerErr != nil {
		t.Fatalf("report %+v", rep)
	}
	if now := owner(); now != was {
		t.Errorf("owner %s -> %s; want unchanged", was, now)
	}
}

func TestProtectFile_Applies(t *testing.T) {
	f := filepath.Join(t.TempDir(), "config.yaml")
	mkfile(t, f)
	setDACL(t, f, "D:(A;;FA;;;WD)", windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	if err := ProtectFile(f); err != nil {
		t.Fatalf("ProtectFile: %v", err)
	}
	onlyInherited(t, f)
	if ok, err := FileTrusted(f); !ok || err != nil {
		t.Errorf("FileTrusted after ProtectFile = %v, %v; want true", ok, err)
	}
}

func TestFileTrusted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	log := filepath.Join(dir, "ui.log")
	mkfile(t, log)
	if ok, err := FileTrusted(filepath.Join(dir, "absent.log")); !ok || err != nil {
		t.Errorf("FileTrusted(absent) = %v, %v; want true", ok, err)
	}
	protectTree(t, dir)
	if ok, err := FileTrusted(log); !ok || err != nil {
		t.Errorf("FileTrusted after ProtectTree = %v, %v; want true", ok, err)
	}
	setDACL(t, log, "D:(A;;FR;;;WD)", windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	if ok, err := FileTrusted(log); ok || err != nil {
		t.Errorf("FileTrusted with an Everyone ACE = %v, %v; want false", ok, err)
	}
}

// OpenLog appends, but never through a symlink and never into a hard link.
func TestOpenLog(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "server.log")
	f, err := OpenLog(log)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	_, _ = f.WriteString("one\n")
	f.Close()
	if f, err = OpenLog(log); err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("two\n")
	f.Close()
	if got, _ := os.ReadFile(log); string(got) != "one\ntwo\n" {
		t.Errorf("log holds %q; want both lines appended", got)
	}

	outside := filepath.Join(t.TempDir(), "outside.txt")
	mkfile(t, outside)
	if err := os.Link(outside, filepath.Join(dir, "linked.log")); err != nil {
		t.Fatal(err)
	}
	if f, err := OpenLog(filepath.Join(dir, "linked.log")); !errors.Is(err, ErrHardLink) {
		if f != nil {
			f.Close()
		}
		t.Errorf("OpenLog on a hard link = %v; want ErrHardLink", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "sym.log")); err == nil {
		if f, err := OpenLog(filepath.Join(dir, "sym.log")); !errors.Is(err, ErrReparsePoint) {
			if f != nil {
				f.Close()
			}
			t.Errorf("OpenLog on a symlink = %v; want ErrReparsePoint", err)
		}
	} else {
		t.Logf("symlink case not run: %v", err)
	}
	if got, _ := os.ReadFile(outside); string(got) != "x" {
		t.Errorf("the outside file was written: %q", got)
	}
}

// What the OWNER RIGHTS ACE buys, pinned on the OS: the owner of a file — this
// test, which created it — keeps an implicit WRITE_DAC as long as no ACE names
// OWNER RIGHTS (it can rewrite a DACL that grants it nothing), and loses it
// once an OWNER RIGHTS ACE grants READ_CONTROL alone. The production DACL also
// grants SYSTEM, Administrators and the service account (this test process is
// one of them), so the owner effect is shown here on a DACL without those.
func TestOwnerRights_RemovesTheOwnersImplicitWriteDAC(t *testing.T) {
	const stranger = "S-1-5-21-1-2-3-4242" // nobody this process is
	set := func(path, sddl string) error {
		sd, err := windows.SecurityDescriptorFromString(sddl)
		if err != nil {
			t.Fatalf("parse %s: %v", sddl, err)
		}
		dacl, _, err := sd.DACL()
		if err != nil {
			t.Fatal(err)
		}
		return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
			windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
	}
	newFile := func(name string) string {
		p := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	without := newFile("without-ow")
	if err := set(without, "D:P(A;;FA;;;"+stranger+")"); err != nil {
		t.Fatalf("first set: %v", err)
	}
	if err := set(without, "D:P(A;;FA;;;"+stranger+")"); err != nil {
		t.Fatalf("without OWNER RIGHTS the owner could not rewrite the DACL (%v); the premise of this test does not hold", err)
	}

	with := newFile("with-ow")
	if err := set(with, "D:P(A;;FA;;;"+stranger+")(A;;RC;;;"+SIDOwnerRights+")"); err != nil {
		t.Fatalf("first set: %v", err)
	}
	// The decisive step, without MARTA in between: opening the file for
	// WRITE_DAC (no backup semantics, so no privilege can grant it) is refused,
	// opening it for READ_CONTROL is not.
	openFor := func(access uint32) error {
		p, _ := windows.UTF16PtrFromString(with)
		h, err := windows.CreateFile(p, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
		if err == nil {
			windows.CloseHandle(h)
		}
		return err
	}
	if err := openFor(windows.WRITE_DAC); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Errorf("with OWNER RIGHTS = READ_CONTROL the owner opened it for WRITE_DAC (err %v); want ERROR_ACCESS_DENIED — %s", err, fullSD(t, with))
	}
	// READ_CONTROL is still there. (Not checked with CreateFile: it always adds
	// SYNCHRONIZE and FILE_READ_ATTRIBUTES to what is asked, which this DACL
	// grants the owner no more than WRITE_DAC.)
	if _, err := windows.GetNamedSecurityInfo(with, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION); err != nil {
		t.Errorf("the owner can no longer read the DACL: %v", err)
	}
	// Through MARTA the rewrite is refused too — but once in 18 Windows CI
	// runs (39129398397) it went through. The product never writes security
	// through MARTA, so this is observed, not asserted.
	if err := set(with, "D:P(A;;FA;;;"+stranger+")"); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Logf("note: SetNamedSecurityInfo rewrote a DACL whose OWNER RIGHTS ACE grants READ_CONTROL only (err %v) — %s", err, fullSD(t, with))
	}
}

// Startup cost on a data directory with thousands of receipts: the first run
// (everything written) and the steady state (only read), with the progress
// callback the service uses for its start checkpoints.
func TestProtectTree_ThousandsOfFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	for d := 0; d < 30; d++ {
		sub := filepath.Join(dir, "history", "receipts", strconv.Itoa(1000+d))
		for f := 0; f < 100; f++ {
			mkfile(t, filepath.Join(sub, "KUPAIN_"+strconv.Itoa(f)+".doc"))
		}
	}
	var calls int
	opt := TreeOptions{Progress: func() { calls++ }}

	start := time.Now()
	rep, err := ProtectTree(dir, opt)
	first := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	firstCalls := calls
	start = time.Now()
	rep2, err := ProtectTree(dir, opt)
	steady := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d entries: first run %v (%d written), steady state %v (%d written)", rep.Entries, first, rep.Reset, steady, rep2.Reset)
	if rep.Entries != 3032 || rep.Reset != 3032 || rep2.Reset != 0 || rep2.RootSet {
		t.Errorf("first %+v / steady %+v; want 3032 written then nothing", rep.Entries, rep2.Reset)
	}
	if firstCalls != 3032/progressEvery || calls != 2*(3032/progressEvery) {
		t.Errorf("progress called %d then %d times; want %d per run", firstCalls, calls-firstCalls, 3032/progressEvery)
	}
	if first > 30*time.Second || steady > 30*time.Second {
		t.Errorf("ProtectTree took %v / %v on 3032 entries", first, steady)
	}
}
