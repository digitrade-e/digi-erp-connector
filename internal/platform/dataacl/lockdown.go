package dataacl

import "errors"

// Plan 087 review R1c: locking down a folder the connector writes documents
// into, so digi-mcp can do it through a signed command instead of a human
// running icacls on the box.
//
// The folder gets a protected DACL — SYSTEM and Administrators Full Control,
// each trusted writer Modify, every other principal that had an allow entry
// read & execute only, each inherited by files and folders (OI)(CI); its deny
// entries are kept (they only take rights away) — and Administrators as its
// owner. Propagation (the bound chosen): every entry beneath, walked by
// handle up to MaxLockdownEntries, is made to inherit exactly that — its own
// explicit entries dropped, its owner handed to Administrators when it is
// not a trusted one — so an existing subfolder (history\, receipts\) or a
// planted file cannot keep its old rights. A folder holding more entries is
// refused before anything is written, as is any reparse point or
// multiply-linked file beneath it: never followed, never rewritten through.
// No path-based propagation (MARTA) is used: as LocalSystem a path can be
// redirected. Folders above are never changed.

// MaxLockdownEntries bounds a lock-down: the folder and every entry beneath.
const MaxLockdownEntries = 20000

// ErrTooManyEntries is a folder holding more than MaxLockdownEntries entries.
var ErrTooManyEntries = errors.New("dataacl: the folder holds more entries than a lock-down walks")

// FolderEntry is one entry's security as a lock-down found it: its path
// relative to the folder ("" for the folder itself), its file ID (volume
// serial and file index: a restore touches only the same object), and its
// owner and DACL as SDDL ("O:…D:…", the DACL's P/AI flags included).
type FolderEntry struct {
	Rel    string `json:"rel"`
	FileID string `json:"fileId"`
	SDDL   string `json:"sddl"`
}

// PrincipalRights is one principal of the folder's own DACL before and after
// a lock-down: "full", "modify", "write", "read", "special" or "none", with
// "deny …" appended where a deny entry names it.
type PrincipalRights struct {
	SID    string `json:"sid"`
	Name   string `json:"name"`
	Before string `json:"before"`
	After  string `json:"after"`
}

// LockdownPlan is what a lock-down of a folder would do, read and nothing
// written: the security of the folder and of every entry beneath (the
// pre-image a restore puts back), the folder's principals before and after,
// the accounts that can still rename the folder away through a folder above
// (they keep that right: parents are never changed), and the new DACLs.
type LockdownPlan struct {
	Entries       []FolderEntry     `json:"entries"`
	Principals    []PrincipalRights `json:"principals"`
	ParentWriters []Writer          `json:"parentWriters"`
	FolderDACL    string            `json:"folderDacl"`
	FileDACL      string            `json:"fileDacl"`
	DirDACL       string            `json:"dirDacl"`
}
