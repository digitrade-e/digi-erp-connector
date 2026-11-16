package hasavshevet

import (
	"errors"
	"fmt"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// Plan 087 (DIG-1563): the documentRule an order carries on
// POST /api/v2/sendOrder — keys only, resolved against the box's documents
// block (config.yaml) — and the rule as frozen in the order's index entry when
// its number is reserved.

// DocumentRule is a request's rule, already checked by the route against the
// box: the folder and the counter exist, both are for orders, a test counter
// writes to a test folder, and temporary is used only where the folder has a
// temporary prefix.
type DocumentRule struct {
	Counter   string `json:"counter"`
	Folder    string `json:"folder"`
	Temporary bool   `json:"temporary,omitempty"`
}

// storedRule is the rule kept in the order's index entry, frozen when its
// number was reserved: every re-queue and recovery uses it, never the
// repeat's, and a repeat naming another rule is refused (409).
type storedRule struct {
	Counter   string `json:"counter"`
	Folder    string `json:"folder"`
	Temporary bool   `json:"temporary"`
	// FileName is the name resolved from the folder's pattern at reservation.
	FileName string `json:"fileName"`
	// CounterTest and FolderTest are the counter's and the folder's test
	// flags at reservation (review R2): a re-queue or a write finds the
	// folder's flag changed since — a test order would land in what is now a
	// production folder, or the reverse — and writes nothing
	// (ErrFolderTestChanged; the order stays retryable).
	CounterTest bool `json:"counterTest"`
	FolderTest  bool `json:"folderTest"`
}

// AppliedRule is what the 202 echoes for an order with a rule.
type AppliedRule struct {
	Counter  string
	Number   int64
	FileName string
	Folder   string
}

// orderDestination is where the worker writes an order with a rule: the
// folder by key (its path and layout read from the config when it writes),
// under the frozen file name, with the test flags frozen beside it.
type orderDestination struct {
	Folder                  string
	FileName                string
	CounterTest, FolderTest bool
}

// ErrFolderTestChanged means the folder's test flag is not the one frozen in
// the order's rule at reservation (review R2: only a hand edit or an
// unreadable in-flight entry gets past the documents command's and restore's
// guards). Nothing is written; the order stays retryable, and a resend writes
// it once the flag is back. The routes answer 503 FOLDER_TEST_CHANGED.
var ErrFolderTestChanged = errors.New("FOLDER_TEST_CHANGED")

// testFlagChanged is why folder f (now) no longer matches the flags frozen
// in the rule, or "".
func testFlagChanged(f config.FolderConfig, counterTest, folderTest bool) string {
	if f.Test == folderTest && f.Test == counterTest {
		return ""
	}
	return fmt.Sprintf("the folder's test flag is %v, and the order was reserved with folder test %v, counter test %v", f.Test, folderTest, counterTest)
}

// same reports whether a repeat's rule is the one frozen in the entry (a nil
// stored rule matches only a request without one).
func (r *storedRule) same(req *DocumentRule) bool {
	if r == nil || req == nil {
		return r == nil && req == nil
	}
	return r.Counter == req.Counter && r.Folder == req.Folder && r.Temporary == req.Temporary
}

func (r *storedRule) describe() string {
	if r == nil {
		return "no documentRule"
	}
	return describeRule(&DocumentRule{Counter: r.Counter, Folder: r.Folder, Temporary: r.Temporary})
}

func describeRule(r *DocumentRule) string {
	if r == nil {
		return "no documentRule"
	}
	return fmt.Sprintf("documentRule {counter %s, folder %s, temporary %v}", r.Counter, r.Folder, r.Temporary)
}

func (r *storedRule) applied(n int64) *AppliedRule {
	if r == nil {
		return nil
	}
	return &AppliedRule{Counter: r.Counter, Number: n, FileName: r.FileName, Folder: r.Folder}
}

func (r *storedRule) destination() *orderDestination {
	if r == nil {
		return nil
	}
	return &orderDestination{Folder: r.Folder, FileName: r.FileName, CounterTest: r.CounterTest, FolderTest: r.FolderTest}
}

// validate checks a stored rule read from the index: keys in the key grammar
// and a plain file name (no separator, no "..", .doc), so a damaged or edited
// entry can never send an order out of its folder.
func (r *storedRule) validate() error {
	if r == nil {
		return nil
	}
	if !config.ValidDocumentKey(r.Counter) || !config.ValidDocumentKey(r.Folder) {
		return errors.New("rule names a counter or folder that is not a key")
	}
	if !plainDocName(r.FileName) {
		return fmt.Errorf("rule file name %q is not a plain .doc name", r.FileName)
	}
	return nil
}

// plainDocName: ASCII letters, digits and _ then .doc — what the closed name
// grammar produces, and nothing a path could hide in.
func plainDocName(name string) bool {
	stem, ok := strings.CutSuffix(name, ".doc")
	if !ok || stem == "" || len(name) > 96 {
		return false
	}
	for i := 0; i < len(stem); i++ {
		c := stem[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// ResolveRule checks a request's documentRule against cfg's documents block,
// before anything is reserved. It returns a *FieldError naming
// "documentRule/<key>": an unknown folder or counter, one of the wrong kind, a
// test counter writing to a folder that is not a test folder or a production
// counter writing to a test folder (review m10), or temporary on a folder
// with no temporary prefix. A retired counter is the queue's to
// refuse (only for a new number: a repeat of an order it numbered is still
// answered).
func ResolveRule(cfg config.Config, r DocumentRule) error {
	f, ok := cfg.Documents.Folders[r.Folder]
	switch {
	case !ok:
		return &FieldError{Field: "documentRule/folder", Reason: "is not a folder configured on this connector"}
	case f.Kind != config.DocumentKindOrders:
		return &FieldError{Field: "documentRule/folder", Reason: "is a folder for " + f.Kind + ", not orders"}
	}
	set := NewCounterSet(cfg, "")
	c, ok := set.Get(r.Counter)
	switch {
	case !ok:
		return &FieldError{Field: "documentRule/counter", Reason: "is not a counter configured on this connector"}
	case c.Kind != config.DocumentKindOrders:
		return &FieldError{Field: "documentRule/counter", Reason: "is a counter for " + c.Kind + ", not orders"}
	case c.Test && !f.Test:
		return &FieldError{Field: "documentRule/folder", Reason: "is not a test folder, and the counter is a test counter: test documents never go to a production folder"}
	case !c.Test && f.Test:
		// Review m10: nor a production number into a test folder — a real
		// order the books never import, and a gap in the production series.
		return &FieldError{Field: "documentRule/counter", Reason: "is a production counter, and the folder is a test folder: production numbers never go to a test folder"}
	}
	if r.Temporary && f.TempPrefix == "" {
		return &FieldError{Field: "documentRule/temporary", Reason: "the folder has no temporary prefix ({prefix} in its name pattern)"}
	}
	return nil
}
