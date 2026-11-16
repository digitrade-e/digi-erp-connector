package provision

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Plan 087 review m6, the rest (connector v1.0.32): behind the link the image
// folders and the importer files a command sets or changes are judged where
// Windows resolves them, as R4-1 judges sendOrderDir and T3 the document
// folders. Link mode's text rules (linkPathRefusal, the system and profile
// folders) ran before anything was read; then, on the box:
//
//   - a junction, a symbolic link or a mount point anywhere on the path, its
//     own name included, is refused (ReparsePointOn);
//   - a path that exists must resolve — the handle's final path, opened
//     without following it — to the configured path itself: a SUBST or
//     mapped drive, an 8.3 name, a link the text rules never see, are
//     refused. One not there yet keeps its text (the existence checks refuse
//     a missing importer or image folder).
//
// An image folder is image_folder_forbidden, an importer file
// importer_not_erp. Paths left as they were are not judged again, and WinRM
// is unchanged.

// checkLinkedPaths judges, behind the link, the image folders and importer
// files a.tgt sets that a.cur did not have.
func (a *applier) checkLinkedPaths() error {
	if !LinkMode(a.dir) {
		return nil
	}
	for _, d := range a.tgt.ImageFolders {
		if slices.ContainsFunc(a.cur.ImageFolders, func(c string) bool { return sameFolder(c, d) }) {
			continue
		}
		if err := a.judgeLinkedPath("image folder", d, false, CodeImageFolderForbidden); err != nil {
			return err
		}
	}
	cur := importerFilesOf(a.cur.HasExePath, a.cur.HasBatFile, a.cur.HasParamFile, a.cur.SendOrderDir, a.cur.HasReceiptBatFile, a.cur.HasReceiptParamFile)
	for _, f := range importerFilesOf(a.tgt.HasExePath, a.tgt.HasBatFile, a.tgt.HasParamFile, a.tgt.SendOrderDir, a.tgt.HasReceiptBatFile, a.tgt.HasReceiptParamFile) {
		if slices.ContainsFunc(cur, func(c string) bool { return sameFolder(c, f) }) {
			continue
		}
		if err := a.judgeLinkedPath("importer file", f, true, CodeImporterNotERP); err != nil {
			return err
		}
	}
	return nil
}

// importerFilesOf are the importer files a config names: has.exe, the BATs,
// the parameter files (a relative order parameter file is in sendOrderDir).
func importerFilesOf(hasExe, bat, param, sendOrderDir, receiptBat, receiptParam string) []string {
	var out []string
	for _, p := range []string{hasExe, bat, receiptBat, receiptParam} {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if p := strings.TrimSpace(param); p != "" {
		if !filepath.IsAbs(p) && strings.TrimSpace(sendOrderDir) != "" {
			p = filepath.Join(strings.TrimSpace(sendOrderDir), p)
		}
		out = append(out, p)
	}
	return out
}

// judgeLinkedPath refuses p (named what; a file or a folder) with code when
// a reparse point is on its path, or when it exists and resolves elsewhere.
func (a *applier) judgeLinkedPath(what, p string, file bool, code string) error {
	at, err := a.env.Sys.ReparsePointOn(p)
	if err != nil {
		return refuse(code, "run it elevated (as an administrator)", "read the folders on the way to %s %s: %v", what, p, err)
	}
	if at != "" {
		return refuse(code, "name it by its real path: no junction, link or mount point on the way to it",
			"%s %s: %s is a junction, a link or a mount point", what, p, at)
	}
	final, err := a.env.Sys.FinalPath(p)
	if file {
		final, err = a.env.Sys.FinalFilePath(p)
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil // not there yet: its text was judged; the existence checks refuse a missing one
	case err != nil:
		return refuse(code, "name it by its real path: no junction, link, hard link or mount point", "read where %s %s resolves: %v", what, p, err)
	}
	if fp, ok := finalDrivePath(final); !ok || !strings.EqualFold(filepath.Clean(fp), filepath.Clean(p)) {
		return refuse(code, "name it by its real path: its long name, on a local drive, no SUBST or mapped drive",
			"%s %s resolves to %s", what, p, fp)
	}
	return nil
}
