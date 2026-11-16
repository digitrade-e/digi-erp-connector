package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// Plan 087 (DIG-1563): Hashavshevet document rules and named counters.
//
// The documents block of config.yaml says what the keys a documentRule names
// mean on this box: typed folders (where a document is written and under
// which name) and counters (which numbers it gets). The rule itself travels
// with each order (POST /api/v2/sendOrder) and carries keys only — never a
// path or a file name — so whoever can edit an erp-manager mapping cannot make
// the connector, which runs as LocalSystem, write anywhere else.
//
// Empty, every install writes exactly as before (two implicit counters, the
// order counter in sendOrderDir and the receipt store in the data directory;
// no typed folder). The block is written only by the provision commands
// counters and documents (digi-mcp, signed, with a pre-image), never by the
// data path.

// The kinds of counters and folders.
const (
	DocumentKindOrders   = "orders"
	DocumentKindReceipts = "receipts"
)

// The two stores that existed before plan 087, which a counter may adopt in
// place (store:). Nothing about them moves: the order counter stays in
// sendOrderDir in the legacy Node format, the receipt store in the data
// directory with its "next" number.
const (
	CounterStoreLastOrderNumber = "lastOrderNumber" // sendOrderDir\lastOrderNumber.json
	CounterStoreReceiptNumber   = "receiptNumber"   // DataDir\receipts\receiptNumber.json
)

// The implicit keys of the two stores while no counter is configured.
const (
	ImplicitOrderCounterKey   = "orders"
	ImplicitReceiptCounterKey = "receipts"
)

// MaxDocumentNumber is the largest number a counter may issue: the nine
// digits of IMOVEIN's MyID (line3) and of KUPAIN's F3. A tenth digit would be
// cut silently in the order's fixed-width record.
const MaxDocumentNumber = 999_999_999

// DefaultReceiptNamePattern is the receipt file's name when no typed receipt
// folder is bound: KUPAIN_<n>.doc, as before plan 087.
const DefaultReceiptNamePattern = "KUPAIN_{n}.doc"

// DocumentsConfig is config.yaml's documents block.
type DocumentsConfig struct {
	// Counters by key. Once one is configured every counter the box uses
	// must be: the order counter of orders without a documentRule is the one
	// with store lastOrderNumber, receipts use Receipts.Counter (or the one
	// with store receiptNumber).
	Counters map[string]CounterConfig `yaml:"counters,omitempty"`
	// Folders by key: where a document of the folder's kind is written.
	Folders map[string]FolderConfig `yaml:"folders,omitempty"`
	// Receipts binds POST /api/sendReceipt to a typed folder and a counter:
	// the receipt body is the backend's frozen contract and carries no rule.
	Receipts DocumentReceiptsConfig `yaml:"receipts,omitempty"`
}

// CounterConfig is one named counter. Its last issued number lives in its
// store file, never here: config.yaml says what the counter is, the store
// what it has issued.
type CounterConfig struct {
	Kind string `yaml:"kind"` // orders | receipts
	// Min and Max bound the numbers it issues, 1 ≤ Min ≤ Max ≤ 999 999 999.
	// Ranges on one box never overlap (test and production, orders and
	// receipts alike): an order's job id, its status, its history folder and
	// the re-queue check are keyed by its number alone.
	Min int64 `yaml:"min"`
	Max int64 `yaml:"max"`
	// Test marks a test counter: its documents may only go to a test folder.
	Test bool `yaml:"test,omitempty"`
	// Retired: it issues no new number, and its range stays reserved so no
	// other counter takes those numbers.
	Retired bool `yaml:"retired,omitempty"`
	// Store adopts one of the two stores that predate counters (in place);
	// empty is a store of its own, DataDir\counters\<key>.json.
	Store string `yaml:"store,omitempty"`
}

// FolderConfig is one typed folder.
type FolderConfig struct {
	Kind string `yaml:"kind"` // orders | receipts
	// Path is the folder the ERP's importer reads; absolute. The connector
	// never creates it.
	Path string `yaml:"path"`
	// NamePattern is the file name, in a closed grammar: an optional
	// "{prefix}", a stem of ASCII letters, digits and "_" starting with a
	// letter, "{n}" (the number in plain decimal), ".doc". The importer's
	// profile decides it (WizSched reads imovein_<n>.doc).
	NamePattern string `yaml:"namePattern"`
	// TempPrefix is what "{prefix}" becomes for a temporary document
	// (documentRule temporary: true), e.g. TMP_; orders only, and exactly
	// when NamePattern holds "{prefix}".
	TempPrefix string `yaml:"tempPrefix,omitempty"`
	// Layout is the IMOVEIN layout the folder's importer reads: official or
	// legacy (orders; required). Receipts have one layout.
	Layout string `yaml:"layout,omitempty"`
	// Test marks a test folder (one the production books do not import).
	Test bool `yaml:"test,omitempty"`
}

// DocumentReceiptsConfig binds receipts to a typed folder and a counter.
type DocumentReceiptsConfig struct {
	Folder  string `yaml:"folder,omitempty"`
	Counter string `yaml:"counter,omitempty"`
}

// CountersConfigured reports whether the box has named counters (plan 087):
// then every counter in use is configured, ranges are enforced and a missing
// order counter file is refused instead of starting at 1.
func (c Config) CountersConfigured() bool { return len(c.Documents.Counters) > 0 }

// DefaultOrderCounterKey is the key of the counter that numbers an order
// without a documentRule: the configured one with store lastOrderNumber, or
// ImplicitOrderCounterKey while no counter is configured; "" when counters
// are configured and none adopts lastOrderNumber.json (ValidateDocuments
// refuses that when orders are configured).
func (c Config) DefaultOrderCounterKey() string {
	if !c.CountersConfigured() {
		return ImplicitOrderCounterKey
	}
	for _, k := range c.CounterKeys() {
		if c.Documents.Counters[k].Store == CounterStoreLastOrderNumber {
			return k
		}
	}
	return ""
}

// ReceiptCounterKey is the key of the counter receipts are numbered from:
// documents.receipts.counter, else the configured one with store
// receiptNumber, else ImplicitReceiptCounterKey while no counter is
// configured.
func (c Config) ReceiptCounterKey() string {
	if k := strings.TrimSpace(c.Documents.Receipts.Counter); k != "" {
		return k
	}
	if !c.CountersConfigured() {
		return ImplicitReceiptCounterKey
	}
	for _, k := range c.CounterKeys() {
		if c.Documents.Counters[k].Store == CounterStoreReceiptNumber {
			return k
		}
	}
	return ""
}

// ReceiptDir is the folder receipts are written to: the bound typed folder's
// path (documents.receipts.folder), else sendReceiptDir. A binding to a
// folder that is not a configured receipts folder is "" (ValidateDocuments
// refuses it).
func (c Config) ReceiptDir() string {
	if k := strings.TrimSpace(c.Documents.Receipts.Folder); k != "" {
		if f, ok := c.Documents.Folders[k]; ok && f.Kind == DocumentKindReceipts {
			return strings.TrimSpace(f.Path)
		}
		return ""
	}
	return strings.TrimSpace(c.SendReceiptDir)
}

// ReceiptNamePattern is the receipt file's name pattern: the bound folder's,
// else DefaultReceiptNamePattern.
func (c Config) ReceiptNamePattern() string {
	if k := strings.TrimSpace(c.Documents.Receipts.Folder); k != "" {
		if f, ok := c.Documents.Folders[k]; ok {
			return f.NamePattern
		}
	}
	return DefaultReceiptNamePattern
}

// CounterKeys and FolderKeys are the configured keys, sorted.
func (c Config) CounterKeys() []string { return sortedKeys(c.Documents.Counters) }
func (c Config) FolderKeys() []string  { return sortedKeys(c.Documents.Folders) }

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// CloneDocuments is a deep copy of the documents block: Config is copied by
// value, and its maps would otherwise be shared between the copies.
func (d DocumentsConfig) Clone() DocumentsConfig {
	out := DocumentsConfig{Receipts: d.Receipts}
	if d.Counters != nil {
		out.Counters = make(map[string]CounterConfig, len(d.Counters))
		for k, v := range d.Counters {
			out.Counters[k] = v
		}
	}
	if d.Folders != nil {
		out.Folders = make(map[string]FolderConfig, len(d.Folders))
		for k, v := range d.Folders {
			out.Folders[k] = v
		}
	}
	return out
}

// documentKeyRe is the grammar of a counter or folder key: a letter, then
// letters, digits and "_", 32 at most, lower case. Never a "/": a key that
// reads as a path ("herp/receipts") is exactly what the rule must not carry.
var documentKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// ValidDocumentKey reports whether k is a counter or folder key.
func ValidDocumentKey(k string) bool { return documentKeyRe.MatchString(k) }

// namePatternRe is the closed grammar of a folder's file name.
var namePatternRe = regexp.MustCompile(`^(\{prefix\})?([A-Za-z][A-Za-z0-9_]{0,31})\{n\}\.doc$`)

// tempPrefixRe is the grammar of a folder's temporary prefix.
var tempPrefixRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,15}$`)

// ParseNamePattern splits a folder's name pattern into whether it takes a
// prefix and its stem, refusing anything outside the closed grammar: ASCII
// only, no path separator or "..", ".doc" only, "{n}" exactly once and last
// before ".doc", "{prefix}" only first. tempPrefix is the folder's
// TempPrefix: a stem that, with or without it, would make a Windows device
// name (COM1.doc, LPT1.doc) is refused.
func ParseNamePattern(pattern, tempPrefix string) (hasPrefix bool, stem string, err error) {
	m := namePatternRe.FindStringSubmatch(pattern)
	if m == nil {
		return false, "", fmt.Errorf("name pattern %q is not {prefix}<stem>{n}.doc: an optional {prefix}, a stem of ASCII letters, digits and _ starting with a letter (32 at most), {n}, then .doc", pattern)
	}
	hasPrefix, stem = m[1] != "", m[2]
	forms := []string{stem}
	if hasPrefix {
		forms = append(forms, tempPrefix+stem)
	}
	for _, f := range forms {
		// The name ends with the number's digits: CON, PRN, AUX and NUL then
		// never match, COM<d> and LPT<d> do.
		if strings.EqualFold(f, "COM") || strings.EqualFold(f, "LPT") {
			return false, "", fmt.Errorf("name pattern %q makes a Windows device name (%s<n>.doc)", pattern, f)
		}
	}
	return hasPrefix, stem, nil
}

// DocumentFileName is a folder's file name for number n, temporary or not:
// the pattern with "{prefix}" replaced by tempPrefix for a temporary document
// and by nothing otherwise, and "{n}" by n in plain decimal.
func DocumentFileName(pattern, tempPrefix string, temporary bool, n int64) string {
	prefix := ""
	if temporary {
		prefix = tempPrefix
	}
	s := strings.Replace(pattern, "{prefix}", prefix, 1)
	return strings.Replace(s, "{n}", fmt.Sprintf("%d", n), 1)
}

// ValidateDocuments refuses a documents block the connector cannot write
// from safely. It reads no disk beyond resolving the folders' symlinks (as
// ValidateOrders does); the provision commands add what needs the box (the
// folder exists, is not a link, the link-mode path rules).
//
// Counters: a key, a kind, 1 ≤ min ≤ max ≤ 999 999 999, a known store adopted
// once and by its own kind, and ranges that never overlap — across every
// counter, test and production, orders and receipts, retired ones included.
// With counters configured, a box that writes orders needs the counter of
// lastOrderNumber.json (not test, not retired: orders without a rule go to the
// production destination), and one that writes receipts needs its receipt
// counter.
//
// Folders: a key, a kind, an absolute path no other folder (typed, pickup or
// receipt) shares in a way that mixes kinds or production with test, never
// the sendOrderDir or a folder holding it, a name pattern in the closed
// grammar, a temporary prefix exactly when the pattern takes one (orders
// only), and a layout for orders.
//
// Receipts binding: a configured receipts folder and receipts counter; a test
// counter only into a test folder; no model (b) importer for a test folder
// (it would import test receipts into the books).
func (c Config) ValidateDocuments() error {
	d := c.Documents
	if len(d.Counters) == 0 && len(d.Folders) == 0 && d.Receipts == (DocumentReceiptsConfig{}) {
		return nil
	}
	if c.ERP != ERPHasavshevet {
		return fmt.Errorf("documents (counters, folders) are Hashavshevet's; erp is %q", c.ERP)
	}
	if err := c.validateCounters(); err != nil {
		return err
	}
	if err := c.validateFolders(); err != nil {
		return err
	}
	return c.validateReceiptsBinding()
}

func (c Config) validateCounters() error {
	keys := c.CounterKeys()
	stores := map[string]string{}
	for _, k := range keys {
		ctr := c.Documents.Counters[k]
		if !ValidDocumentKey(k) {
			return fmt.Errorf("counter key %q: use a lower-case letter, then letters, digits or _ (32 at most)", k)
		}
		switch ctr.Kind {
		case DocumentKindOrders, DocumentKindReceipts:
		default:
			return fmt.Errorf("counter %s: kind %q is not %q or %q", k, ctr.Kind, DocumentKindOrders, DocumentKindReceipts)
		}
		if ctr.Min < 1 || ctr.Max > MaxDocumentNumber || ctr.Min > ctr.Max {
			return fmt.Errorf("counter %s: range %d-%d is not within 1-%d with min ≤ max", k, ctr.Min, ctr.Max, MaxDocumentNumber)
		}
		switch ctr.Store {
		case "":
		case CounterStoreLastOrderNumber:
			if ctr.Kind != DocumentKindOrders {
				return fmt.Errorf("counter %s: store %s is the order counter; the counter's kind must be %s", k, ctr.Store, DocumentKindOrders)
			}
		case CounterStoreReceiptNumber:
			if ctr.Kind != DocumentKindReceipts {
				return fmt.Errorf("counter %s: store %s is the receipt store; the counter's kind must be %s", k, ctr.Store, DocumentKindReceipts)
			}
		default:
			return fmt.Errorf("counter %s: store %q is not %q, %q or empty (a store of its own)", k, ctr.Store, CounterStoreLastOrderNumber, CounterStoreReceiptNumber)
		}
		if ctr.Store != "" {
			if other, dup := stores[ctr.Store]; dup {
				return fmt.Errorf("counters %s and %s both adopt the store %s: it holds one series", other, k, ctr.Store)
			}
			stores[ctr.Store] = k
		}
	}
	for i, a := range keys {
		for _, b := range keys[i+1:] {
			ca, cb := c.Documents.Counters[a], c.Documents.Counters[b]
			if ca.Min <= cb.Max && cb.Min <= ca.Max {
				return fmt.Errorf("counters %s (%d-%d) and %s (%d-%d) overlap: ranges on one box never share a number (an order's job id, status and history are keyed by its number)",
					a, ca.Min, ca.Max, b, cb.Min, cb.Max)
			}
		}
	}
	if len(keys) == 0 {
		return nil
	}
	if c.OrdersConfigured() {
		k := c.DefaultOrderCounterKey()
		if k == "" {
			return fmt.Errorf("orders are configured but no counter adopts the order counter (store %s): today's counter becomes a key before any other counter is added",
				CounterStoreLastOrderNumber)
		}
		ctr := c.Documents.Counters[k]
		if ctr.Test {
			return fmt.Errorf("counter %s numbers the orders sent without a documentRule, which go to the production destination: it cannot be a test counter", k)
		}
		if ctr.Retired {
			return fmt.Errorf("counter %s numbers the orders sent without a documentRule: it cannot be retired", k)
		}
	}
	return nil
}

func (c Config) validateFolders() error {
	fold := runtime.GOOS == "windows"
	order := strings.TrimSpace(c.SendOrderDir)
	pickup := strings.TrimSpace(c.SendOrderPickupDir)
	receipt := strings.TrimSpace(c.SendReceiptDir)
	keys := c.FolderKeys()
	for _, k := range keys {
		f := c.Documents.Folders[k]
		if !ValidDocumentKey(k) {
			return fmt.Errorf("folder key %q: use a lower-case letter, then letters, digits or _ (32 at most)", k)
		}
		p := strings.TrimSpace(f.Path)
		if p != f.Path || p == "" || !filepath.IsAbs(p) || hasControlChar(p) {
			return fmt.Errorf("folder %s: path %q must be an absolute path", k, f.Path)
		}
		hasPrefix, _, err := ParseNamePattern(f.NamePattern, f.TempPrefix)
		if err != nil {
			return fmt.Errorf("folder %s: %v", k, err)
		}
		switch f.Kind {
		case DocumentKindOrders:
			if hasPrefix != (f.TempPrefix != "") {
				return fmt.Errorf("folder %s: a temporary prefix goes with a {prefix} in the name pattern, and only with one", k)
			}
			if f.TempPrefix != "" && !tempPrefixRe.MatchString(f.TempPrefix) {
				return fmt.Errorf("folder %s: temporary prefix %q is not ASCII letters, digits and _ starting with a letter (16 at most)", k, f.TempPrefix)
			}
			switch f.Layout {
			case OrderFormatOfficial, OrderFormatLegacy:
			default:
				return fmt.Errorf("folder %s: layout %q: name the layout its importer's PRM reads (%q or %q)", k, f.Layout, OrderFormatOfficial, OrderFormatLegacy)
			}
		case DocumentKindReceipts:
			if hasPrefix || f.TempPrefix != "" {
				return fmt.Errorf("folder %s: a receipt is never temporary: no {prefix} and no temporary prefix", k)
			}
			if f.Layout != "" {
				return fmt.Errorf("folder %s: a receipt folder has no layout (KUPAIN has one)", k)
			}
		default:
			return fmt.Errorf("folder %s: kind %q is not %q or %q", k, f.Kind, DocumentKindOrders, DocumentKindReceipts)
		}
		if order != "" && (sameDir(order, p, fold) || insideDir(order, p, fold)) {
			return fmt.Errorf("folder %s (%s) is, or holds, the sendOrderDir: the order counter and history copies must not sit in a folder an importer reads", k, p)
		}
		// A test folder is never a production destination; a folder of one
		// kind is never the other kind's.
		if pickup != "" && sameDir(pickup, p, fold) && (f.Test || f.Kind != DocumentKindOrders) {
			return fmt.Errorf("folder %s (%s) is the sendOrderPickupDir, the production orders' folder: only a production orders folder may share it", k, p)
		}
		if receipt != "" && sameDir(receipt, p, fold) && (f.Test || f.Kind != DocumentKindReceipts) {
			return fmt.Errorf("folder %s (%s) is the sendReceiptDir, the production receipts' folder: only a production receipts folder may share it", k, p)
		}
	}
	for i, a := range keys {
		for _, b := range keys[i+1:] {
			if sameDir(c.Documents.Folders[a].Path, c.Documents.Folders[b].Path, fold) {
				return fmt.Errorf("folders %s and %s are the same folder: each key is a folder of its own", a, b)
			}
		}
	}
	return nil
}

func (c Config) validateReceiptsBinding() error {
	b := c.Documents.Receipts
	var folder *FolderConfig
	if k := strings.TrimSpace(b.Folder); k != "" {
		f, ok := c.Documents.Folders[k]
		if !ok {
			return fmt.Errorf("documents.receipts.folder %q is not a configured folder", k)
		}
		if f.Kind != DocumentKindReceipts {
			return fmt.Errorf("documents.receipts.folder %s is a folder for %s, not receipts", k, f.Kind)
		}
		folder = &f
	}
	if k := strings.TrimSpace(b.Counter); k != "" {
		ctr, ok := c.Documents.Counters[k]
		if !ok {
			return fmt.Errorf("documents.receipts.counter %q is not a configured counter", k)
		}
		if ctr.Kind != DocumentKindReceipts {
			return fmt.Errorf("documents.receipts.counter %s is a counter for %s, not receipts", k, ctr.Kind)
		}
	}
	if !c.ReceiptsConfigured() {
		return nil
	}
	if c.CountersConfigured() {
		k := c.ReceiptCounterKey()
		if k == "" {
			return fmt.Errorf("receipts are configured but no counter numbers them: adopt the receipt store (store %s) or name one in documents.receipts.counter",
				CounterStoreReceiptNumber)
		}
		ctr := c.Documents.Counters[k]
		if ctr.Retired {
			return fmt.Errorf("counter %s numbers the receipts: it cannot be retired", k)
		}
		if ctr.Test && (folder == nil || !folder.Test) {
			return fmt.Errorf("counter %s is a test counter: receipts numbered by it may only go to a test folder (documents.receipts.folder)", k)
		}
	}
	if folder != nil && folder.Test && (strings.TrimSpace(c.HasReceiptBatFile) != "" || strings.TrimSpace(c.HasReceiptParamFile) != "") {
		return errors.New("documents.receipts.folder is a test folder and a receipt importer is configured (hasReceiptBatFile / hasReceiptParamFile): it would import test receipts into the books")
	}
	return nil
}

func hasControlChar(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}
