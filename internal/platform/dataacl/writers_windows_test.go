//go:build windows

package dataacl

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"golang.org/x/sys/windows"
)

func wellKnownName(t *testing.T, k windows.WELL_KNOWN_SID_TYPE) string {
	t.Helper()
	sid, err := windows.CreateWellKnownSid(k)
	if err != nil {
		t.Fatal(err)
	}
	return accountName(sid)
}

// Writers names whoever besides the administrators can change a file, or add
// to and delete from its folder — and nobody for a file only they control.
func TestWriters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Hash7")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	bat := filepath.Join(dir, "importorders.bat")
	if err := os.WriteFile(bat, []byte("@echo off\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const adminsOnly = "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;OW)"
	setDACL(t, dir, adminsOnly+"(A;OICIIO;FA;;;CO)", windows.PROTECTED_DACL_SECURITY_INFORMATION)
	setDACL(t, bat, adminsOnly, windows.PROTECTED_DACL_SECURITY_INFORMATION)
	if who, err := Writers(bat); err != nil || len(who) != 0 {
		t.Fatalf("an administrators-only file: %v, %v", who, err)
	}

	// Everyone may write the file.
	setDACL(t, bat, adminsOnly+"(A;;0x12019f;;;WD)", windows.PROTECTED_DACL_SECURITY_INFORMATION)
	who, err := Writers(bat)
	if err != nil || !slices.Contains(who, wellKnownName(t, windows.WinWorldSid)) {
		t.Errorf("Everyone writes the file: %v, %v", who, err)
	}

	// Users may add files to its folder (a BAT's bare command names resolve
	// there first); read-only Users on the file itself change nothing.
	setDACL(t, bat, adminsOnly+"(A;;FR;;;BU)", windows.PROTECTED_DACL_SECURITY_INFORMATION)
	setDACL(t, dir, adminsOnly+"(A;;0x1200a9;;;BU)(A;;0x2;;;BU)", windows.PROTECTED_DACL_SECURITY_INFORMATION)
	who, err = Writers(bat)
	if err != nil || !slices.Contains(who, wellKnownName(t, windows.WinBuiltinUsersSid)) {
		t.Errorf("Users add to the folder: %v, %v", who, err)
	}
}

// Review m12/m8: a folder above the file that Users may rename a child of (FILE_DELETE_CHILD) lets them swap the
// whole folder; so does DELETE on the file's own folder. Creating folders in an ancestor (as Users may in C:\) does not.
func TestWritersLooksAtEveryFolderAbove(t *testing.T) {
	const adminsOnly = "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;OW)"
	top := filepath.Join(t.TempDir(), "top")
	dir := filepath.Join(top, "Hash7")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cert := filepath.Join(dir, "cert.pem")
	if err := os.WriteFile(cert, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	setDACL(t, cert, adminsOnly, windows.PROTECTED_DACL_SECURITY_INFORMATION)
	setDACL(t, dir, adminsOnly, windows.PROTECTED_DACL_SECURITY_INFORMATION)
	users := wellKnownName(t, windows.WinBuiltinUsersSid)

	// Users may create folders in the grandparent: harmless
	setDACL(t, top, adminsOnly+"(A;;0x1200a9;;;BU)(A;;0x4;;;BU)", windows.PROTECTED_DACL_SECURITY_INFORMATION)
	if who, err := Writers(cert); err != nil || slices.Contains(who, users) {
		t.Fatalf("adding to a folder above: %v, %v", who, err)
	}
	// Users may delete children of the grandparent: they can rename Hash7 away and put their own in its place
	setDACL(t, top, adminsOnly+"(A;;0x1200a9;;;BU)(A;;0x44;;;BU)", windows.PROTECTED_DACL_SECURITY_INFORMATION)
	if who, err := Writers(cert); err != nil || !slices.Contains(who, users) {
		t.Fatalf("delete-child on a folder above: %v, %v", who, err)
	}
	// DELETE on the file's own folder: the same
	setDACL(t, top, adminsOnly, windows.PROTECTED_DACL_SECURITY_INFORMATION)
	setDACL(t, dir, adminsOnly+"(A;;0x10000;;;BU)", windows.PROTECTED_DACL_SECURITY_INFORMATION)
	if who, err := Writers(cert); err != nil || !slices.Contains(who, users) {
		t.Fatalf("delete on the folder: %v, %v", who, err)
	}
}

// Plan 087 review M1: a folder the connector writes documents into. Adding a file to it or deleting one, renaming it
// away (DELETE on it, delete-child on its parent) name the account; adding entries to the parent — which Users may do
// in most folders an administrator makes under C:\ — does not, though Writers' file case judges the parent by that.
func TestFolderWriters(t *testing.T) {
	const adminsOnly = "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;OW)"
	top := filepath.Join(t.TempDir(), "xampp")
	dir := filepath.Join(top, "herp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	users := wellKnownName(t, windows.WinBuiltinUsersSid)
	setDACL(t, top, adminsOnly, windows.PROTECTED_DACL_SECURITY_INFORMATION)
	setDACL(t, dir, adminsOnly, windows.PROTECTED_DACL_SECURITY_INFORMATION)
	if who, err := FolderWriters(dir); err != nil || len(who) != 0 {
		t.Fatalf("an administrators-only folder: %v, %v", who, err)
	}
	for _, tc := range []struct {
		name           string
		topACE, dirACE string
		named          bool
	}{
		{"Users create files in it", "", "(A;;0x1200a9;;;BU)(A;;0x2;;;BU)", true},
		{"Users create folders in it", "", "(A;;0x1200a9;;;BU)(A;;0x4;;;BU)", true},
		{"Users delete entries in it", "", "(A;;0x1200a9;;;BU)(A;;0x40;;;BU)", true},
		{"Users rename it away", "", "(A;;0x10000;;;BU)", true},
		{"Users read it", "", "(A;;0x1200a9;;;BU)", false},
		{"Users create files and folders in the parent", "(A;;0x1200a9;;;BU)(A;;0x6;;;BU)", "", false},
		{"Users delete entries in the parent", "(A;;0x1200a9;;;BU)(A;;0x40;;;BU)", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setDACL(t, top, adminsOnly+tc.topACE, windows.PROTECTED_DACL_SECURITY_INFORMATION)
			setDACL(t, dir, adminsOnly+tc.dirACE, windows.PROTECTED_DACL_SECURITY_INFORMATION)
			who, err := FolderWriters(dir)
			if err != nil || slices.Contains(who, users) != tc.named {
				t.Fatalf("FolderWriters = %v, %v; Users named: want %v", who, err, tc.named)
			}
		})
	}
}
