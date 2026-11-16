//go:build !windows

package dataacl

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// OpenRead opens path, a file under root, for reading, never through a symlink: a symlink, a multiply-linked file, a
// directory and anything outside root are refused (the Windows version works by handle; see read_windows.go).
func OpenRead(root, path string) (*os.File, error) {
	if err := PlainDir(root); err != nil {
		return nil, err
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: %s", ErrReparsePoint, path)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("dataacl: %s is not a regular file", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || !os.SameFile(fi, st) {
		_ = f.Close()
		return nil, fmt.Errorf("dataacl: %s changed while it was opened", path)
	}
	if s, ok := st.Sys().(*syscall.Stat_t); ok && s.Nlink > 1 {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s (%d links)", ErrHardLink, path, s.Nlink)
	}
	realRoot, err1 := filepath.EvalSymlinks(root)
	realDir, err2 := filepath.EvalSymlinks(filepath.Dir(path))
	rel, err3 := filepath.Rel(realRoot, filepath.Join(realDir, filepath.Base(path)))
	if err1 != nil || err2 != nil || err3 != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s", ErrOutsideTree, path)
	}
	return f, nil
}

// FinalDirPath is dir, cleaned, when it is a directory and not a symlink: off Windows a path has no other spelling to
// resolve (the Windows version reads it from a handle).
func FinalDirPath(dir string) (string, error) {
	if err := PlainDir(dir); err != nil {
		return "", err
	}
	return filepath.Clean(dir), nil
}

// PlainDir reports an error unless path is a directory and not a symlink.
func PlainDir(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", ErrReparsePoint, path)
	}
	if !fi.IsDir() {
		return fmt.Errorf("dataacl: %s is not a directory", path)
	}
	return nil
}
