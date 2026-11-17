//go:build windows

package dataacl

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

// OpenRead opens path, a file under root, for reading — by handle and never through a link (plan 086 C4: the
// diagnostics read the connector's own files in the data directory, as SYSTEM). It refuses a reparse point, a
// multiply-linked file, a directory and anything whose final path is not under root's own final path, so a link
// planted under a log's name cannot make it read another file. The share mode lets the daemon keep appending to the
// file and move it aside while it is read. A missing path wraps os.ErrNotExist.
func OpenRead(root, path string) (*os.File, error) {
	rootFinal, err := plainFinalPath(root)
	if err != nil {
		return nil, err
	}
	p, err := windows.UTF16PtrFromString(longPath(path))
	if err != nil {
		return nil, err
	}
	var h windows.Handle
	err = retryShare(func() (e error) {
		h, e = windows.CreateFile(p, windows.GENERIC_READ,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		return e
	})
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrNotExist}
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	if err := refuseLinks(path, info); err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("dataacl: %s is a directory", path)
	}
	final, err := finalPath(h)
	if err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	if !underRoot(rootFinal, final) {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("%w: %s", ErrOutsideTree, path)
	}
	return os.NewFile(uintptr(h), path), nil
}

// PlainDir reports an error unless path is a directory that is not a reparse point (a junction or a symlink): a
// folder the connector creates or writes in as SYSTEM, checked by handle.
func PlainDir(path string) error {
	_, err := plainFinalPath(path)
	return err
}

// FinalDirPath is the path Windows resolves directory dir to, read from a handle opened on it without following it
// (GetFinalPathNameByHandle: \\?\C:\x) — an 8.3 name, a trailing dot, a SUBST or mapped drive come back as what they
// are. A reparse point or a file is refused; one that does not exist wraps os.ErrNotExist.
func FinalDirPath(dir string) (string, error) { return plainFinalPath(dir) }

// FinalFilePath is FinalDirPath for a file (v1.0.32, review m6: the importer
// files a command names): opened without following it — a reparse point or a
// multiply-linked file is refused, a folder too — its final path.
func FinalFilePath(path string) (string, error) {
	n, err := open(path, windows.READ_CONTROL)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(n.h)
	if n.dir() {
		return "", fmt.Errorf("dataacl: %s is a folder, not a file", path)
	}
	return finalPath(n.h)
}

// plainFinalPath opens dir (not following it) and returns its final path; a reparse point or a file is refused.
func plainFinalPath(dir string) (string, error) {
	n, err := open(dir, windows.READ_CONTROL)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(n.h)
	if !n.dir() {
		return "", fmt.Errorf("dataacl: %s is not a directory", dir)
	}
	return finalPath(n.h)
}

// underRoot: final names a path inside rootFinal (both GetFinalPathNameByHandle's form, compared without case).
func underRoot(rootFinal, final string) bool {
	r := strings.TrimRight(rootFinal, `\`) + `\`
	return len(final) > len(r) && strings.EqualFold(final[:len(r)], r)
}
