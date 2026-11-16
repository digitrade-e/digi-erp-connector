package provision

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
)

// Plan 087 (DIG-1563): the documents command — the typed folders a
// documentRule names (config.yaml's documents.folders) and the receipts'
// folder and counter (documents.receipts). Signed, run by digi-mcp over the
// link; config.yaml only, so it is the flow of every config command
// (runConfigChange): every check before the first write, the change set's
// pre-image of config.yaml (restore puts it back), the order queue asked, the
// restart and the proof. Not sealed: it carries no secret.

// DocumentsInput is the documents command's stdin. Each folder named is set
// whole (an object) or removed (null); folders not named are kept.
type DocumentsInput struct {
	Version   int                     `json:"version"` // 1
	ChangeSet string                  `json:"changeSet"`
	Folders   map[string]*FolderInput `json:"folders,omitempty"`
	// Receipts binds POST /api/sendReceipt to a receipts folder and a
	// receipts counter; a field left out keeps the box's, "" clears it.
	Receipts   *DocumentsReceiptsInput `json:"receipts,omitempty"`
	AssumeIdle bool                    `json:"assumeIdle,omitempty"`
	Expect     *Expect                 `json:"expect,omitempty"`
}

// FolderInput is one typed folder (config.FolderConfig).
type FolderInput struct {
	Kind        string `json:"kind"`                 // orders | receipts
	Path        string `json:"path"`                 // absolute; the folder must exist
	NamePattern string `json:"namePattern"`          // {prefix}<stem>{n}.doc, {prefix} optional
	TempPrefix  string `json:"tempPrefix,omitempty"` // orders, with {prefix}
	Layout      string `json:"layout,omitempty"`     // orders: official | legacy
	Test        bool   `json:"test,omitempty"`
}

// DocumentsReceiptsInput is documents.receipts.
type DocumentsReceiptsInput struct {
	Folder  *string `json:"folder,omitempty"`
	Counter *string `json:"counter,omitempty"`
}

// DecodeDocuments reads one JSON input, strictly like DecodeSpec.
func DecodeDocuments(r io.Reader) (DocumentsInput, *Refusal) {
	var in DocumentsInput
	if err := decodeStrict(r, &in); err != nil {
		return DocumentsInput{}, refuse(CodeSpecInvalid, "send one JSON object with only the documented fields", "the documents input does not decode: %v", err)
	}
	return in, nil
}

// Validate checks the input before anything is read. Behind the link a path
// is one link mode takes (a plain path on a local drive, no "." or "..", none
// of cmd.exe's characters) outside the system folders.
func (in DocumentsInput) Validate(linked bool, systemDirs []string) *Refusal {
	if r := validateHeader(in.Version, in.ChangeSet, in.Expect); r != nil {
		return r
	}
	if len(in.Folders) == 0 && in.Receipts == nil {
		return refuse(CodeSpecInvalid, "name at least one folder, or the receipts binding", "the input changes nothing")
	}
	for _, k := range sortedFolderKeys(in.Folders) {
		f := in.Folders[k]
		if !config.ValidDocumentKey(k) {
			return refuse(CodeSpecInvalid, "use a lower-case letter, then letters, digits or _ (32 at most)", "folder key %q is not a key", k)
		}
		if f == nil {
			continue
		}
		p := f.Path
		if strings.TrimSpace(p) == "" {
			return refuse(CodeSpecInvalid, "give the folder's absolute path", "folder %s has no path", k)
		}
		if r := absOrEmpty("folder "+k+" path", &p, linked); r != nil {
			return r
		}
		if linked && winUnder(p, systemDirs) {
			return refuse(CodeFolderForbidden, "name the folder the ERP's importer reads, outside the system folders", "folder %s (%s) is in a system folder", k, strings.TrimSpace(p))
		}
		if _, _, err := config.ParseNamePattern(f.NamePattern, f.TempPrefix); err != nil {
			return refuse(CodeSpecInvalid, "use {prefix}<stem>{n}.doc: ASCII letters, digits and _, {prefix} optional, .doc only", "folder %s: %v", k, err)
		}
	}
	if b := in.Receipts; b != nil {
		for name, v := range map[string]*string{"folder": b.Folder, "counter": b.Counter} {
			if v != nil && *v != "" && !config.ValidDocumentKey(*v) {
				return refuse(CodeSpecInvalid, "name a key, or \"\" to clear it", "receipts.%s %q is not a key", name, *v)
			}
		}
	}
	return nil
}

func sortedFolderKeys(m map[string]*FolderInput) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ApplyDocuments sets the typed folders and the receipts binding of
// config.yaml and nothing else.
func ApplyDocuments(ctx context.Context, env Env, in DocumentsInput, check bool) (*ApplyResult, error) {
	res := &ApplyResult{ChangeSet: in.ChangeSet, Steps: []Step{}}
	if r := in.Validate(LinkMode(env.Sys.DataDir()), SystemDirs()); r != nil {
		return res, r
	}
	a := &applier{env: env, s: Spec{Version: in.Version, ChangeSet: in.ChangeSet, AssumeIdle: in.AssumeIdle, Expect: in.Expect},
		dir: env.Sys.DataDir(), res: res, check: check}
	unlock, err := lockRuns(a.dir)
	if err != nil {
		return res, err
	}
	defer unlock()
	set := func(t *config.Config) {
		t.Documents = t.Documents.Clone()
		for k, f := range in.Folders {
			if f == nil {
				delete(t.Documents.Folders, k)
				continue
			}
			if t.Documents.Folders == nil {
				t.Documents.Folders = map[string]config.FolderConfig{}
			}
			t.Documents.Folders[k] = config.FolderConfig{Kind: f.Kind, Path: strings.TrimSpace(f.Path), NamePattern: f.NamePattern,
				TempPrefix: f.TempPrefix, Layout: f.Layout, Test: f.Test}
		}
		if len(t.Documents.Folders) == 0 {
			t.Documents.Folders = nil
		}
		if b := in.Receipts; b != nil {
			setTrimmed(&t.Documents.Receipts.Folder, b.Folder)
			setTrimmed(&t.Documents.Receipts.Counter, b.Counter)
		}
	}
	err = a.runConfigChange(ctx, set, func() error { return a.checkDocuments(in) })
	a.settle()
	return res, err
}

// checkDocuments: each folder set is a folder on this machine, not a link or
// junction, outside the data directory; a folder removed exists and holds no
// unfinished order frozen for it; the receipts' folder or counter changes
// only while no receipt is reserved (Recover finishes one into the folder in
// use).
func (a *applier) checkDocuments(in DocumentsInput) error {
	for _, k := range sortedFolderKeys(in.Folders) {
		f := in.Folders[k]
		if f == nil {
			if _, ok := a.cur.Documents.Folders[k]; !ok {
				return refuse(CodeFolderUnknown, "name a configured folder to remove", "there is no folder %s", k)
			}
			if r := a.folderIdle(k, "removed"); r != nil {
				return r
			}
			continue
		}
		p := strings.TrimSpace(f.Path)
		fi, err := os.Lstat(p)
		switch {
		case err != nil:
			return refuse(CodeFolderMissing, "create the folder the ERP's importer reads (the connector never creates it)", "folder %s: %s is not a folder on this machine", k, p)
		case fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0:
			return refuse(CodeFolderForbidden, "name the folder itself, not a link or junction to it", "folder %s: %s is a link or a junction", k, p)
		case !fi.IsDir():
			return refuse(CodeFolderMissing, "name a folder", "folder %s: %s is not a folder", k, p)
		}
		if inside(p, a.dir) {
			return refuse(CodeFolderForbidden, "name the folder the ERP's importer reads, outside the connector's data directory", "folder %s: %s is in the data directory", k, p)
		}
		if old, ok := a.cur.Documents.Folders[k]; ok && (old.Kind != f.Kind || !sameFolder(old.Path, p)) {
			if r := a.folderIdle(k, "moved or retyped"); r != nil {
				return r
			}
		}
	}
	if a.cur.ReceiptDir() != a.tgt.ReceiptDir() || a.cur.ReceiptCounterKey() != a.tgt.ReceiptCounterKey() {
		n, err := hasavshevet.ReceiptsInFlight(a.dir)
		if err != nil {
			return refuse(CodeWriteFailed, "run it elevated (as an administrator)", "read the receipt index: %v", err)
		}
		if n > 0 {
			return refuse(CodeReceiptsInFlight, "let the reserved receipts finish (a repeat of their key, or the next start), then run it again",
				"%d receipt(s) are reserved: they would be finished into the new folder or under the new counter", n)
		}
	}
	return nil
}

// folderIdle refuses when unfinished orders (reserved, or waiting for a
// resend) are frozen for folder k.
func (a *applier) folderIdle(k, what string) *Refusal {
	n, err := hasavshevet.OrdersInFlightForFolder(a.dir, k)
	if err != nil {
		return refuse(CodeWriteFailed, "run it elevated (as an administrator)", "read the order index: %v", err)
	}
	if n > 0 {
		return refuse(CodeFolderInFlight, "let them be written (or resolve them, docs/troubleshooting.md), then run it again",
			"%d order(s) frozen for folder %s are not finished: a resend would write into the folder %s", n, k, what)
	}
	return nil
}

// inside reports whether p is dir or lies under it, compared as the file
// system does (sameFolder).
func inside(p, dir string) bool {
	for cur := filepath.Clean(p); ; {
		if sameFolder(cur, dir) {
			return true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return false
		}
		cur = parent
	}
}
