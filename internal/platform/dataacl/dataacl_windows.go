//go:build windows

package dataacl

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// ProtectDir sets the protected data-directory DACL on dir. Windows
// propagates its inheritable ACEs to everything already under dir (an
// existing child's own explicit ACEs are kept); new children inherit them.
func ProtectDir(dir string) error { return protect(dir, true) }

// ProtectFile sets the protected DACL on one file.
func ProtectFile(path string) error { return protect(path, false) }

func protect(path string, container bool) error {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("dataacl: read the process token user: %w", err)
	}
	sddl, err := SDDL(tu.User.Sid.String(), container)
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return fmt.Errorf("dataacl: parse %s: %w", sddl, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("dataacl: %w", err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("dataacl: set the protected DACL on %s: %w", path, err)
	}
	return nil
}
