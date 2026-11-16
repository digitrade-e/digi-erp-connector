package dataacl

import "errors"

// Plan 087 review R1c, T2: locking down a folder the connector writes
// documents into, so digi-mcp can do it through a signed command instead of
// a human running icacls on the box.
//
// A lock-down never grants anything, to anyone, on any entry (T2): on the
// folder and on every entry beneath, walked by handle up to
// MaxLockdownEntries, it masks the DACL the entry already has (MaskDACL) —
// SYSTEM, Administrators and TrustedInstaller unchanged, a trusted writer
// capped at Modify, anyone else at read & execute, generic rights mapped
// first, an entry left with nothing dropped, deny entries and every entry's
// type, condition and flags kept — and adds no entry, so a trusted writer
// with no right before keeps none. The folder's link to its parent is cut:
// its DACL is protected, its inherited entries kept as explicit (masked the
// same way); every other entry keeps its own protection. An owner that is not
// trusted (SYSTEM, an administrator, TrustedInstaller) is handed to
// Administrators. A folder holding more entries, or any reparse point or
// multiply-linked file beneath, is refused before anything is written —
// never followed, never rewritten through. No path-based propagation (MARTA)
// is used: as LocalSystem a path can be redirected. Folders above are never
// changed.

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

// PrincipalRights is one principal with an allow entry anywhere in the tree
// (review T4): the strongest rights it has anywhere before and after the
// lock-down ("full", "modify", "write", "read", "special", "none") and on how
// many entries they change — so the approval form shows exactly who loses
// write, beneath the folder too.
type PrincipalRights struct {
	SID     string `json:"sid"`
	Name    string `json:"name"`
	Before  string `json:"before"`
	After   string `json:"after"`
	Entries int    `json:"entries"`
}

// OwnerChange is an owner a lock-down hands to Administrators, and on how
// many entries.
type OwnerChange struct {
	SID     string `json:"sid"`
	Name    string `json:"name"`
	Entries int    `json:"entries"`
}

// LockdownPlan is what a lock-down of a folder would do, read and nothing
// written: the security of the folder and of every entry beneath (the
// pre-image a restore puts back), every principal's rights before and after,
// the owners handed over, how many entries change, the files whose extension
// is executable (review T1), the folder's final path as Windows resolves it
// (review T3), and the accounts that can still rename the folder away
// through a folder above (parents are never changed).
type LockdownPlan struct {
	Entries        []FolderEntry     `json:"entries"`
	Principals     []PrincipalRights `json:"principals"`
	Owners         []OwnerChange     `json:"owners"`
	EntriesChanged int               `json:"entriesChanged"`
	Executables    []string          `json:"executables"`
	FinalPath      string            `json:"finalPath"`
	ParentWriters  []Writer          `json:"parentWriters"`
}
