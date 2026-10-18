//go:build windows

package dataacl

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
