//go:build !windows

package atomicfile

import "os"

// publishNoReplace makes oldpath's file appear at newpath unless newpath
// exists: link(2) refuses an existing name (EEXIST, which errors.Is(err,
// fs.ErrExist) matches), and the file is whole before it gets the name. The
// temp name is removed afterwards by the caller.
func publishNoReplace(oldpath, newpath string) error {
	return os.Link(oldpath, newpath)
}
