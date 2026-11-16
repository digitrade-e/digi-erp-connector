package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bflDocuments is BFL's box as plan 087 sets it: today's order counter and
// receipt store adopted as b2b and receipts, test counters beside them, and
// typed folders for both kinds, production and test.
func bflDocuments(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	dir := func(parts ...string) string {
		p := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	return Config{
		ERP:                ERPHasavshevet,
		SendOrderDir:       dir("digi", "orders"),
		SendOrderPickupDir: dir("herp"),
		SendOrderFormat:    OrderFormatLegacy,
		SendReceiptDir:     dir("herp", "receipts"),
		Documents: DocumentsConfig{
			Counters: map[string]CounterConfig{
				"b2b":           {Kind: DocumentKindOrders, Min: 1_000_000, Max: 1_999_999, Store: CounterStoreLastOrderNumber},
				"receipts":      {Kind: DocumentKindReceipts, Min: 500_000, Max: 599_999, Store: CounterStoreReceiptNumber},
				"b2b_test":      {Kind: DocumentKindOrders, Min: 9_000_000, Max: 9_099_999, Test: true},
				"receipts_test": {Kind: DocumentKindReceipts, Min: 9_100_000, Max: 9_199_999, Test: true},
			},
			Folders: map[string]FolderConfig{
				"herp":               {Kind: DocumentKindOrders, Path: dir("herp"), NamePattern: "{prefix}IMOVEIN_{n}.doc", TempPrefix: "TMP_", Layout: OrderFormatLegacy},
				"herp_receipts":      {Kind: DocumentKindReceipts, Path: dir("herp", "receipts"), NamePattern: "KUPAIN_{n}.doc"},
				"herp_test":          {Kind: DocumentKindOrders, Path: dir("herp_test"), NamePattern: "{prefix}IMOVEIN_{n}.doc", TempPrefix: "TMP_", Layout: OrderFormatLegacy, Test: true},
				"herp_receipts_test": {Kind: DocumentKindReceipts, Path: dir("herp_test", "receipts"), NamePattern: "KUPAIN_{n}.doc", Test: true},
			},
			Receipts: DocumentReceiptsConfig{Folder: "herp_receipts", Counter: "receipts"},
		},
	}
}

func TestValidateDocuments_BFLBoxIsAccepted(t *testing.T) {
	cfg := bflDocuments(t)
	if err := cfg.ValidateOrders(); err != nil {
		t.Fatalf("ValidateOrders: %v", err)
	}
	if got := cfg.DefaultOrderCounterKey(); got != "b2b" {
		t.Errorf("default order counter %q, want b2b", got)
	}
	if got := cfg.ReceiptCounterKey(); got != "receipts" {
		t.Errorf("receipt counter %q, want receipts", got)
	}
	if got := cfg.ReceiptDir(); got != cfg.Documents.Folders["herp_receipts"].Path {
		t.Errorf("receipt dir %q", got)
	}
}

func TestValidateDocuments_EmptyIsTodaysBehaviour(t *testing.T) {
	cfg := Config{ERP: ERPHasavshevet, SendOrderDir: t.TempDir()}
	if err := cfg.ValidateDocuments(); err != nil {
		t.Fatal(err)
	}
	if cfg.CountersConfigured() || cfg.DefaultOrderCounterKey() != ImplicitOrderCounterKey || cfg.ReceiptCounterKey() != ImplicitReceiptCounterKey {
		t.Error("an empty documents block must keep the implicit counters")
	}
	if cfg.ReceiptNamePattern() != DefaultReceiptNamePattern {
		t.Error("an empty documents block must keep KUPAIN_{n}.doc")
	}
	// The block is omitted from config.yaml when empty: an apply that changes
	// nothing writes nothing.
	out, err := Marshal(cfg)
	if err != nil || strings.Contains(string(out), "documents") {
		t.Errorf("an empty documents block is written: %s %v", out, err)
	}
}

func TestValidateDocuments_Refusals(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(c *Config)
		want   string
	}{
		{"sap", func(c *Config) { c.ERP = ERPSAP }, "Hashavshevet"},
		{"counter key with a slash", func(c *Config) {
			c.Documents.Counters["b2b/test"] = CounterConfig{Kind: DocumentKindOrders, Min: 8_000_000, Max: 8_000_001}
		}, "counter key"},
		{"counter key upper case", func(c *Config) {
			c.Documents.Counters["B2B2"] = CounterConfig{Kind: DocumentKindOrders, Min: 8_000_000, Max: 8_000_001}
		}, "counter key"},
		{"unknown kind", func(c *Config) {
			c.Documents.Counters["x"] = CounterConfig{Kind: "invoices", Min: 8_000_000, Max: 8_000_001}
		}, "kind"},
		{"max past nine digits", func(c *Config) {
			c.Documents.Counters["x"] = CounterConfig{Kind: DocumentKindOrders, Min: 8_000_000, Max: 1_000_000_000}
		}, "range"},
		{"min zero", func(c *Config) {
			c.Documents.Counters["x"] = CounterConfig{Kind: DocumentKindOrders, Min: 0, Max: 5}
		}, "range"},
		{"min above max", func(c *Config) {
			c.Documents.Counters["x"] = CounterConfig{Kind: DocumentKindOrders, Min: 8_000_002, Max: 8_000_001}
		}, "range"},
		{"overlap between test and production", func(c *Config) {
			ctr := c.Documents.Counters["b2b_test"]
			ctr.Min = 1_999_999
			c.Documents.Counters["b2b_test"] = ctr
		}, "overlap"},
		{"overlap between an order and a receipt counter", func(c *Config) {
			c.Documents.Counters["x"] = CounterConfig{Kind: DocumentKindReceipts, Min: 1_500_000, Max: 1_500_001}
		}, "overlap"},
		{"overlap with a retired counter", func(c *Config) {
			c.Documents.Counters["old"] = CounterConfig{Kind: DocumentKindOrders, Min: 1, Max: 999_999, Retired: true}
			c.Documents.Counters["new"] = CounterConfig{Kind: DocumentKindOrders, Min: 999_999, Max: 999_999}
		}, "overlap"},
		{"unknown store", func(c *Config) {
			c.Documents.Counters["x"] = CounterConfig{Kind: DocumentKindOrders, Min: 8_000_000, Max: 8_000_001, Store: "other.json"}
		}, "store"},
		{"order store on a receipts counter", func(c *Config) {
			ctr := c.Documents.Counters["b2b"]
			ctr.Kind = DocumentKindReceipts
			c.Documents.Counters["b2b"] = ctr
		}, "store"},
		{"store adopted twice", func(c *Config) {
			c.Documents.Counters["x"] = CounterConfig{Kind: DocumentKindOrders, Min: 8_000_000, Max: 8_000_001, Store: CounterStoreLastOrderNumber}
		}, "both adopt"},
		{"orders without the order counter adopted", func(c *Config) {
			ctr := c.Documents.Counters["b2b"]
			ctr.Store = ""
			c.Documents.Counters["b2b"] = ctr
		}, "adopts the order counter"},
		{"default order counter is a test counter", func(c *Config) {
			ctr := c.Documents.Counters["b2b"]
			ctr.Test = true
			c.Documents.Counters["b2b"] = ctr
		}, "test counter"},
		{"default order counter retired", func(c *Config) {
			ctr := c.Documents.Counters["b2b"]
			ctr.Retired = true
			c.Documents.Counters["b2b"] = ctr
		}, "retired"},
		{"receipt counter retired", func(c *Config) {
			ctr := c.Documents.Counters["receipts"]
			ctr.Retired = true
			c.Documents.Counters["receipts"] = ctr
		}, "retired"},
		{"test receipt counter into a production folder", func(c *Config) {
			c.Documents.Receipts.Counter = "receipts_test"
		}, "test folder"},
		{"receipts bound to an orders folder", func(c *Config) { c.Documents.Receipts.Folder = "herp" }, "not receipts"},
		{"receipts bound to an unknown folder", func(c *Config) { c.Documents.Receipts.Folder = "nope" }, "not a configured folder"},
		{"receipts bound to an orders counter", func(c *Config) { c.Documents.Receipts.Counter = "b2b" }, "not receipts"},
		{"receipts bound to an unknown counter", func(c *Config) { c.Documents.Receipts.Counter = "nope" }, "not a configured counter"},
		{"test receipt folder with an importer", func(c *Config) {
			c.Documents.Receipts = DocumentReceiptsConfig{Folder: "herp_receipts_test", Counter: "receipts_test"}
			c.HasReceiptBatFile = filepath.Join(c.SendOrderDir, "kupain.bat")
		}, "import test receipts"},
		{"folder key with a slash", func(c *Config) {
			f := c.Documents.Folders["herp_test"]
			delete(c.Documents.Folders, "herp_test")
			c.Documents.Folders["herp/test"] = f
		}, "folder key"},
		{"relative path", func(c *Config) { setFolder(c, "herp_test", func(f *FolderConfig) { f.Path = "herp_test" }) }, "absolute"},
		{"path with surrounding spaces", func(c *Config) { setFolder(c, "herp_test", func(f *FolderConfig) { f.Path = " " + f.Path }) }, "absolute"},
		{"pattern without {n}", func(c *Config) {
			setFolder(c, "herp_test", func(f *FolderConfig) { f.NamePattern = "{prefix}IMOVEIN.doc" })
		}, "name pattern"},
		{"pattern with a separator", func(c *Config) {
			setFolder(c, "herp_test", func(f *FolderConfig) { f.NamePattern = `{prefix}..\x\IMOVEIN_{n}.doc` })
		}, "name pattern"},
		{"pattern ending .bat", func(c *Config) {
			setFolder(c, "herp_test", func(f *FolderConfig) { f.NamePattern = "{prefix}IMOVEIN_{n}.bat" })
		}, "name pattern"},
		{"pattern not ASCII", func(c *Config) {
			setFolder(c, "herp_test", func(f *FolderConfig) { f.NamePattern = "{prefix}הזמנה_{n}.doc" })
		}, "name pattern"},
		{"pattern with text after {n}", func(c *Config) {
			setFolder(c, "herp_test", func(f *FolderConfig) { f.NamePattern = "{prefix}IMOVEIN_{n}_x.doc" })
		}, "name pattern"},
		{"pattern with {prefix} not first", func(c *Config) {
			setFolder(c, "herp_test", func(f *FolderConfig) { f.NamePattern = "IMOVEIN_{prefix}{n}.doc" })
		}, "name pattern"},
		{"pattern with two {n}", func(c *Config) {
			setFolder(c, "herp_test", func(f *FolderConfig) { f.NamePattern = "{prefix}A{n}{n}.doc" })
		}, "name pattern"},
		{"device name COM", func(c *Config) {
			setFolder(c, "herp_test", func(f *FolderConfig) { f.NamePattern = "{prefix}COM{n}.doc" })
		}, "device"},
		{"device name lpt", func(c *Config) {
			setFolder(c, "herp_test", func(f *FolderConfig) { f.NamePattern = "lpt{n}.doc"; f.TempPrefix = "" })
		}, "device"},
		{"device name through the prefix", func(c *Config) {
			setFolder(c, "herp_test", func(f *FolderConfig) { f.NamePattern = "{prefix}M{n}.doc"; f.TempPrefix = "CO" })
		}, "device"},
		{"prefix without {prefix}", func(c *Config) {
			setFolder(c, "herp_test", func(f *FolderConfig) { f.NamePattern = "IMOVEIN_{n}.doc" })
		}, "temporary prefix"},
		{"{prefix} without a prefix", func(c *Config) { setFolder(c, "herp_test", func(f *FolderConfig) { f.TempPrefix = "" }) }, "temporary prefix"},
		{"prefix with a dot", func(c *Config) { setFolder(c, "herp_test", func(f *FolderConfig) { f.TempPrefix = "T.MP" }) }, "temporary prefix"},
		{"orders folder without a layout", func(c *Config) { setFolder(c, "herp_test", func(f *FolderConfig) { f.Layout = "" }) }, "layout"},
		{"receipts folder with a prefix", func(c *Config) {
			setFolder(c, "herp_receipts_test", func(f *FolderConfig) { f.NamePattern = "{prefix}KUPAIN_{n}.doc"; f.TempPrefix = "TMP_" })
		}, "never temporary"},
		{"receipts folder with a layout", func(c *Config) {
			setFolder(c, "herp_receipts_test", func(f *FolderConfig) { f.Layout = OrderFormatLegacy })
		}, "no layout"},
		{"folder kind unknown", func(c *Config) { setFolder(c, "herp_test", func(f *FolderConfig) { f.Kind = "files" }) }, "kind"},
		{"folder is the sendOrderDir", func(c *Config) { setFolder(c, "herp_test", func(f *FolderConfig) { f.Path = c.SendOrderDir }) }, "sendOrderDir"},
		{"folder holds the sendOrderDir", func(c *Config) {
			setFolder(c, "herp_test", func(f *FolderConfig) { f.Path = filepath.Dir(c.SendOrderDir) })
		}, "sendOrderDir"},
		{"test folder is the pickup folder", func(c *Config) {
			setFolder(c, "herp", func(f *FolderConfig) { f.Path += "_moved" })
			setFolder(c, "herp_test", func(f *FolderConfig) { f.Path = c.SendOrderPickupDir })
		}, "sendOrderPickupDir"},
		{"receipts folder is the pickup folder", func(c *Config) {
			setFolder(c, "herp", func(f *FolderConfig) { f.Path += "_moved" })
			setFolder(c, "herp_receipts_test", func(f *FolderConfig) { f.Path = c.SendOrderPickupDir; f.Test = false })
		}, "sendOrderPickupDir"},
		{"test folder is the receipt folder", func(c *Config) {
			setFolder(c, "herp_receipts", func(f *FolderConfig) { f.Path += "_moved" })
			setFolder(c, "herp_receipts_test", func(f *FolderConfig) { f.Path = c.SendReceiptDir })
		}, "sendReceiptDir"},
		{"two keys, one folder", func(c *Config) {
			setFolder(c, "herp_test", func(f *FolderConfig) { f.Path = c.Documents.Folders["herp_receipts_test"].Path; f.Test = true })
		}, "same folder"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := bflDocuments(t)
			tc.mutate(&cfg)
			err := cfg.ValidateOrders()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error with %q", err, tc.want)
			}
		})
	}
}

func setFolder(c *Config, key string, f func(*FolderConfig)) {
	v := c.Documents.Folders[key]
	f(&v)
	c.Documents.Folders[key] = v
}

func TestValidateDocuments_ReceiptsOnlyBoxNeedsNoOrderCounter(t *testing.T) {
	cfg := Config{ERP: ERPHasavshevet, SendReceiptDir: t.TempDir(), Documents: DocumentsConfig{
		Counters: map[string]CounterConfig{"receipts": {Kind: DocumentKindReceipts, Min: 1, Max: 999, Store: CounterStoreReceiptNumber}},
	}}
	if err := cfg.ValidateOrders(); err != nil {
		t.Fatalf("a box without orders needs no order counter: %v", err)
	}
	cfg.Documents.Counters = map[string]CounterConfig{"r2": {Kind: DocumentKindReceipts, Min: 1, Max: 999}}
	if err := cfg.ValidateOrders(); err == nil || !strings.Contains(err.Error(), "no counter numbers them") {
		t.Fatalf("receipts with counters but none bound: %v", err)
	}
	cfg.Documents.Receipts.Counter = "r2"
	if err := cfg.ValidateOrders(); err != nil {
		t.Fatalf("a receipt counter of its own, bound: %v", err)
	}
}

func TestValidateDocuments_ReceiptFolderTurnsReceiptsOn(t *testing.T) {
	cfg := bflDocuments(t)
	cfg.SendReceiptDir = ""
	if !cfg.ReceiptsConfigured() || cfg.ReceiptDir() != cfg.Documents.Folders["herp_receipts"].Path {
		t.Fatal("a bound receipts folder is the receipt folder")
	}
	if err := cfg.ValidateOrders(); err != nil {
		t.Fatal(err)
	}
	cfg.Documents.Receipts = DocumentReceiptsConfig{Folder: "herp_receipts_test", Counter: "receipts_test"}
	if err := cfg.ValidateOrders(); err != nil {
		t.Fatalf("test counter into a test folder: %v", err)
	}
	if cfg.ReceiptNamePattern() != "KUPAIN_{n}.doc" {
		t.Error(cfg.ReceiptNamePattern())
	}
}

func TestDocumentFileName(t *testing.T) {
	tests := []struct {
		pattern, prefix string
		temporary       bool
		n               int64
		want            string
	}{
		{"{prefix}IMOVEIN_{n}.doc", "TMP_", false, 1000123, "IMOVEIN_1000123.doc"},
		{"{prefix}IMOVEIN_{n}.doc", "TMP_", true, 1000123, "TMP_IMOVEIN_1000123.doc"},
		{"KUPAIN_{n}.doc", "", false, 7, "KUPAIN_7.doc"},
	}
	for _, tc := range tests {
		if got := DocumentFileName(tc.pattern, tc.prefix, tc.temporary, tc.n); got != tc.want {
			t.Errorf("%s %v %d = %q, want %q", tc.pattern, tc.temporary, tc.n, got, tc.want)
		}
	}
}

func TestDocumentsClone_DoesNotShareMaps(t *testing.T) {
	cfg := bflDocuments(t)
	c := cfg.Documents.Clone()
	c.Counters["new"] = CounterConfig{}
	c.Folders["new"] = FolderConfig{}
	if _, ok := cfg.Documents.Counters["new"]; ok {
		t.Error("counters shared")
	}
	if _, ok := cfg.Documents.Folders["new"]; ok {
		t.Error("folders shared")
	}
}

func TestDocuments_RoundTripsThroughYAML(t *testing.T) {
	cfg := bflDocuments(t)
	out, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := Marshal(back)
	if string(again) != string(out) {
		t.Errorf("not stable:\n%s\n%s", out, again)
	}
	if back.Documents.Counters["b2b_test"].Test != true || back.Documents.Folders["herp"].TempPrefix != "TMP_" {
		t.Errorf("lost fields: %+v", back.Documents)
	}
}

// A box that writes orders and receipts adopts one store, then the other:
// the kind without a configured counter keeps today's under its implicit key.
func TestValidateDocuments_OneKindAtATime(t *testing.T) {
	cfg := Config{ERP: ERPHasavshevet, SendOrderDir: t.TempDir(), SendReceiptDir: t.TempDir(), Documents: DocumentsConfig{
		Counters: map[string]CounterConfig{"b2b": {Kind: DocumentKindOrders, Min: 1_000_000, Max: 1_999_999, Store: CounterStoreLastOrderNumber}},
	}}
	if err := cfg.ValidateOrders(); err != nil {
		t.Fatalf("orders adopted first: %v", err)
	}
	if cfg.ReceiptCounterKey() != ImplicitReceiptCounterKey || !cfg.ImplicitCounter(DocumentKindReceipts) || cfg.ImplicitCounter(DocumentKindOrders) {
		t.Errorf("receipts keep today's store: %q", cfg.ReceiptCounterKey())
	}
	cfg.Documents.Counters = map[string]CounterConfig{"receipts": {Kind: DocumentKindReceipts, Min: 500_000, Max: 599_999, Store: CounterStoreReceiptNumber}}
	if err := cfg.ValidateOrders(); err != nil {
		t.Fatalf("receipts adopted first: %v", err)
	}
	if cfg.DefaultOrderCounterKey() != ImplicitOrderCounterKey {
		t.Errorf("orders keep today's counter: %q", cfg.DefaultOrderCounterKey())
	}
	// The implicit keys are not for a counter of the other kind.
	cfg.Documents.Counters["orders"] = CounterConfig{Kind: DocumentKindReceipts, Min: 1, Max: 9}
	if err := cfg.ValidateOrders(); err == nil || !strings.Contains(err.Error(), "today's order counter") {
		t.Errorf("a receipts counter named orders: %v", err)
	}
}

// Review m2: a key that is a Windows device name is refused — a counter's own
// store is DataDir\counters\<key>.json, and aux.json names a device on
// Windows Server 2016-2022 (a read can block).
func TestValidDocumentKey_RefusesWindowsDeviceNames(t *testing.T) {
	for _, k := range []string{"con", "prn", "aux", "nul", "com0", "com1", "com9", "lpt0", "lpt1", "lpt9"} {
		if ValidDocumentKey(k) {
			t.Errorf("%q accepted", k)
		}
	}
	for _, k := range []string{"aux1", "auxiliary", "console", "nuls", "com10", "comx", "lpt_1", "com_1", "b2b", "orders"} {
		if !ValidDocumentKey(k) {
			t.Errorf("%q refused", k)
		}
	}
	for _, name := range []string{"AUX", "aux.json", "Com1.doc", "lpt9.tar.gz", "NUL ", "con .txt", "prn"} {
		if !WindowsDeviceName(name) {
			t.Errorf("WindowsDeviceName(%q) = false", name)
		}
	}
	for _, name := range []string{"aux1.json", "com.json", "lpt", "x.aux", "kupain_1.doc"} {
		if WindowsDeviceName(name) {
			t.Errorf("WindowsDeviceName(%q) = true", name)
		}
	}
	cfg := bflDocuments(t)
	cfg.Documents.Counters["aux"] = CounterConfig{Kind: DocumentKindOrders, Min: 8_000_000, Max: 8_000_001}
	if err := cfg.ValidateOrders(); err == nil || !strings.Contains(err.Error(), "device name") {
		t.Errorf("counter aux: %v", err)
	}
	cfg = bflDocuments(t)
	cfg.Documents.Folders["nul"] = FolderConfig{Kind: DocumentKindOrders, Path: t.TempDir(), NamePattern: "X_{n}.doc", Layout: OrderFormatLegacy}
	if err := cfg.ValidateOrders(); err == nil || !strings.Contains(err.Error(), "device name") {
		t.Errorf("folder nul: %v", err)
	}
}
