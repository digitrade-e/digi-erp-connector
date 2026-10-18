//go:build !windows

package dataacl

import "os"

// ProtectTree does nothing outside Windows (see the package doc).
func ProtectTree(string, TreeOptions) (Report, error) { return Report{}, nil }

// ProtectFile does nothing outside Windows.
func ProtectFile(string) error { return nil }

// FileTrusted is always true outside Windows.
func FileTrusted(string) (bool, error) { return true, nil }

// OpenLog opens path for appending, creating it 0600.
func OpenLog(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}
