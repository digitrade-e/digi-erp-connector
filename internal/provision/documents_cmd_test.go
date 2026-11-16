package provision

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
)

// Plan 087: the documents command.

func docsIn(f func(*DocumentsInput)) DocumentsInput {
	in := DocumentsInput{Version: 1, ChangeSet: "cs-documents-01"}
	f(&in)
	return in
}

func strp(s string) *string { return &s }

// countersBox is the hand-install box with BFL's counters adopted.
func countersBox(t *testing.T) (*fakeSys, Env, string) {
	t.Helper()
	sys, fdb, orders := handInstallBox(t)
	cfg, _ := readConfig(t, sys.dir)
	cfg.Documents.Counters = map[string]config.CounterConfig{
		"b2b":           {Kind: "orders", Min: 1_000_000, Max: 1_999_999, Store: config.CounterStoreLastOrderNumber},
		"b2b_test":      {Kind: "orders", Min: 9_000_000, Max: 9_099_999, Test: true},
		"receipts":      {Kind: "receipts", Min: 500_000, Max: 599_999, Store: config.CounterStoreReceiptNumber},
		"receipts_test": {Kind: "receipts", Min: 9_100_000, Max: 9_199_999, Test: true},
	}
	writeConfig(t, sys.dir, cfg)
	writeOrderCounter(t, orders, 1_000_295)
	return sys, testEnv(sys, fdb), orders
}

func TestDocuments_TypedFoldersAndTheReceiptsBinding(t *testing.T) {
	sys, env, _ := countersBox(t)
	_, beforeBytes := readConfig(t, sys.dir)
	herp, herpTest := mkdir(t, "herp"), mkdir(t, "herp_test")
	kupain, kupainTest := mkdir(t, "herp", "receipts"), mkdir(t, "herp_test", "receipts")
	in := docsIn(func(in *DocumentsInput) {
		in.Folders = map[string]*FolderInput{
			"herp":               {Kind: "orders", Path: herp, NamePattern: "{prefix}IMOVEIN_{n}.doc", TempPrefix: "TMP_", Layout: "legacy"},
			"herp_test":          {Kind: "orders", Path: herpTest, NamePattern: "{prefix}IMOVEIN_{n}.doc", TempPrefix: "TMP_", Layout: "legacy", Test: true},
			"herp_receipts":      {Kind: "receipts", Path: kupain, NamePattern: "KUPAIN_{n}.doc"},
			"herp_receipts_test": {Kind: "receipts", Path: kupainTest, NamePattern: "KUPAIN_{n}.doc", Test: true},
		}
		in.Receipts = &DocumentsReceiptsInput{Folder: strp("herp_receipts"), Counter: strp("receipts")}
	})
	chk, err := ApplyDocuments(context.Background(), env, in, true)
	if err != nil || chk.Check == nil || chk.Check.Config != "changed: documents" || !chk.Check.Restart {
		t.Fatalf("--check %+v %v", chk.Check, err)
	}
	res, err := ApplyDocuments(context.Background(), env, in, false)
	if err != nil || !res.Restarted || res.Proof == nil {
		t.Fatalf("documents: %+v %v", res, err)
	}
	cfg, _ := readConfig(t, sys.dir)
	if len(cfg.Documents.Folders) != 4 || cfg.ReceiptDir() != kupain || cfg.ReceiptCounterKey() != "receipts" {
		t.Fatalf("config %+v", cfg.Documents)
	}
	if len(cfg.Documents.Counters) != 4 {
		t.Error("the counters were touched")
	}

	// Receipts to the test pair: a test counter into a test folder.
	sw := docsIn(func(in *DocumentsInput) {
		in.ChangeSet = "cs-documents-02"
		in.Receipts = &DocumentsReceiptsInput{Folder: strp("herp_receipts_test"), Counter: strp("receipts_test")}
	})
	if _, err := ApplyDocuments(context.Background(), env, sw, false); err != nil {
		t.Fatal(err)
	}
	// A test counter into a production folder: the daemon's own check.
	_, err = ApplyDocuments(context.Background(), env, docsIn(func(in *DocumentsInput) {
		in.ChangeSet = "cs-documents-03"
		in.Receipts = &DocumentsReceiptsInput{Folder: strp("herp_receipts")}
	}), false)
	if r := (*Refusal)(nil); !errors.As(err, &r) || r.Code != CodeConfigRefused || !strings.Contains(r.Message, "test folder") {
		t.Fatalf("test counter, production folder: %v", err)
	}

	// Restore both change sets: back to the box before any folder.
	for _, cs := range []string{"cs-documents-02", "cs-documents-01"} {
		if _, err := Restore(context.Background(), env, RestoreInput{ChangeSet: cs}); err != nil {
			t.Fatalf("restore %s: %v", cs, err)
		}
	}
	if _, b := readConfig(t, sys.dir); !bytes.Equal(b, beforeBytes) {
		t.Error("restore did not put config.yaml back")
	}
}

func TestDocuments_Refusals(t *testing.T) {
	sys, env, orders := countersBox(t)
	herp := mkdir(t, "herp")
	link := filepath.Join(filepath.Dir(herp), "herp-link")
	if err := os.Symlink(herp, link); err != nil {
		t.Fatal(err)
	}
	file := touch(t, filepath.Join(herp, "a.txt"))
	inData := filepath.Join(sys.dir, "pickup")
	if err := os.MkdirAll(inData, 0o700); err != nil {
		t.Fatal(err)
	}
	folder := func(p string) map[string]*FolderInput {
		return map[string]*FolderInput{"herp": {Kind: "orders", Path: p, NamePattern: "IMOVEIN_{n}.doc", Layout: "legacy"}}
	}
	tests := []struct {
		name string
		in   DocumentsInput
		code string
	}{
		{"nothing", docsIn(func(*DocumentsInput) {}), CodeSpecInvalid},
		{"bad key", docsIn(func(in *DocumentsInput) { in.Folders = map[string]*FolderInput{"herp/x": nil} }), CodeSpecInvalid},
		{"no path", docsIn(func(in *DocumentsInput) { in.Folders = folder("") }), CodeSpecInvalid},
		{"relative path", docsIn(func(in *DocumentsInput) { in.Folders = folder("herp") }), CodeSpecInvalid},
		{"bad pattern", docsIn(func(in *DocumentsInput) {
			in.Folders = map[string]*FolderInput{"herp": {Kind: "orders", Path: herp, NamePattern: "{n}.bat", Layout: "legacy"}}
		}), CodeSpecInvalid},
		{"device name", docsIn(func(in *DocumentsInput) {
			in.Folders = map[string]*FolderInput{"herp": {Kind: "orders", Path: herp, NamePattern: "COM{n}.doc", Layout: "legacy"}}
		}), CodeSpecInvalid},
		{"missing folder", docsIn(func(in *DocumentsInput) { in.Folders = folder(filepath.Join(herp, "nope")) }), CodeFolderMissing},
		{"a file", docsIn(func(in *DocumentsInput) { in.Folders = folder(file) }), CodeFolderMissing},
		{"a link", docsIn(func(in *DocumentsInput) { in.Folders = folder(link) }), CodeFolderReparse},
		{"in the data directory", docsIn(func(in *DocumentsInput) { in.Folders = folder(inData) }), CodeFolderForbidden},
		{"the sendOrderDir", docsIn(func(in *DocumentsInput) { in.Folders = folder(orders) }), CodeConfigRefused},
		{"no layout", docsIn(func(in *DocumentsInput) {
			in.Folders = map[string]*FolderInput{"herp": {Kind: "orders", Path: herp, NamePattern: "IMOVEIN_{n}.doc"}}
		}), CodeConfigRefused},
		{"remove an unknown folder", docsIn(func(in *DocumentsInput) { in.Folders = map[string]*FolderInput{"herp": nil} }), CodeFolderUnknown},
		{"receipts to an unknown folder", docsIn(func(in *DocumentsInput) {
			in.Receipts = &DocumentsReceiptsInput{Folder: strp("nope")}
		}), CodeConfigRefused},
		{"unknown field", DocumentsInput{}, CodeSpecInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.name == "unknown field" {
				out, code := Run(context.Background(), env, []string{"documents", "--spec", "-"},
					strings.NewReader(`{"version":1,"changeSet":"cs-documents-01","folders":{"herp":{"kind":"orders","path":"C:\\x","namePattern":"A{n}.doc","fileName":"x"}}}`))
				if code != ExitRefused || !strings.Contains(string(out), tc.code) {
					t.Fatalf("%d %s", code, out)
				}
				return
			}
			_, err = ApplyDocuments(context.Background(), env, tc.in, false)
			if r := (*Refusal)(nil); !errors.As(err, &r) || r.Code != tc.code {
				t.Fatalf("%v, want %s", err, tc.code)
			}
		})
	}
}

// A folder whose orders are not finished is not removed or moved; the
// receipts' folder and counter do not change while a receipt is reserved.
func TestDocuments_InFlight(t *testing.T) {
	sys, env, _ := countersBox(t)
	herp, kupain := mkdir(t, "herp"), mkdir(t, "kupain")
	if _, err := ApplyDocuments(context.Background(), env, docsIn(func(in *DocumentsInput) {
		in.Folders = map[string]*FolderInput{
			"herp":          {Kind: "orders", Path: herp, NamePattern: "IMOVEIN_{n}.doc", Layout: "legacy"},
			"herp_receipts": {Kind: "receipts", Path: kupain, NamePattern: "KUPAIN_{n}.doc"},
		}
		in.Receipts = &DocumentsReceiptsInput{Folder: strp("herp_receipts"), Counter: strp("receipts")}
	}), false); err != nil {
		t.Fatal(err)
	}
	ix := hasavshevet.OrderIndexDir(sys.dir)
	if err := os.MkdirAll(ix, 0o700); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(ix, "e.json")
	if err := os.WriteFile(entry, []byte(`{"key":"k","number":1000300,"state":"reserved","reservedAt":"x","rule":{"counter":"b2b","folder":"herp","temporary":false,"fileName":"IMOVEIN_1000300.doc"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]DocumentsInput{
		"remove": docsIn(func(in *DocumentsInput) {
			in.ChangeSet = "cs-documents-rm"
			in.Folders = map[string]*FolderInput{"herp": nil}
		}),
		"move": docsIn(func(in *DocumentsInput) {
			in.ChangeSet = "cs-documents-mv"
			in.Folders = map[string]*FolderInput{"herp": {Kind: "orders", Path: mkdir(t, "herp2"), NamePattern: "IMOVEIN_{n}.doc", Layout: "legacy"}}
		}),
	} {
		_, err := ApplyDocuments(context.Background(), env, in, false)
		if r := (*Refusal)(nil); !errors.As(err, &r) || r.Code != CodeFolderInFlight {
			t.Errorf("%s: %v, want %s", name, err, CodeFolderInFlight)
		}
	}
	// Review m5: any other attribute waits too — the pattern decides the
	// "number taken" scan, the layout the record, the test flag whether a
	// production order may be written there.
	for name, f := range map[string]*FolderInput{
		"pattern":          {Kind: "orders", Path: herp, NamePattern: "ORD_{n}.doc", Layout: "legacy"},
		"temporary prefix": {Kind: "orders", Path: herp, NamePattern: "{prefix}IMOVEIN_{n}.doc", TempPrefix: "TMP_", Layout: "legacy"},
		"layout":           {Kind: "orders", Path: herp, NamePattern: "IMOVEIN_{n}.doc", Layout: "official"},
		"test":             {Kind: "orders", Path: herp, NamePattern: "IMOVEIN_{n}.doc", Layout: "legacy", Test: true},
	} {
		_, err := ApplyDocuments(context.Background(), env, docsIn(func(in *DocumentsInput) {
			in.ChangeSet = "cs-documents-pt"
			in.Folders = map[string]*FolderInput{"herp": f}
		}), false)
		if r := (*Refusal)(nil); !errors.As(err, &r) || r.Code != CodeFolderInFlight {
			t.Errorf("%s change with an order in flight: %v, want %s", name, err, CodeFolderInFlight)
		}
	}
	// Set again as it is: nothing changes, nothing waits.
	if _, err := ApplyDocuments(context.Background(), env, docsIn(func(in *DocumentsInput) {
		in.ChangeSet = "cs-documents-same"
		in.Folders = map[string]*FolderInput{"herp": {Kind: "orders", Path: herp, NamePattern: "IMOVEIN_{n}.doc", Layout: "legacy"}}
	}), false); err != nil {
		t.Errorf("the same folder again: %v", err)
	}
	_ = os.Remove(entry)
	if _, err := ApplyDocuments(context.Background(), env, docsIn(func(in *DocumentsInput) {
		in.ChangeSet = "cs-documents-pt2"
		in.Folders = map[string]*FolderInput{"herp": {Kind: "orders", Path: herp, NamePattern: "ORD_{n}.doc", Layout: "legacy"}}
	}), false); err != nil {
		t.Errorf("pattern change with nothing in flight: %v", err)
	}

	rix := filepath.Join(sys.dir, "receipts", "index")
	if err := os.MkdirAll(rix, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rix, "r.json"), []byte(`{"key":"r","number":500001,"state":"reserved"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ApplyDocuments(context.Background(), env, docsIn(func(in *DocumentsInput) {
		in.ChangeSet = "cs-documents-rc"
		in.Receipts = &DocumentsReceiptsInput{Folder: strp("")}
		in.Folders = map[string]*FolderInput{}
	}), false)
	if r := (*Refusal)(nil); !errors.As(err, &r) || r.Code != CodeReceiptsInFlight {
		t.Fatalf("receipts binding with one reserved: %v", err)
	}
	// Review m5: the bound folder's pattern or test flag waits too (a file
	// written under the old name would be written again under the new one).
	_, err = ApplyDocuments(context.Background(), env, docsIn(func(in *DocumentsInput) {
		in.ChangeSet = "cs-documents-rp"
		in.Folders = map[string]*FolderInput{"herp_receipts": {Kind: "receipts", Path: kupain, NamePattern: "KUP_{n}.doc"}}
	}), false)
	if r := (*Refusal)(nil); !errors.As(err, &r) || r.Code != CodeReceiptsInFlight {
		t.Fatalf("the bound folder's pattern with one reserved: %v", err)
	}
	// And the receipts command's sendReceiptDir, when it is the folder in use.
	sys2, fdb2, _ := handInstallBox(t)
	env2 := testEnv(sys2, fdb2)
	cfg, _ := readConfig(t, sys2.dir)
	cfg.SendReceiptDir = mkdir(t, "kupain_old")
	writeConfig(t, sys2.dir, cfg)
	rix2 := filepath.Join(sys2.dir, "receipts", "index")
	if err := os.MkdirAll(rix2, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rix2, "r.json"), []byte(`{"key":"r","number":7,"state":"reserved"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	moved := mkdir(t, "kupain_new")
	_, err = ApplyReceipts(context.Background(), env2, receiptsInput(func(in *ReceiptsInput) { in.SendReceiptDir = &moved }), false)
	if r := (*Refusal)(nil); !errors.As(err, &r) || r.Code != CodeReceiptsInFlight {
		t.Fatalf("receipts moving sendReceiptDir with one reserved: %v", err)
	}
}

// Behind the link a folder is a path link mode takes, outside the system
// folders.
func TestDocuments_LinkModePathRules(t *testing.T) {
	for _, p := range []string{`C:\xampp\htdocs\..\herp`, `\\server\share\herp`, `C:\herp&calc`, `C:\Windows\Temp\herp`} {
		in := docsIn(func(in *DocumentsInput) {
			in.Folders = map[string]*FolderInput{"herp": {Kind: "orders", Path: p, NamePattern: "IMOVEIN_{n}.doc", Layout: "legacy"}}
		})
		if r := in.Validate(true, []string{`C:\Windows`, `C:\Program Files`}); r == nil {
			t.Errorf("%s accepted behind the link", p)
		}
	}
	ok := docsIn(func(in *DocumentsInput) {
		in.Folders = map[string]*FolderInput{"herp": {Kind: "orders", Path: `C:\xampp\htdocs\herp`, NamePattern: "IMOVEIN_{n}.doc", Layout: "legacy"}}
	})
	if r := ok.Validate(true, []string{`C:\Windows`}); r != nil && !strings.Contains(r.Message, "absolute") {
		t.Errorf("a plain local path: %v", r)
	}
}

// Review m6: the system-folder rules compare text, so behind the link a path
// Windows resolves to another folder is refused before they run — a part
// ending in a dot or a space, an 8.3 short name, a stream after the drive.
// Each of these named a system folder and passed before.
func TestDocuments_LinkModeRefusesOtherSpellings(t *testing.T) {
	system := []string{`C:\Windows`, `C:\Program Files`, `C:\ProgramData`}
	for _, p := range []string{
		`C:\Windows.\System32\herp`,
		`C:\ProgramData.\Microsoft\Windows\Start Menu\Programs\StartUp`,
		`C:\PROGRA~1\herp`,
		`C:\Windows::$INDEX_ALLOCATION\System32`,
		`C:\Windows \herp`,
		`C:\xampp\herp.`,
		`C:\xampp\herp:stream`,
	} {
		in := docsIn(func(in *DocumentsInput) {
			in.Folders = map[string]*FolderInput{"herp": {Kind: "orders", Path: p, NamePattern: "IMOVEIN_{n}.doc", Layout: "legacy"}}
		})
		if r := in.Validate(true, system); r == nil {
			t.Errorf("%s accepted behind the link", p)
		}
		if why := linkPathRefusal(p); why == "" {
			t.Errorf("linkPathRefusal(%s) = \"\"", p)
		}
	}
	for _, p := range []string{`C:\xampp\htdocs\herp`, `D:\Hash7\herp_test`, `C:\xampp\htdocs\herp.d\x`} {
		if why := linkPathRefusal(p); why != "" {
			t.Errorf("linkPathRefusal(%s) = %q", p, why)
		}
	}
}
