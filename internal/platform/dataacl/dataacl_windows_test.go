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

func currentUserSID(t *testing.T) string {
	t.Helper()
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return tu.User.Sid.String()
}

// Applied to a real directory: protected, only our principals, and inherited
// by a file that was already there and by one created afterwards.
func TestProtectDir_AppliesAndPropagates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(filepath.Join(dir, "receipts"), 0o700); err != nil {
		t.Fatal(err)
	}
	before := filepath.Join(dir, "receipts", "index.json")
	if err := os.WriteFile(before, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := ProtectDir(dir); err != nil {
		t.Fatalf("ProtectDir: %v", err)
	}
	after := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(after, []byte("x: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	sddl, ctl := daclOf(t, dir)
	if ctl&windows.SE_DACL_PROTECTED == 0 {
		t.Errorf("%s: the DACL is not protected", sddl)
	}
	assertOnlyOurs(t, dir, sddl, currentUserSID(t))
	for _, p := range []string{before, after} {
		s, _ := daclOf(t, p)
		assertOnlyOurs(t, p, s, currentUserSID(t))
	}

	// Idempotent: a second start applies the same DACL without error.
	if err := ProtectDir(dir); err != nil {
		t.Fatalf("second ProtectDir: %v", err)
	}
}

func TestProtectFile_Applies(t *testing.T) {
	f := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(f, []byte("x: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ProtectFile(f); err != nil {
		t.Fatalf("ProtectFile: %v", err)
	}
	sddl, ctl := daclOf(t, f)
	if ctl&windows.SE_DACL_PROTECTED == 0 {
		t.Errorf("%s: the DACL is not protected", sddl)
	}
	assertOnlyOurs(t, f, sddl, currentUserSID(t))
}

func TestProtectDir_MissingIsAnError(t *testing.T) {
	if err := ProtectDir(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("ProtectDir on a missing directory = nil; want an error")
	}
}

// assertOnlyOurs checks every ACE in sddl names SYSTEM, Administrators or the
// current user.
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
		// The SID may come back as an alias (SY, BA, LA, …): resolve it.
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
	err := set(with, "D:P(A;;FA;;;"+stranger+")")
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("with OWNER RIGHTS = READ_CONTROL the owner rewrote the DACL (err %v); want ERROR_ACCESS_DENIED", err)
	}
	// READ_CONTROL is still there: the owner can read the DACL.
	if _, err := windows.GetNamedSecurityInfo(with, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION); err != nil {
		t.Errorf("the owner can no longer read the DACL: %v", err)
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

// onlyInherited fails unless every ACE on path is inherited and the DACL is
// not protected: nothing of its own survived.
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

// A child planted before the first protected start — a folder with its own
// protected DACL granting Everyone, a file with an explicit Everyone ACE —
// keeps its ACL through ProtectDir alone. ProtectTree drops both for the
// inherited DACL.
func TestProtectTree_ResetsPlantedACLs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	receipts := filepath.Join(dir, "receipts")
	if err := os.MkdirAll(receipts, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "planted.json")
	if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	user := currentUserSID(t)
	setDACL(t, receipts, "D:P(A;OICI;FA;;;WD)(A;OICI;FA;;;"+user+")", windows.PROTECTED_DACL_SECURITY_INFORMATION)
	setDACL(t, file, "D:(A;;FA;;;WD)", windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	child := filepath.Join(receipts, "index")
	if err := os.MkdirAll(child, 0o700); err != nil { // inherits Everyone from the planted folder
		t.Fatal(err)
	}

	rep, err := ProtectTree(dir)
	if err != nil {
		t.Fatalf("ProtectTree: %v", err)
	}
	if rep.Reset < 2 || rep.Entries != 3 {
		t.Errorf("report %+v; want 3 entries and at least the 2 planted ones reset", rep)
	}
	for _, p := range []string{receipts, child, file} {
		onlyInherited(t, p)
	}

	// Steady state: nothing of its own left, so a second start resets nothing.
	rep, err = ProtectTree(dir)
	if err != nil || rep.Reset != 0 {
		t.Errorf("second ProtectTree = %+v, %v; want nothing reset", rep, err)
	}
}

func mklinkJ(t *testing.T, link, target string) {
	t.Helper()
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		// Fatal, not Skip: on the Windows CI a junction needs no privilege, and a
		// skipped test here would prove nothing.
		t.Fatalf("mklink /J: %v %s", err, out)
	}
}

// A junction inside the data directory (or the directory itself being one)
// stops ProtectTree before anything is changed — and leaves the target alone.
func TestProtectTree_RefusesAJunction(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	before, _ := daclOf(t, target)
	mklinkJ(t, filepath.Join(dir, "history"), target)

	if _, err := ProtectTree(dir); !errors.Is(err, ErrReparsePoint) {
		t.Fatalf("ProtectTree with a junction inside = %v; want ErrReparsePoint", err)
	}
	if after, _ := daclOf(t, target); after != before {
		t.Errorf("the junction's target was changed:\n before %s\n after  %s", before, after)
	}
	if _, ctl := daclOf(t, dir); ctl&windows.SE_DACL_PROTECTED != 0 {
		t.Error("the data directory was protected although ProtectTree refused")
	}

	root := filepath.Join(t.TempDir(), "data-link")
	mklinkJ(t, root, target)
	if _, err := ProtectTree(root); !errors.Is(err, ErrReparsePoint) {
		t.Fatalf("ProtectTree on a junction = %v; want ErrReparsePoint", err)
	}
	if after, _ := daclOf(t, target); after != before {
		t.Errorf("a junctioned data directory's target was changed")
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
	rep, err := ProtectTree(dir)
	if err != nil || rep.OwnerSet || rep.OwnerErr != nil {
		t.Fatalf("ProtectTree = %+v, %v", rep, err)
	}
	if now := owner(); now != was {
		t.Errorf("owner %s -> %s; want unchanged", was, now)
	}
}

// Startup cost on a data directory with thousands of receipts: the first run
// (one planted folder to reset) and the steady state.
func TestProtectTree_ThousandsOfFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	for d := 0; d < 30; d++ {
		sub := filepath.Join(dir, "history", "receipts", strconv.Itoa(1000+d))
		if err := os.MkdirAll(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		for f := 0; f < 100; f++ {
			if err := os.WriteFile(filepath.Join(sub, "KUPAIN_"+strconv.Itoa(f)+".doc"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	setDACL(t, filepath.Join(dir, "history", "receipts", "1000"), "D:P(A;OICI;FA;;;WD)(A;OICI;FA;;;"+currentUserSID(t)+")",
		windows.PROTECTED_DACL_SECURITY_INFORMATION)

	start := time.Now()
	rep, err := ProtectTree(dir)
	first := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	rep2, err := ProtectTree(dir)
	steady := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("3032 entries: first %v (%d reset), steady %v (%d reset)", first, rep.Reset, steady, rep2.Reset)
	if rep.Reset != 1 || rep2.Reset != 0 {
		t.Errorf("reset %d then %d; want 1 then 0", rep.Reset, rep2.Reset)
	}
	if first > 30*time.Second || steady > 30*time.Second {
		t.Errorf("ProtectTree took %v / %v on 3032 entries", first, steady)
	}
}
