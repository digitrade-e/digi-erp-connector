package provision

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// Plan 087 review T3: where a document folder may be, judged on the path
// Windows resolves, not only on the text configured. A junction above a
// folder (C:\Users\All Users is one, to C:\ProgramData) moves whatever a
// command does to the folder — a lock-down above all — into the folder it
// points at, and a text-only rule never sees it. So:
//
//   - a configured path with a reparse point anywhere on it — a junction, a
//     symbolic link, a mount point, from its drive down to the folder itself
//     — is refused (folder_reparse);
//   - a document folder is never a drive's root; never in, nor holding, the
//     Windows, Program Files, Program Files (x86) or ProgramData folders, the
//     profiles' folder (every profile: Public, Default, All Users), the
//     connector's install folder or its data directory; never in an AppData,
//     a System Volume Information or a $Recycle.Bin (folder_forbidden);
//   - folder-acl runs the same rules again on the path the opened folder's
//     handle resolves to, and refuses one that differs from the configured
//     path (folder_reparse).
//
// Scope: typed folders (the documents command), every folder-acl target, and
// — behind the link — the pre-087 folders (sendOrderDir, sendOrderPickupDir,
// the receipt folder) a command sets or moves.

// FolderRules are what a document folder may not be in or hold (Dirs), and
// the path parts it may not have (Parts), compared without case.
type FolderRules struct {
	Dirs  []string
	Parts []string
}

// machineFolderRules are this machine's FolderRules: its Windows, Program
// Files and ProgramData folders, the profiles' folder (profiles: the
// registry's ProfilesDirectory, "" when unread), and the install folder of
// the running program; AppData, System Volume Information and $Recycle.Bin
// as parts.
func machineFolderRules(profiles string) FolderRules {
	r := FolderRules{Parts: []string{"AppData", "System Volume Information", "$Recycle.Bin"}}
	add := func(d string) {
		if d = strings.TrimSpace(d); d != "" && !slices.ContainsFunc(r.Dirs, func(x string) bool { return strings.EqualFold(x, d) }) {
			r.Dirs = append(r.Dirs, d)
		}
	}
	for _, v := range []string{"SystemRoot", "windir", "ProgramFiles", "ProgramFiles(x86)", "ProgramW6432", "ProgramData", "ALLUSERSPROFILE"} {
		add(os.Getenv(v))
	}
	if d := strings.TrimSpace(os.Getenv("SystemDrive")); d != "" {
		for _, s := range []string{`\Windows`, `\Program Files`, `\Program Files (x86)`, `\ProgramData`, `\Users`, `\Documents and Settings`} {
			add(d + s)
		}
	}
	for _, v := range []string{"PUBLIC", "USERPROFILE"} { // a profile's parent is the profiles' folder
		if d := strings.TrimSpace(os.Getenv(v)); d != "" {
			add(winParent(d))
		}
	}
	add(profiles)
	if exe, err := os.Executable(); err == nil {
		add(filepath.Dir(exe))
	}
	return r
}

// documentFolderRefusal is why p may not be a document folder under r, or
// "" (see above).
func documentFolderRefusal(p string, r FolderRules) string {
	n := winNorm(p)
	if c := filepath.Clean(strings.TrimSpace(p)); n == "" || len(n) <= 3 && strings.HasSuffix(n, ":") || filepath.Dir(c) == c {
		return "it is a drive's root"
	}
	for _, part := range strings.Split(n, `\`) {
		for _, bad := range r.Parts {
			if part == strings.ToLower(bad) {
				return fmt.Sprintf("it is in a %s folder", bad)
			}
		}
	}
	for _, d := range r.Dirs {
		switch {
		case winUnder(p, []string{d}):
			return fmt.Sprintf("it is in %s", d)
		case winUnder(d, []string{p}):
			return fmt.Sprintf("it holds %s", d)
		}
	}
	return ""
}

// reparseAlong is the first part of p, from its drive (or /) down to p
// itself, that is a reparse point — a junction, a symbolic link, a mount
// point, any other reparse point Windows reports — or "" when none is. A part
// that does not exist ends the walk: nothing beyond it can be followed.
func reparseAlong(p string) (string, error) {
	p = filepath.Clean(strings.TrimSpace(p))
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%s is not an absolute path", p)
	}
	var parts []string
	for cur := p; ; {
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		parts = append(parts, cur)
		cur = parent
	}
	for i := len(parts) - 1; i >= 0; i-- {
		fi, err := os.Lstat(parts[i])
		if os.IsNotExist(err) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return parts[i], nil
		}
	}
	return "", nil
}

// finalDrivePath is a final path (GetFinalPathNameByHandle: \\?\C:\x) as a
// drive path (C:\x); ok is false for one on no drive letter (\\?\UNC\…,
// \\?\Volume{…}). Off Windows (the tests) a plain absolute path is one.
func finalDrivePath(final string) (string, bool) {
	p := strings.TrimPrefix(final, `\\?\`)
	if len(p) >= 3 && p[1] == ':' && p[2] == '\\' && (p[0]|0x20) >= 'a' && (p[0]|0x20) <= 'z' {
		return p, true
	}
	return p, runtime.GOOS != "windows" && filepath.IsAbs(p)
}

// folderRules are the machine's FolderRules with the data directory.
func (a *applier) folderRules() FolderRules {
	r := a.env.Sys.FolderRules()
	r.Dirs = append(slices.Clone(r.Dirs), a.dir)
	return r
}

// folderPathRefusal refuses document folder p (named what) when its path has
// a reparse point on it (folder_reparse) or the rules above refuse it
// (folder_forbidden).
func (a *applier) folderPathRefusal(what, p string) error {
	at, err := a.env.Sys.ReparsePointOn(p)
	if err != nil {
		return refuse(CodeFolderReparse, "run it elevated (as an administrator)", "read the folders on the way to %s (%s): %v", what, p, err)
	}
	if at != "" {
		return refuse(CodeFolderReparse, "name the folder by its real path: no junction, link or mount point on the way to it",
			"%s (%s): %s is a junction, a link or a mount point", what, p, at)
	}
	if why := documentFolderRefusal(p, a.folderRules()); why != "" {
		return refuse(CodeFolderForbidden, "name the folder the ERP reads: not a drive's root, a system or profile folder, nor the connector's own",
			"%s (%s): %s", what, p, why)
	}
	return nil
}

// checkSetFolderPaths: behind the link, the pre-087 folders this command
// sets or moves — sendOrderDir, sendOrderPickupDir, sendReceiptDir — under
// folderPathRefusal (review T3). Folders left where they were are not
// judged again.
func (a *applier) checkSetFolderPaths() error {
	if !LinkMode(a.dir) {
		return nil
	}
	for _, f := range []struct {
		name, cur, tgt string
		used           bool
	}{
		{"sendOrderDir", a.cur.SendOrderDir, a.tgt.SendOrderDir, a.tgt.OrdersConfigured()},
		{"sendOrderPickupDir", a.cur.SendOrderPickupDir, a.tgt.SendOrderPickupDir, a.tgt.OrdersPickup()},
		{"sendReceiptDir", a.cur.SendReceiptDir, a.tgt.SendReceiptDir, a.tgt.ERP == config.ERPHasavshevet},
	} {
		tgt, cur := strings.TrimSpace(f.tgt), strings.TrimSpace(f.cur)
		if !f.used || tgt == "" || (cur != "" && sameFolder(cur, tgt)) {
			continue
		}
		if err := a.folderPathRefusal(f.name, tgt); err != nil {
			return err
		}
		// review R4-1: a folder that exists already is judged by the path its handle resolves to, as folder-acl's —
		// a SUBST or mapped drive, an 8.3 name, a trailing dot are what Windows makes of them; one not made yet has
		// only its text, which link mode's path rule judged before anything was read
		final, err := a.env.Sys.FinalPath(tgt)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return refuse(CodeFolderReparse, "run it elevated (as an administrator)", "read where %s (%s) resolves: %v", f.name, tgt, err)
		default:
			if err := a.finalPathRefusal(f.name, tgt, final); err != nil {
				return err
			}
		}
	}
	return nil
}
