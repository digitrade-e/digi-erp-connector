package provision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/atomicfile"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
)

// Plan 087 review R1c: the folder-acl command — digi-mcp locks down a folder
// the connector writes documents into itself, instead of a human running
// icacls on the box (docs/troubleshooting.md keeps the recipe as background).
// Signed (folder-acl, folder-acl-check over the link; WinRM too); not sealed.
//
//   - op lockdown, for named folders only: a typed folder's key, or the roles
//     sendOrderDir, pickup and sendReceiptDir, resolved from config.yaml —
//     never a path. A lock-down never grants anything, to anyone, on any
//     entry (review T2): the folder's and every entry's DACL beneath (by
//     handle, at most dataacl.MaxLockdownEntries entries, no reparse point or
//     hard link anywhere beneath) is masked in place — SYSTEM,
//     Administrators and TrustedInstaller unchanged, a documents.trustedWriters
//     account capped at Modify, anyone else at read & execute, no entry
//     added — the folder cut from its parent, and an untrusted owner handed
//     to Administrators. Parents are never changed: who can still rename the
//     folder away through one is reported (parent_writable).
//   - Before anything is written the previous owner and DACL of the folder
//     and of every entry beneath are recorded in a protected file,
//     DataDir\folder-acl\<sha256>.json — the sha256 of its bytes — answered
//     as the folder's preimage.
//   - op restore {folder, preimage} puts back only a pre-image this box
//     recorded for that folder at its configured path, never an SDDL.
//   - The bound, literally (review T2): a stolen team key can only lower
//     rights on a configured document folder's tree — never raise one, never
//     add an entry, never touch a folder code runs from (T1) or a place a
//     document folder may not be (T3) — or put back an ACL that tree had.
//   - --check reads and writes nothing: per folder, every principal with an
//     allow entry anywhere in the tree, the strongest rights it has anywhere
//     before and after and on how many entries they change (review T4), the
//     owners handed to Administrators, and how many entries change.
//   - Refused: a reparse point or hard link beneath, a junction, link or
//     mount point on the configured path, or a folder whose handle resolves
//     to another path (folder_reparse; T3); more entries than the bound
//     (folder_too_large); unfinished work in the folder (folder_in_flight, as
//     the documents command); a drive's root, the system, profile and
//     ProgramData folders, any AppData, the install folder or the data
//     directory, or a folder holding one (folder_forbidden; T3); a folder
//     holding the importer's files or executables (folder_has_executables;
//     T1). Where an importer runs no trusted writer is kept (T1).
//   - Afterwards the service is restarted (the queue asked first) and
//     proved: the daemon reads the folders' new ACLs at its start.

// The folder-acl ops and the roles a folder may be named by.
const (
	FolderACLLockdown        = "lockdown"
	FolderACLRestore         = "restore"
	FolderRoleSendOrderDir   = "sendOrderDir"
	FolderRolePickup         = "pickup"
	FolderRoleSendReceiptDir = "sendReceiptDir"
	maxFolderACLFolders      = 8
)

// FolderACLInput is the folder-acl command's stdin.
type FolderACLInput struct {
	Version   int    `json:"version"` // 1
	ChangeSet string `json:"changeSet"`
	Op        string `json:"op"` // lockdown | restore
	// Folders are lockdown's: typed folder keys or roles (sendOrderDir,
	// pickup, sendReceiptDir), at most 8.
	Folders []string `json:"folders,omitempty"`
	// Folder and Preimage are restore's: the folder's name and the sha256 a
	// lockdown of it answered.
	Folder   string `json:"folder,omitempty"`
	Preimage string `json:"preimage,omitempty"`
	// AcceptUnreadable: the in-flight check reads the indexes (review R7).
	AcceptUnreadable *int    `json:"acceptUnreadable,omitempty"`
	AssumeIdle       bool    `json:"assumeIdle,omitempty"`
	Expect           *Expect `json:"expect,omitempty"`
}

// FolderACLResult is the command's answer (ApplyResult.FolderACL).
type FolderACLResult struct {
	Op      string            `json:"op"`
	Folders []FolderACLFolder `json:"folders"`
}

// FolderACLFolder is one folder of the answer.
type FolderACLFolder struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// Entries: the folder and every entry beneath (lockdown: found; restore:
	// recorded).
	Entries int `json:"entries"`
	// EntriesChanged (lockdown) is how many entries' DACL or owner changes.
	EntriesChanged int `json:"entriesChanged,omitempty"`
	// Principals (lockdown) are every principal with an allow entry anywhere
	// in the tree: the strongest rights it has anywhere before → after, and
	// on how many entries they change (review T2/T4).
	Principals []dataacl.PrincipalRights `json:"principals,omitempty"`
	// Owners (lockdown) are the owners handed to Administrators, by entries.
	Owners []dataacl.OwnerChange `json:"owners,omitempty"`
	// ParentWritable (lockdown) names who can still rename the folder away
	// through a folder above (parents are never changed).
	ParentWritable []string `json:"parentWritable,omitempty"`
	// Preimage (lockdown, not --check) is the sha256 a restore takes.
	Preimage string `json:"preimage,omitempty"`
	// Written (lockdown), Restored and Skipped (restore: entries gone or
	// replaced since) count what was done.
	Written  int `json:"written,omitempty"`
	Restored int `json:"restored,omitempty"`
	Skipped  int `json:"skipped,omitempty"`
}

// folderACLPreimage is what a lockdown records before it writes.
type folderACLPreimage struct {
	Version   int                   `json:"version"`
	Folder    string                `json:"folder"`
	Path      string                `json:"path"`
	ChangeSet string                `json:"changeSet"`
	Created   string                `json:"created"`
	Entries   []dataacl.FolderEntry `json:"entries"`
}

var preimageSHA = regexp.MustCompile(`^[0-9a-f]{64}$`)

func folderACLDir(dataDir string) string { return filepath.Join(dataDir, "folder-acl") }

// DecodeFolderACL reads one JSON input, strictly like DecodeSpec.
func DecodeFolderACL(r io.Reader) (FolderACLInput, *Refusal) {
	var in FolderACLInput
	if err := decodeStrict(r, &in); err != nil {
		return FolderACLInput{}, refuse(CodeSpecInvalid, "send one JSON object with only the documented fields", "the folder-acl input does not decode: %v", err)
	}
	return in, nil
}

// validFolderName: a typed folder's key, or one of the roles.
func validFolderName(n string) bool {
	switch n {
	case FolderRoleSendOrderDir, FolderRolePickup, FolderRoleSendReceiptDir:
		return true
	}
	return config.ValidDocumentKey(n)
}

// Validate checks the input before anything is read.
func (in FolderACLInput) Validate() *Refusal {
	if r := validateHeader(in.Version, in.ChangeSet, in.Expect); r != nil {
		return r
	}
	if r := validAcceptUnreadable(in.AcceptUnreadable); r != nil {
		return r
	}
	nameFix := "name a typed folder's key, or sendOrderDir, pickup or sendReceiptDir — never a path"
	switch in.Op {
	case FolderACLLockdown:
		if in.Folder != "" || in.Preimage != "" {
			return refuse(CodeSpecInvalid, "lockdown takes folders only", "folder and preimage are restore's")
		}
		if len(in.Folders) == 0 || len(in.Folders) > maxFolderACLFolders {
			return refuse(CodeSpecInvalid, fmt.Sprintf("name 1 to %d folders", maxFolderACLFolders), "lockdown names %d folders", len(in.Folders))
		}
		seen := map[string]bool{}
		for _, n := range in.Folders {
			if !validFolderName(n) || seen[n] {
				return refuse(CodeSpecInvalid, nameFix, "%q is not a folder name, or is named twice", n)
			}
			seen[n] = true
		}
	case FolderACLRestore:
		if len(in.Folders) != 0 {
			return refuse(CodeSpecInvalid, "restore takes folder and preimage", "folders are lockdown's")
		}
		if !validFolderName(in.Folder) {
			return refuse(CodeSpecInvalid, nameFix, "%q is not a folder name", in.Folder)
		}
		if !preimageSHA.MatchString(in.Preimage) {
			return refuse(CodeSpecInvalid, "give the preimage (a sha256, 64 lower-case hex digits) the lockdown answered", "preimage %q is not a sha256", in.Preimage)
		}
	default:
		return refuse(CodeSpecInvalid, `use "lockdown" or "restore"`, "op %q is not a folder-acl op", in.Op)
	}
	return nil
}

// ApplyFolderACL runs the folder-acl command (check: --check, nothing written).
func ApplyFolderACL(ctx context.Context, env Env, in FolderACLInput, check bool) (*ApplyResult, error) {
	res := &ApplyResult{ChangeSet: in.ChangeSet, Steps: []Step{}}
	if r := in.Validate(); r != nil {
		return res, r
	}
	a := &applier{env: env, s: Spec{Version: in.Version, ChangeSet: in.ChangeSet, AssumeIdle: in.AssumeIdle, Expect: in.Expect},
		dir: env.Sys.DataDir(), res: res, check: check, acceptUnreadable: in.AcceptUnreadable}
	unlock, err := lockRuns(a.dir)
	if err != nil {
		return res, err
	}
	defer unlock()
	if err := a.readCurrent(); err != nil {
		return res, err
	}
	if a.curBytes == nil {
		return res, refuse(CodeConfigRefused, "provision the connector with apply first", "there is no config.yaml: this command locks down a configured connector's folders")
	}
	a.tgt = a.cur
	res.FolderACL = &FolderACLResult{Op: in.Op, Folders: []FolderACLFolder{}}
	if in.Op == FolderACLLockdown {
		err = a.folderLockdown(ctx, in)
	} else {
		err = a.folderACLRestore(ctx, in)
	}
	a.settle()
	return res, err
}

// resolveFolder is a folder name's configured path, refused when it is not
// configured, has a reparse point on it (folder_reparse), or is in or holds
// a folder a document folder may not be (folder_forbidden; review T3:
// documentFolderRefusal — a drive's root, the system, profile and
// ProgramData folders, any AppData, the connector's install folder and data
// directory). PlanFolderLockdown's final path is judged again
// (finalPathRefusal). A restore (lockdown false) skips the T3 rules: it
// puts back only a pre-image this box recorded for the folder, on objects
// that are still the ones recorded — an undo is never blocked.
func (a *applier) resolveFolder(name string, lockdown bool) (string, error) {
	var p string
	switch name {
	case FolderRoleSendOrderDir:
		if a.cur.OrdersConfigured() {
			p = a.cur.SendOrderDir
		}
	case FolderRolePickup:
		if a.cur.OrdersPickup() {
			p = a.cur.SendOrderPickupDir
		}
	case FolderRoleSendReceiptDir:
		if a.cur.ERP == config.ERPHasavshevet {
			p = a.cur.SendReceiptDir
		}
	default:
		p = a.cur.Documents.Folders[name].Path
	}
	p = strings.TrimSpace(p)
	if p == "" {
		return "", refuse(CodeFolderUnknown, "name a folder config.yaml has (state lists them)", "there is no folder %s on this box", name)
	}
	if !filepath.IsAbs(p) || inside(p, a.dir) || inside(a.dir, p) {
		return "", refuse(CodeFolderForbidden, "lock down the folder the ERP's importer reads, not a drive, a system folder or the connector's data directory",
			"folder %s (%s) is not absolute, or is or holds the data directory", name, p)
	}
	if lockdown {
		if err := a.folderPathRefusal("folder "+name, p); err != nil {
			return "", err
		}
	}
	return p, nil
}

// finalPathRefusal judges the path the opened folder's handle resolves to
// (review T3): on a drive, under the same rules, and the configured path
// itself — anything else means Windows took the configured text elsewhere.
func (a *applier) finalPathRefusal(name, p, final string) error {
	fp, ok := finalDrivePath(final)
	if !ok {
		return refuse(CodeFolderReparse, "name a folder on a local drive", "folder %s (%s) resolves to %s, on no drive letter", name, p, final)
	}
	if why := documentFolderRefusal(fp, a.folderRules()); why != "" {
		return refuse(CodeFolderForbidden, "name the folder the ERP reads: not a drive's root, a system or profile folder, nor the connector's own",
			"folder %s (%s) resolves to %s: %s", name, p, fp, why)
	}
	if !strings.EqualFold(filepath.Clean(fp), filepath.Clean(p)) {
		return refuse(CodeFolderReparse, "name the folder by its real path (its long name, no junction, link or mount point on the way)",
			"folder %s (%s) resolves to %s", name, p, fp)
	}
	return nil
}

// folderACLIdle refuses a folder with unfinished work in it (as the
// documents command): orders frozen for a typed folder, orders without a rule
// for sendOrderDir and the pickup folder, reserved receipts for the receipt
// folder; and index entries it cannot read unless accepted (review R7).
func (a *applier) folderACLIdle(folders map[string]string) error {
	orders, err := hasavshevet.ScanOrdersInFlight(a.dir)
	if err != nil {
		return refuse(CodeWriteFailed, "run it elevated (as an administrator)", "read the order index: %v", err)
	}
	receipts, rUnreadable, err := hasavshevet.ScanReceiptsInFlight(a.dir)
	if err != nil {
		return refuse(CodeWriteFailed, "run it elevated (as an administrator)", "read the receipt index: %v", err)
	}
	a.noteUnreadable(config.DocumentKindOrders, orders.Unreadable, "an unfinished one among them could be in the folder")
	a.noteUnreadable(config.DocumentKindReceipts, rUnreadable, "an unfinished one among them could be in the folder")
	if r := a.unreadableSettled(); r != nil { // review m3: the command's one total
		return r
	}
	for _, name := range sortedKeys(folders) {
		n := orders.ByFolder[name]
		if name == FolderRoleSendOrderDir || name == FolderRolePickup {
			n = orders.Default
		}
		if rd := a.cur.ReceiptDir(); a.cur.ReceiptsConfigured() && rd != "" && sameFolder(rd, folders[name]) {
			n += receipts
		}
		if n > 0 {
			return refuse(CodeFolderInFlight, "let them finish (a repeat of their key, the next start; docs/troubleshooting.md), then run it again",
				"%d unfinished order(s) or receipt(s) go to folder %s", n, name)
		}
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// lockdownRefusal maps dataacl's errors to refusals.
func lockdownRefusal(name, path string, err error) *Refusal {
	switch {
	case errors.Is(err, dataacl.ErrReparsePoint), errors.Is(err, dataacl.ErrHardLink), errors.Is(err, dataacl.ErrOutsideTree):
		return refuse(CodeFolderReparse, "remove the link, junction or hard link (a lock-down never follows or rewrites one), then run it again",
			"folder %s (%s): %v", name, path, err)
	case errors.Is(err, dataacl.ErrTooManyEntries):
		return refuse(CodeFolderTooLarge, fmt.Sprintf("move what the importer no longer needs (old *.MASOFON2.BAK files) out of it: a lock-down walks at most %d entries", dataacl.MaxLockdownEntries),
			"folder %s (%s): %v", name, path, err)
	}
	return refuse(CodeWriteFailed, "run it elevated (as an administrator)", "folder %s (%s): %v", name, path, err)
}

func (a *applier) folderLockdown(ctx context.Context, in FolderACLInput) error {
	paths := map[string]string{}
	for _, n := range in.Folders {
		p, r := a.resolveFolder(n, true)
		if r != nil {
			return r
		}
		paths[n] = p
	}
	if err := a.folderACLIdle(paths); err != nil {
		return err
	}
	trustedFor := map[string][]string{}
	for _, n := range in.Folders {
		// Review T1: never where the connector runs code from — the importer's
		// files — and no trusted writer where an importer runs.
		for _, f := range a.cur.ImporterFiles() {
			if inside(f, paths[n]) {
				return refuse(CodeFolderHasExecutables, "keep the importer's files (has.exe, its BAT and parameter files) out of the folders documents are written into",
					"folder %s (%s) holds the importer's file %s", n, paths[n], f)
			}
		}
		if !a.cur.ExecutesIn(paths[n]) {
			for _, w := range a.cur.Documents.TrustedWriters {
				trustedFor[n] = append(trustedFor[n], w.SID)
			}
		}
	}
	plans := make([]dataacl.LockdownPlan, len(in.Folders))
	exts := executableExtensions()
	for i, n := range in.Folders {
		plan, err := a.env.Sys.PlanFolderLockdown(paths[n], trustedFor[n], exts)
		if err != nil {
			return lockdownRefusal(n, paths[n], err)
		}
		if err := a.finalPathRefusal(n, paths[n], plan.FinalPath); err != nil { // review T3
			return err
		}
		if len(plan.Executables) > 0 {
			return refuse(CodeFolderHasExecutables, "move the programs, scripts and libraries out of the folder: a lock-down never tightens a folder code is run from",
				"folder %s (%s) holds files Windows runs: %s", n, paths[n], strings.Join(plan.Executables, ", "))
		}
		plans[i] = plan
		out := FolderACLFolder{Name: n, Path: paths[n], Entries: len(plan.Entries), EntriesChanged: plan.EntriesChanged,
			Principals: plan.Principals, Owners: plan.Owners}
		for _, w := range plan.ParentWriters {
			out.ParentWritable = append(out.ParentWritable, w.Name)
		}
		if len(out.ParentWritable) > 0 {
			a.warn("parent_writable: %s (%s) can still be renamed away through a folder above it by %s — parents are never changed; let only the administrators delete or rename the folders above",
				n, paths[n], strings.Join(out.ParentWritable, ", "))
		}
		// Review T2: a lock-down never grants, so a trusted writer keeps only
		// what its own entries give it — write it had through a group (Users)
		// is capped with the group. Said before anything is written.
		for _, w := range a.cur.Documents.TrustedWriters {
			if !slices.Contains(trustedFor[n], w.SID) {
				continue
			}
			after := "none"
			for _, p := range plan.Principals {
				if p.SID == w.SID {
					after = p.After
				}
			}
			if after != "full" && after != "modify" && after != "write" {
				a.warn("trusted_writer_no_write: %s, a trusted writer, has no write of its own in folder %s (%s) after the lock-down (%s): a lock-down never grants, and write it had through a group is capped with the group — if the ERP's scheduler must write there, give it its own entry first",
					w.Name, n, paths[n], after)
			}
		}
		a.res.FolderACL.Folders = append(a.res.FolderACL.Folders, out)
	}
	if a.check {
		a.res.Check = &CheckResult{Config: "unchanged", Restart: a.running}
		return nil
	}
	if a.running {
		if err := a.idle(ctx, "before the lock-down"); err != nil {
			return err
		}
	}
	if err := a.env.Sys.ProtectDataDir(); err != nil {
		return refuse(CodeWriteFailed, `see "The service will not start" in docs/troubleshooting.md`,
			"the data directory could not be protected, so nothing was written: %v", err)
	}
	for i, n := range in.Folders {
		sha, err := a.recordFolderACL(n, paths[n], plans[i].Entries)
		if err != nil {
			return refuse(CodeWriteFailed, "run it elevated (as an administrator)", "record the pre-image of folder %s: %v", n, err)
		}
		a.res.FolderACL.Folders[i].Preimage = sha
		written, err := a.env.Sys.ApplyFolderLockdown(paths[n], trustedFor[n], plans[i])
		a.res.FolderACL.Folders[i].Written = written
		if err != nil {
			r := lockdownRefusal(n, paths[n], err)
			r.Message += fmt.Sprintf(" — the lock-down stopped part way (%d entries written): folder-acl restore with preimage %s puts back what it found", written, sha)
			return r
		}
		a.step("folder-acl", true, fmt.Sprintf("%s locked down (%d entries); restore with preimage %s", n, written, sha))
	}
	return a.restartForFolders(ctx)
}

// executableExtensions are the file extensions a lock-down refuses (review
// T1): PATHEXT's, never fewer than Windows' default PATHEXT; .dll and .ps1;
// and the other program images (.scr, .cpl, .ocx, .sys, .drv). Never .php:
// a PHP site's pages are data to the ERP's folders.
func executableExtensions() map[string]bool {
	out := map[string]bool{".dll": true, ".ps1": true, ".scr": true, ".cpl": true, ".ocx": true, ".sys": true, ".drv": true}
	pathext := ".COM;.EXE;.BAT;.CMD;.VBS;.VBE;.JS;.JSE;.WSF;.WSH;.MSC;" + os.Getenv("PATHEXT")
	for _, e := range strings.Split(pathext, ";") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			if !strings.HasPrefix(e, ".") {
				e = "." + e
			}
			out[e] = true
		}
	}
	delete(out, ".php")
	return out
}

// recordFolderACL writes a lockdown's pre-image under the data directory and
// answers its sha256 (the file's name and its restore's preimage).
func (a *applier) recordFolderACL(name, path string, entries []dataacl.FolderEntry) (string, error) {
	b, err := json.MarshalIndent(folderACLPreimage{Version: 1, Folder: name, Path: path, ChangeSet: a.s.ChangeSet,
		Created: a.env.Now().UTC().Format("2006-01-02T15:04:05Z"), Entries: entries}, "", " ")
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	sha := hex.EncodeToString(sum[:])
	if err := os.MkdirAll(folderACLDir(a.dir), 0o700); err != nil {
		return "", err
	}
	return sha, atomicfile.Write(filepath.Join(folderACLDir(a.dir), sha+".json"), b, 0o600)
}

func (a *applier) folderACLRestore(ctx context.Context, in FolderACLInput) error {
	path, r := a.resolveFolder(in.Folder, false)
	if r != nil {
		return r
	}
	b, err := os.ReadFile(filepath.Join(folderACLDir(a.dir), in.Preimage+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return refuse(CodePreimageMissing, "give a preimage a lockdown of this folder answered on this box", "there is no folder pre-image %s on this box", in.Preimage)
	}
	if err != nil {
		return refuse(CodeWriteFailed, "run it elevated (as an administrator)", "read the folder pre-image: %v", err)
	}
	sum := sha256.Sum256(b)
	var pre folderACLPreimage
	switch {
	case hex.EncodeToString(sum[:]) != in.Preimage:
		return refuse(CodePreimageMissing, "restore by hand from the file", "the folder pre-image %s does not match its sha256: it was changed", in.Preimage)
	case json.Unmarshal(b, &pre) != nil || pre.Version != 1 || len(pre.Entries) == 0:
		return refuse(CodePreimageMissing, "restore by hand from the file", "the folder pre-image %s does not read", in.Preimage)
	case pre.Folder != in.Folder || !sameFolder(pre.Path, path):
		return refuse(CodePreimageMissing, "give the preimage recorded for this folder at its configured path",
			"pre-image %s was recorded for folder %s at %s, not folder %s at %s", in.Preimage, pre.Folder, pre.Path, in.Folder, path)
	}
	if err := a.folderACLIdle(map[string]string{in.Folder: path}); err != nil {
		return err
	}
	out := FolderACLFolder{Name: in.Folder, Path: path, Entries: len(pre.Entries), Preimage: in.Preimage}
	if a.check {
		a.res.FolderACL.Folders = append(a.res.FolderACL.Folders, out)
		a.res.Check = &CheckResult{Config: "unchanged", Restart: a.running}
		return nil
	}
	if a.running {
		if err := a.idle(ctx, "before the restore"); err != nil {
			return err
		}
	}
	restored, skipped, err := a.env.Sys.RestoreFolderACL(path, pre.Entries)
	out.Restored, out.Skipped = restored, skipped
	a.res.FolderACL.Folders = append(a.res.FolderACL.Folders, out)
	if err != nil {
		if errors.Is(err, dataacl.ErrOutsideTree) || errors.Is(err, dataacl.ErrReparsePoint) {
			return refuse(CodeFolderReparse, "the folder at that path is not the one recorded (replaced, or a link now): restore by hand", "folder %s (%s): %v", in.Folder, path, err)
		}
		return refuse(CodeWriteFailed, "run it elevated (as an administrator)", "restore folder %s (%s): %v", in.Folder, path, err)
	}
	if skipped > 0 {
		a.warn("%d entries of folder %s were gone or replaced since the lock-down and were left as they are", skipped, in.Folder)
	}
	a.step("folder-acl", true, fmt.Sprintf("%s restored from preimage %s (%d entries, %d skipped)", in.Folder, in.Preimage, restored, skipped))
	return a.restartForFolders(ctx)
}

// restartForFolders restarts a running service and proves it, so the daemon
// reads the folders' new ACLs at once (it reads them at its start).
func (a *applier) restartForFolders(ctx context.Context) error {
	if !a.running {
		a.step("restart", false, "the service is stopped: it reads the folders when it starts")
		return nil
	}
	if err := a.env.Sys.StopService(ServiceName); err != nil {
		return refuse(CodeServiceFailed, "restart it in services.msc", "the ACLs were written, but the service did not stop: %v", err)
	}
	a.touched = true
	if err := a.env.Sys.StartService(ServiceName); err != nil {
		return refuse(CodeServiceFailed, "read server.log (the logs command) for why it stopped", "the ACLs were written, but the service did not start: %v", err)
	}
	a.res.Restarted = true
	a.step("restart", true, "")
	return a.proveAfter(ctx)
}
