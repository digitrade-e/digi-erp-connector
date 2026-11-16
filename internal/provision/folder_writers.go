package provision

import (
	"path/filepath"
	"runtime"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// Plan 087 review M1: who can write to a folder the connector writes
// documents into. The connector writes there as LocalSystem and an ERP
// importer reads it, so whoever else can add a file to it can pre-create the
// next document's name (every order into it then fails for good, each one
// burning a number), drop a document the importer takes into the books, or —
// where an importer runs with the folder as its working directory (has.exe in
// sendOrderDir, the receipt importer in the receipt folder) — plant a program
// or a DLL it loads as LocalSystem. On a default Windows Server every folder
// an administrator creates under C:\ lets every local user create files in
// it, so this is the normal case until the folder is locked down
// (docs/troubleshooting.md § A folder others can write to).
//
// Behind the link such a folder is refused (folder_writable): what a signed
// command can make a box do is bounded tighter than over WinRM, where
// allowWritable accepts it as a risk a human was shown — the importer's rule
// (checkImporterWriters).

// The scope (review R1a): a command refuses only a folder it adds, moves (a
// new path), or newly makes typed or bound — checkFolderWriters. Every other
// folder the box writes documents into that others can write to is reported
// as the daemon reports it, a warning and an entry in the result's
// writableFolders (reportWritableFolders): a box upgraded with folders never
// locked down keeps taking the plan-086 commands, and behind the link the
// ERP's own scheduler, which must write there, is trusted by name
// (documents' trustedWriters) or the folder locked down by folder-acl.

// folderKey compares folders as the file system does (cleaned, without case
// on Windows) for the folderChecked set.
func folderKey(p string) string {
	p = filepath.Clean(strings.TrimSpace(p))
	if runtime.GOOS == "windows" {
		return strings.ToLower(p)
	}
	return p
}

// checkFolderWriters refuses folder path (named what), one the command adds,
// moves, or newly makes typed or bound, when accounts besides the
// administrators can write to it; over WinRM allow accepts it with a
// warning.
func (a *applier) checkFolderWriters(what, path string, allow bool) error {
	if a.folderChecked == nil {
		a.folderChecked = map[string]bool{}
	}
	a.folderChecked[folderKey(path)] = true
	who, err := a.env.Sys.FolderWriters(path)
	if err != nil {
		return refuse(CodeFolderWritable, "run it elevated (as an administrator)", "read who can write to %s (%s): %v", what, path, err)
	}
	if len(who) == 0 {
		return nil
	}
	linked := LinkMode(a.dir)
	if allow && !linked {
		a.warn("folder_writable: %s (%s) can be written by %s, and an importer reads it as the connector writes it (LocalSystem)",
			what, path, strings.Join(who, ", "))
		a.res.WritableFolders = append(a.res.WritableFolders, StateWritableFolder{WriteFolder: config.WriteFolder{Name: what, Path: path}, Writers: who})
		return nil
	}
	fix := "let only the administrators (and SYSTEM) write to it and rename it — docs/troubleshooting.md § A folder others can write to"
	if !linked {
		fix += " — or set allowWritable once a human accepted the risk"
	}
	return refuse(CodeFolderWritable, fix,
		"%s (%s) can be written by %s: any of them could pre-create the next document's file, drop a document the importer takes into the books, or plant what an importer running there loads",
		what, path, strings.Join(who, ", "))
}

// reportWritableFolders adds to the result every folder of cfg that others
// can write to and no checkFolderWriters looked at: a warning and a
// writableFolders entry, never a refusal (review R1a).
func (a *applier) reportWritableFolders(cfg config.Config) {
	for _, f := range cfg.WriteFolders() {
		if a.folderChecked[folderKey(f.Path)] {
			continue
		}
		who, err := a.env.Sys.FolderWriters(f.Path)
		switch {
		case err != nil:
			a.res.WritableFolders = append(a.res.WritableFolders, StateWritableFolder{WriteFolder: f, Writers: []string{}, Error: err.Error()})
			a.warn("folder_writable: who can write to %s (%s) could not be read: %v", f.Name, f.Path, err)
		case len(who) > 0:
			a.res.WritableFolders = append(a.res.WritableFolders, StateWritableFolder{WriteFolder: f, Writers: who})
			a.warn("folder_writable: %s (%s) can be written by %s; this command leaves it as it is, so it is a warning (docs/troubleshooting.md § A folder others can write to)",
				f.Name, f.Path, strings.Join(who, ", "))
		}
	}
}

// refuseAllowWritableLinked is Validate's refusal of allowWritable behind the
// link, where a folder others can write to is never accepted.
func refuseAllowWritableLinked(allow bool) *Refusal {
	if !allow {
		return nil
	}
	return refuse(CodeSpecInvalid, "leave allowWritable out: behind the link a folder the connector writes into is one only administrators can write to",
		"allowWritable is refused behind the link")
}
