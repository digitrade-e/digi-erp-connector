//go:build !windows

package dataacl

import (
	"fmt"
	"os"
	"syscall"
)

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

// FileID identifies the file object at path (device and inode).
func FileID(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("dataacl: no inode for %s", path)
	}
	return fmt.Sprintf("%x-%x", uint64(st.Dev), st.Ino), nil
}
