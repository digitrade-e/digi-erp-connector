//go:build windows

package dataacl

import (
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
	aces := strings.Split(strings.Trim(sddl[i+2:], "PAIRCU"), "(")
	n := 0
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
		default:
			t.Errorf("%s: ACE for %s in %s; want only SYSTEM, Administrators, %s", path, parts[5], sddl, user)
		}
	}
	if n == 0 {
		t.Errorf("%s: empty DACL %s", path, sddl)
	}
}
