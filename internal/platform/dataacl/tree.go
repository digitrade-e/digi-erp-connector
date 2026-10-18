package dataacl

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ErrReparsePoint means the data directory, or something under it, is a
// symlink, a junction or another reparse point that redirects a path. Receipt
// data written through it would land outside the protected tree, so
// ProtectTree refuses and the daemon does not start. (Data Deduplication files,
// which Go reports as regular, are not refused.)
var ErrReparsePoint = errors.New("dataacl: a symlink or junction is in the data directory")

// Report is what ProtectTree did.
type Report struct {
	Entries  int   // entries under the directory (the directory itself not counted)
	Reset    int   // entries whose own DACL (protected, NULL or with explicit ACEs) was dropped for the inherited one
	OwnerSet bool  // the directory's owner was set to SYSTEM (only when running as SYSTEM)
	OwnerErr error // setting the owner failed; not fatal
}

// refusedMode reports whether an entry with this Lstat mode stops
// ProtectTree. On Windows Go reports a symlink as ModeSymlink and a junction or
// other name-surrogate reparse point as ModeIrregular.
func refusedMode(m fs.FileMode) bool {
	return m&(fs.ModeSymlink|fs.ModeIrregular) != 0
}

// ownAsSystem reports whether ProtectTree makes SYSTEM the owner: only when
// the process runs as SYSTEM, the one case where it can always assign that
// owner (WRITE_OWNER comes with its Full Control ACE, and SYSTEM is its own
// token user). Elsewhere the owner is left as it is.
func ownAsSystem(tokenSID string) bool { return tokenSID == SIDLocalSystem }

// scanTree lists every entry under root (root excluded), parents before
// children, never following a link: root itself or any entry that is a
// symlink or junction is ErrReparsePoint.
func scanTree(root string) ([]string, error) {
	fi, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if refusedMode(fi.Mode()) {
		return nil, fmt.Errorf("%w: %s", ErrReparsePoint, root)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("dataacl: %s is not a directory", root)
	}
	var out []string
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		info, err := d.Info() // Lstat
		if err != nil {
			return err
		}
		if refusedMode(info.Mode()) {
			return fmt.Errorf("%w: %s", ErrReparsePoint, p)
		}
		out = append(out, p)
		return nil
	})
	return out, err
}
