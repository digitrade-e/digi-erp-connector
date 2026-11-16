package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
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
	// TrustedWriters (plan 087 review R1b) are the accounts — the ERP's own
	// importer and scheduler, which must write into the folders (Masofon
	// renames what it imported) — that are never counted as writers of a
	// folder the connector writes documents into: by the commands, the daemon
	// and state. At most MaxTrustedWriters, each a single user account; set
	// only by the documents command, which resolves the names on the box.
	TrustedWriters []TrustedWriter `yaml:"trustedWriters,omitempty"`
}

// TrustedWriter is one trusted account: its SID (what is compared) and its
// name as the box resolved it (for a human).
type TrustedWriter struct {
	SID  string `yaml:"sid" json:"sid"`
	Name string `yaml:"name" json:"name"`
}

// MaxTrustedWriters bounds documents.trustedWriters.
const MaxTrustedWriters = 8

// accountSIDRe is a domain or local account's SID: S-1-5-21-<three
// sub-authorities>-<RID>. Well-known and built-in SIDs (Everyone, Users,
// Authenticated Users, INTERACTIVE, NETWORK, ANONYMOUS, LOCAL SERVICE,
// NETWORK SERVICE, every BUILTIN alias, …) never match; RIDs below 1000 are
// built-in accounts and groups (Guest, Domain Users/Guests/Computers, …).
var accountSIDRe = regexp.MustCompile(`^S-1-5-21-[0-9]{1,10}-[0-9]{1,10}-[0-9]{1,10}-([0-9]{1,10})$`)

// AccountSID reports whether sid can be a single account's (a user, or a
// group an administrator created: the box's lookup tells them apart): an
// S-1-5-21 SID with a RID of 1000 or above.
func AccountSID(sid string) bool {
	m := accountSIDRe.FindStringSubmatch(sid)
	if m == nil {
		return false
	}
	rid, err := strconv.ParseUint(m[1], 10, 32)
	return err == nil && rid >= 1000
}

// TrustedSIDs is the set of documents.trustedWriters' SIDs.
func (c Config) TrustedSIDs() map[string]bool {
	out := map[string]bool{}
	for _, w := range c.Documents.TrustedWriters {
		out[w.SID] = true
	}
	return out
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
// ImplicitOrderCounterKey while no orders counter is configured (today's
// counter, under its implicit key); "" when orders counters are configured
// and none adopts lastOrderNumber.json (ValidateDocuments refuses that when
// orders are configured).
func (c Config) DefaultOrderCounterKey() string {
	if k := c.counterWithStore(CounterStoreLastOrderNumber); k != "" {
		return k
	}
	if !c.hasCounterOfKind(DocumentKindOrders) {
		return ImplicitOrderCounterKey
	}
	return ""
}

// ReceiptCounterKey is the key of the counter receipts are numbered from:
// documents.receipts.counter, else the configured one with store
// receiptNumber, else ImplicitReceiptCounterKey while no receipts counter is
// configured; "" otherwise (ValidateDocuments refuses that when receipts are
// configured).
func (c Config) ReceiptCounterKey() string {
	if k := strings.TrimSpace(c.Documents.Receipts.Counter); k != "" {
		return k
	}
	if k := c.counterWithStore(CounterStoreReceiptNumber); k != "" {
		return k
	}
	if !c.hasCounterOfKind(DocumentKindReceipts) {
		return ImplicitReceiptCounterKey
	}
	return ""
}

// ImplicitCounter reports whether kind is numbered by today's store under its
// implicit key: no counter of that kind is configured. Counters are adopted
// one kind at a time (a box writing orders and receipts adopts one store,
// then the other); a kind without a configured counter keeps today's. Its
// range (1-999 999 999) may overlap the other kind's configured ranges: each
// kind has its own index, history and job ids, so that is harmless, and
// within a kind the implicit counter never sits beside a configured one.
func (c Config) ImplicitCounter(kind string) bool { return !c.hasCounterOfKind(kind) }

func (c Config) counterWithStore(store string) string {
	for _, k := range c.CounterKeys() {
		if c.Documents.Counters[k].Store == store {
			return k
		}
	}
	return ""
}

func (c Config) hasCounterOfKind(kind string) bool {
	for _, ctr := range c.Documents.Counters {
		if ctr.Kind == kind {
			return true
		}
	}
	return false
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

// WriteFolder is a folder the connector writes documents into, as
// LocalSystem, for an importer to read (plan 087 review M1): whoever else can
// write to it can pre-create the next document's name, drop a document the
// importer takes into the books, or plant what an importer running there
// loads. Only the administrators should be able to.
type WriteFolder struct {
	// Name is how logs, state and refusals name it: "sendOrderDir",
	// "sendOrderPickupDir", "sendReceiptDir" or "folder <key>".
	Name string `json:"name"`
	Path string `json:"path"`
	// Key is a typed folder's key; Typed marks a typed folder (plan 087), set
	// only by the documents command — behind the link the daemon writes into
	// none that others can write to. The other three predate plan 087: the
	// daemon warns about them.
	Key   string `json:"key,omitempty"`
	Typed bool   `json:"typed,omitempty"`
	// Receipts: receipts are written here (documents.receipts.folder's
	// folder, or sendReceiptDir).
	Receipts bool `json:"receipts,omitempty"`
}

// WriteFolders lists every folder the connector writes documents into:
// sendOrderDir (the order counter, history and model (b)'s IMOVEIN, where
// has.exe runs) and the pickup folder when orders are configured, every typed
// folder, and sendReceiptDir when receipts go there (no bound typed folder).
func (c Config) WriteFolders() []WriteFolder {
	var out []WriteFolder
	if c.OrdersConfigured() {
		out = append(out, WriteFolder{Name: "sendOrderDir", Path: strings.TrimSpace(c.SendOrderDir)})
		if c.OrdersPickup() {
			out = append(out, WriteFolder{Name: "sendOrderPickupDir", Path: strings.TrimSpace(c.SendOrderPickupDir)})
		}
	}
	bound := strings.TrimSpace(c.Documents.Receipts.Folder)
	for _, k := range c.FolderKeys() {
		out = append(out, WriteFolder{Name: "folder " + k, Path: strings.TrimSpace(c.Documents.Folders[k].Path), Key: k, Typed: true,
			Receipts: k == bound && c.ReceiptsConfigured()})
	}
	if bound == "" && c.ReceiptsConfigured() {
		out = append(out, WriteFolder{Name: "sendReceiptDir", Path: strings.TrimSpace(c.SendReceiptDir), Receipts: true})
	}
	return out
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
	out := DocumentsConfig{Receipts: d.Receipts, TrustedWriters: append([]TrustedWriter(nil), d.TrustedWriters...)}
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

// DocumentKeyGrammar is the key grammar as a refusal's fix says it.
const DocumentKeyGrammar = "use a lower-case letter, then letters, digits or _ (32 at most), and not a Windows device name (con, prn, aux, nul, com0-9, lpt0-9)"

// ValidDocumentKey reports whether k is a counter or folder key: the grammar,
// and never a Windows device name (review m2) — a counter's own store is
// DataDir\counters\<key>.json, and on Windows Server 2016-2022 aux.json or
// com1.json names a device, whose read can block (in Accept, under the
// queue's lock, every order would stall).
func ValidDocumentKey(k string) bool { return documentKeyRe.MatchString(k) && !WindowsDeviceName(k) }

// WindowsDeviceName reports whether name, with or without an extension and
// in any case, is a name Windows reserves for a device: CON, PRN, AUX, NUL,
// COM0-COM9, LPT0-LPT9.
func WindowsDeviceName(name string) bool {
	stem, _, _ := strings.Cut(name, ".")
	stem = strings.ToUpper(strings.TrimRight(stem, " "))
	switch stem {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	return len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '0' && stem[3] <= '9'
}

// namePatternRe is the closed grammar of a folder's file name. The stem ends
// in a letter or "_" (review m11): a stem ending in a digit runs into the
// number — IMOVEIN_1 with n = 23 is IMOVEIN_123, another series' name for
// order 123.
var namePatternRe = regexp.MustCompile(`^(\{prefix\})?([A-Za-z](?:[A-Za-z0-9_]{0,30}[A-Za-z_])?)\{n\}\.doc$`)

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
		return false, "", fmt.Errorf("name pattern %q is not {prefix}<stem>{n}.doc: an optional {prefix}, a stem of ASCII letters, digits and _ starting with a letter and ending in a letter or _ (32 at most), {n}, then .doc", pattern)
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
// counter only into a test folder, and a test folder only from a test counter
// (review m10); no model (b) importer for a test folder (it would import test
// receipts into the books).
func (c Config) ValidateDocuments() error {
	d := c.Documents
	if len(d.Counters) == 0 && len(d.Folders) == 0 && d.Receipts == (DocumentReceiptsConfig{}) && len(d.TrustedWriters) == 0 {
		return nil
	}
	if c.ERP != ERPHasavshevet {
		return fmt.Errorf("documents (counters, folders) are Hashavshevet's; erp is %q", c.ERP)
	}
	if err := c.validateTrustedWriters(); err != nil {
		return err
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
			return fmt.Errorf("counter key %q: %s", k, DocumentKeyGrammar)
		}
		switch ctr.Kind {
		case DocumentKindOrders, DocumentKindReceipts:
		default:
			return fmt.Errorf("counter %s: kind %q is not %q or %q", k, ctr.Kind, DocumentKindOrders, DocumentKindReceipts)
		}
		// The implicit keys name today's stores while their kind has no
		// configured counter: a configured counter of the other kind under
		// one of them would share its key.
		if (k == ImplicitOrderCounterKey && ctr.Kind != DocumentKindOrders) || (k == ImplicitReceiptCounterKey && ctr.Kind != DocumentKindReceipts) {
			return fmt.Errorf("counter %s: the key %q is today's %s counter's; a counter of kind %s takes another key", k, k, map[string]string{ImplicitOrderCounterKey: "order", ImplicitReceiptCounterKey: "receipt"}[k], ctr.Kind)
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
			return fmt.Errorf("orders are configured but no counter adopts the order counter (store %s): today's counter becomes a key before any other orders counter is added",
				CounterStoreLastOrderNumber)
		}
		ctr := c.Documents.Counters[k] // the zero value for the implicit counter
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
			return fmt.Errorf("folder key %q: %s", k, DocumentKeyGrammar)
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
	// Review m11: a test folder is never inside a production folder, nor the
	// reverse — an importer that reads subfolders would take test documents
	// into the books. The pickup and receipt folders are production folders.
	type named struct {
		name, path string
		test       bool
	}
	var all []named
	for _, k := range keys {
		all = append(all, named{"folder " + k, c.Documents.Folders[k].Path, c.Documents.Folders[k].Test})
	}
	if pickup != "" {
		all = append(all, named{"the sendOrderPickupDir", pickup, false})
	}
	if receipt != "" {
		all = append(all, named{"the sendReceiptDir", receipt, false})
	}
	for _, a := range all {
		for _, b := range all {
			if a.test && !b.test && (insideDir(a.path, b.path, fold) || insideDir(b.path, a.path, fold)) {
				return fmt.Errorf("%s (%s, a test folder) and %s (%s, a production folder) are nested: an importer that reads subfolders would take test documents into the books",
					a.name, a.path, b.name, b.path)
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
	counterTest := false // today's receipt store, under its implicit key, is production
	if c.CountersConfigured() {
		k := c.ReceiptCounterKey()
		if k == "" {
			return fmt.Errorf("receipts are configured but no counter numbers them: adopt the receipt store (store %s) or name one in documents.receipts.counter",
				CounterStoreReceiptNumber)
		}
		ctr := c.Documents.Counters[k] // the zero value for the implicit counter
		if ctr.Retired {
			return fmt.Errorf("counter %s numbers the receipts: it cannot be retired", k)
		}
		if ctr.Test && (folder == nil || !folder.Test) {
			return fmt.Errorf("counter %s is a test counter: receipts numbered by it may only go to a test folder (documents.receipts.folder)", k)
		}
		counterTest = ctr.Test
	}
	// Review m10: nor a production counter into a test folder — a receipt is a
	// tax document, and a production number written where the books never
	// import it is a gap in the series.
	if folder != nil && folder.Test && !counterTest {
		return fmt.Errorf("documents.receipts.folder %s is a test folder and receipts are numbered by the production counter %s: a production receipt number "+
			"written where the books never import it is a gap in the tax-document series — bind a test receipts counter (documents.receipts.counter)",
			strings.TrimSpace(b.Folder), c.ReceiptCounterKey())
	}
	if folder != nil && folder.Test && (strings.TrimSpace(c.HasReceiptBatFile) != "" || strings.TrimSpace(c.HasReceiptParamFile) != "") {
		return errors.New("documents.receipts.folder is a test folder and a receipt importer is configured (hasReceiptBatFile / hasReceiptParamFile): it would import test receipts into the books")
	}
	return nil
}

// validateTrustedWriters: at most MaxTrustedWriters, each an account SID
// (never a well-known or built-in one: those are groups of every user) given
// once, with a name.
func (c Config) validateTrustedWriters() error {
	ws := c.Documents.TrustedWriters
	if len(ws) > MaxTrustedWriters {
		return fmt.Errorf("documents.trustedWriters has %d accounts: at most %d (the ERP's own importer and scheduler)", len(ws), MaxTrustedWriters)
	}
	seen := map[string]bool{}
	for _, w := range ws {
		switch {
		case !AccountSID(w.SID):
			return fmt.Errorf("documents.trustedWriters: %q (%s) is not a single account's SID: well-known and built-in SIDs (Everyone, Users, Authenticated Users, Domain Users, …) are never trusted", w.SID, w.Name)
		case seen[w.SID]:
			return fmt.Errorf("documents.trustedWriters names %s twice", w.SID)
		case strings.TrimSpace(w.Name) == "" || len(w.Name) > 256 || hasControlChar(w.Name):
			return fmt.Errorf("documents.trustedWriters: %s has no usable name", w.SID)
		}
		seen[w.SID] = true
	}
	return nil
}

func hasControlChar(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}
