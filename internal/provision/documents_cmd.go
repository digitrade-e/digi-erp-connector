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
	Receipts *DocumentsReceiptsInput `json:"receipts,omitempty"`
	// AllowWritable accepts, over WinRM only, a folder set or bound that
	// accounts besides the administrators can write to (review M1): a risk a
	// human was shown. Behind the link it is refused.
	AllowWritable bool `json:"allowWritable,omitempty"`
	// AcceptUnreadable is the number of unreadable order and receipt index
	// entries a human checked (review R7): without it, or with another count,
	// a change the in-flight guard must scan for refuses (index_unreadable).
	AcceptUnreadable *int    `json:"acceptUnreadable,omitempty"`
	AssumeIdle       bool    `json:"assumeIdle,omitempty"`
	Expect           *Expect `json:"expect,omitempty"`
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
	if linked {
		if r := refuseAllowWritableLinked(in.AllowWritable); r != nil {
			return r
		}
	}
	if r := validAcceptUnreadable(in.AcceptUnreadable); r != nil {
		return r
	}
	for _, k := range sortedFolderKeys(in.Folders) {
		f := in.Folders[k]
		if !config.ValidDocumentKey(k) {
			return refuse(CodeSpecInvalid, config.DocumentKeyGrammar, "folder key %q is not a key", k)
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
// junction, outside the data directory, that only the administrators can
// write to (review M1); a folder removed exists; and no folder that
// unfinished work uses, nor the receipt binding, changes under it
// (documentsIdle).
func (a *applier) checkDocuments(in DocumentsInput) error {
	checked := map[string]bool{}
	for _, k := range sortedFolderKeys(in.Folders) {
		f := in.Folders[k]
		if f == nil {
			if _, ok := a.cur.Documents.Folders[k]; !ok {
				return refuse(CodeFolderUnknown, "name a configured folder to remove", "there is no folder %s", k)
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
		if err := a.checkFolderWriters("folder "+k, p, in.AllowWritable); err != nil {
			return err
		}
		checked[k] = true
	}
	// The receipt folder a binding names (it may have been set earlier).
	if k := strings.TrimSpace(a.tgt.Documents.Receipts.Folder); in.Receipts != nil && k != "" && !checked[k] && a.tgt.ReceiptDir() != "" {
		if err := a.checkFolderWriters("folder "+k, a.tgt.ReceiptDir(), in.AllowWritable); err != nil {
			return err
		}
	}
	return a.documentsIdle(a.cur, a.tgt, in.AcceptUnreadable)
}

// documentsIdle refuses changing, from cur to tgt, what unfinished work
// depends on (review m5): any attribute of a folder with unfinished orders
// frozen for it — its path (where the file goes), kind, name pattern and
// temporary prefix (the "number taken" scan), layout (the record), test flag
// (whether production may write there) — and the receipt binding — the
// receipt folder, its name pattern and test flag, the receipt counter —
// while a receipt is reserved: Recover or a repeat finishes it into the
// folder in use, under its pattern (a reserved receipt whose file was written
// under the old name would be written again under the new one: two copies a
// kupain*.doc importer takes both of). Restore runs it too (review m9). An
// index entry it cannot read refuses unless accept is their number (review
// R7: an unfinished one among them would be stranded unseen).
func (a *applier) documentsIdle(cur, tgt config.Config, accept *int) error {
	var changed []string
	for _, k := range cur.FolderKeys() {
		if nf, ok := tgt.Documents.Folders[k]; !ok || !sameFolderConfig(cur.Documents.Folders[k], nf) {
			changed = append(changed, k)
		}
	}
	binding := !sameReceiptBinding(cur, tgt)
	if len(changed) == 0 && !binding {
		return nil
	}
	var orders hasavshevet.OrdersInFlight
	var receipts, unreadable int
	if len(changed) > 0 {
		var err error
		if orders, err = hasavshevet.ScanOrdersInFlight(a.dir); err != nil {
			return refuse(CodeWriteFailed, "run it elevated (as an administrator)", "read the order index: %v", err)
		}
		unreadable += orders.Unreadable
	}
	if binding {
		var n int
		var err error
		if receipts, n, err = hasavshevet.ScanReceiptsInFlight(a.dir); err != nil {
			return refuse(CodeWriteFailed, "run it elevated (as an administrator)", "read the receipt index: %v", err)
		}
		unreadable += n
	}
	if r := a.unreadableAccepted(unreadable, "order or receipt", "an unfinished one among them would be stranded unseen by the change", accept); r != nil {
		return r
	}
	for _, k := range changed {
		what := "removed"
		if _, ok := tgt.Documents.Folders[k]; ok {
			what = "changed (path, kind, name pattern, temporary prefix, layout or test)"
		}
		if n := orders.ByFolder[k]; n > 0 {
			return refuse(CodeFolderInFlight, "let them be written (or resolve them, docs/troubleshooting.md), then run it again",
				"%d order(s) frozen for folder %s are not finished, and the folder would be %s under them (a resend writes by it)", n, k, what)
		}
	}
	if binding && receipts > 0 {
		return refuse(CodeReceiptsInFlight, "let the reserved receipts finish (a repeat of their key, or the next start), then run it again",
			"%d receipt(s) are reserved: they would be finished into another folder, under another name or another counter", receipts)
	}
	return nil
}

// sameFolderConfig reports whether two typed folders write the same file the
// same way.
func sameFolderConfig(a, b config.FolderConfig) bool {
	return a.Kind == b.Kind && a.NamePattern == b.NamePattern && a.TempPrefix == b.TempPrefix && a.Layout == b.Layout &&
		a.Test == b.Test && sameFolder(strings.TrimSpace(a.Path), strings.TrimSpace(b.Path))
}

// sameReceiptBinding reports whether receipts go to the same folder, under
// the same name pattern and test flag, from the same counter.
func sameReceiptBinding(a, b config.Config) bool {
	test := func(c config.Config) bool {
		f, ok := c.Documents.Folders[strings.TrimSpace(c.Documents.Receipts.Folder)]
		return ok && f.Test
	}
	da, db := a.ReceiptDir(), b.ReceiptDir()
	return (da == db || (da != "" && db != "" && sameFolder(da, db))) && a.ReceiptNamePattern() == b.ReceiptNamePattern() &&
		a.ReceiptCounterKey() == b.ReceiptCounterKey() && test(a) == test(b)
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
