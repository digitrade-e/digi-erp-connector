package provision

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
)

// Plan 087 review M1: who can write to a folder the connector writes
// documents into. Over WinRM a folder others can write to is refused unless
// allowWritable (then a warning); behind the link it is refused, and
// allowWritable with it.

const usersWrite = `BUILTIN\Users`

func warned(res *ApplyResult, prefix string) bool {
	return slices.ContainsFunc(res.Warnings, func(w string) bool { return strings.HasPrefix(w, prefix) })
}

func TestDocuments_FolderWriters(t *testing.T) {
	sys, env, _ := countersBox(t)
	herp, kupain := mkdir(t, "herp"), mkdir(t, "kupain")
	sys.writers = map[string][]string{herp: {usersWrite}, kupain: {usersWrite}}
	set := func(cs string, allow bool) DocumentsInput {
		return docsIn(func(in *DocumentsInput) {
			in.ChangeSet, in.AllowWritable = cs, allow
			in.Folders = map[string]*FolderInput{"herp": {Kind: "orders", Path: herp, NamePattern: "IMOVEIN_{n}.doc", Layout: "legacy"}}
		})
	}

	// WinRM: refused, unless a human accepted it.
	_, err := ApplyDocuments(context.Background(), env, set("cs-documents-w1", false), false)
	if r := wantRefusal(t, err, CodeFolderWritable); !strings.Contains(r.Message, usersWrite) || !strings.Contains(r.Fix, "allowWritable") {
		t.Errorf("refusal %+v", r)
	}
	res, err := ApplyDocuments(context.Background(), env, set("cs-documents-w2", true), false)
	if err != nil || !warned(res, "folder_writable: folder herp") {
		t.Fatalf("allowWritable over WinRM: %v %v", res.Warnings, err)
	}
	// A receipts folder set earlier (allowWritable) and bound now: the binding checks it too.
	if _, err := ApplyDocuments(context.Background(), env, docsIn(func(in *DocumentsInput) {
		in.ChangeSet, in.AllowWritable = "cs-documents-w3", true
		in.Folders = map[string]*FolderInput{"herp_receipts": {Kind: "receipts", Path: kupain, NamePattern: "KUPAIN_{n}.doc"}}
	}), false); err != nil {
		t.Fatal(err)
	}
	bind := docsIn(func(in *DocumentsInput) {
		in.ChangeSet = "cs-documents-w4"
		in.Receipts = &DocumentsReceiptsInput{Folder: strp("herp_receipts"), Counter: strp("receipts")}
	})
	_, err = ApplyDocuments(context.Background(), env, bind, false)
	wantRefusal(t, err, CodeFolderWritable)

	// Behind the link: refused, and allowWritable is no way out.
	linkBox(t, sys)
	_, err = ApplyDocuments(context.Background(), env, set("cs-documents-w5", false), false)
	if r := wantRefusal(t, err, CodeFolderWritable); strings.Contains(r.Fix, "allowWritable") {
		t.Errorf("behind the link the fix offers allowWritable: %+v", r)
	}
	_, err = ApplyDocuments(context.Background(), env, set("cs-documents-w6", true), false)
	wantRefusal(t, err, CodeSpecInvalid)
	// Locked down: accepted.
	sys.writers = nil
	if _, err := ApplyDocuments(context.Background(), env, set("cs-documents-w7", false), false); err != nil {
		t.Fatalf("a folder only the administrators can write to: %v", err)
	}
}

func TestReceiptsAndOrders_FolderWriters(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	env := testEnv(sys, fdb)
	kupain, pickup := mkdir(t, "kupain"), mkdir(t, "herp")
	sys.writers = map[string][]string{kupain: {usersWrite}}

	receipts := func(cs string, allow bool) ReceiptsInput {
		return receiptsInput(func(in *ReceiptsInput) { in.ChangeSet, in.SendReceiptDir, in.AllowWritable = cs, &kupain, allow })
	}
	_, err := ApplyReceipts(context.Background(), env, receipts("cs-receipts-w1", false), false)
	wantRefusal(t, err, CodeFolderWritable)
	res, err := ApplyReceipts(context.Background(), env, receipts("cs-receipts-w2", true), false)
	if err != nil || !warned(res, "folder_writable: the receipt folder") {
		t.Fatalf("allowWritable: %v %v", res.Warnings, err)
	}

	// sendOrderDir and the pickup folder.
	sys.writers = map[string][]string{orders: {usersWrite}}
	format := "legacy"
	ordersIn := func(cs string, allow bool) OrdersInput {
		return OrdersInput{Version: 1, ChangeSet: cs, SendOrderPickupDir: &pickup, SendOrderFormat: &format, AllowWritable: allow}
	}
	_, err = ApplyOrders(context.Background(), env, ordersIn("cs-orders-w1", false), false)
	if r := wantRefusal(t, err, CodeFolderWritable); !strings.Contains(r.Message, "sendOrderDir") {
		t.Errorf("refusal %+v", r)
	}
	sys.writers = map[string][]string{pickup: {usersWrite}}
	_, err = ApplyOrders(context.Background(), env, ordersIn("cs-orders-w2", false), false)
	if r := wantRefusal(t, err, CodeFolderWritable); !strings.Contains(r.Message, "sendOrderPickupDir") {
		t.Errorf("refusal %+v", r)
	}
	res, err = ApplyOrders(context.Background(), env, ordersIn("cs-orders-w3", true), false)
	if err != nil || !warned(res, "folder_writable: sendOrderPickupDir") {
		t.Fatalf("allowWritable: %v %v", res.Warnings, err)
	}

	// Behind the link: refused; allowWritable refused with it.
	linkBox(t, sys)
	format2 := "official"
	in := OrdersInput{Version: 1, ChangeSet: "cs-orders-w4", SendOrderFormat: &format2}
	_, err = ApplyOrders(context.Background(), env, in, false)
	wantRefusal(t, err, CodeFolderWritable)
	in.AllowWritable = true
	_, err = ApplyOrders(context.Background(), env, in, false)
	wantRefusal(t, err, CodeSpecInvalid)

	// state lists them.
	st, err := State(context.Background(), env, StateInput{})
	if err != nil || len(st.WritableFolders) != 1 || st.WritableFolders[0].Name != "sendOrderPickupDir" ||
		!slices.Equal(st.WritableFolders[0].Writers, []string{usersWrite}) {
		t.Fatalf("state writableFolders %+v %v", st.WritableFolders, err)
	}
}

// Review R2: state lists the unfinished orders whose frozen test flag no
// longer matches their folder.
func TestState_FolderTestChanged(t *testing.T) {
	sys, env, _ := countersBox(t)
	cfg, _ := readConfig(t, sys.dir)
	cfg.Documents.Folders = map[string]config.FolderConfig{
		"herp_test": {Kind: "orders", Path: mkdir(t, "herp_test"), NamePattern: "ORD_{n}.doc", Layout: "official"}, // test: false now
	}
	writeConfig(t, sys.dir, cfg)
	ix := hasavshevet.OrderIndexDir(sys.dir)
	if err := os.MkdirAll(ix, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ix, "t.json"), []byte(`{"key":"t","number":9000000,"state":"retryable","reservedAt":"x","rule":{"counter":"b2b_test","folder":"herp_test","temporary":false,"fileName":"ORD_9000000.doc","counterTest":true,"folderTest":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := State(context.Background(), env, StateInput{})
	if err != nil || len(st.Orders.FolderTestChanged) != 1 || st.Orders.FolderTestChanged[0] != 9_000_000 {
		t.Fatalf("state orders.folderTestChanged = %v, %v", st.Orders.FolderTestChanged, err)
	}
}
