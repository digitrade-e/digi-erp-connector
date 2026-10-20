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
