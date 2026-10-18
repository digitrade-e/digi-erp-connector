//go:build !windows

package dataacl

// ProtectDir does nothing outside Windows (see the package doc).
func ProtectDir(string) error { return nil }

// ProtectFile does nothing outside Windows.
func ProtectFile(string) error { return nil }
