package provision

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
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
	herp, herp2, kupain := mkdir(t, "herp"), mkdir(t, "herp2"), mkdir(t, "kupain")
	sys.writers = map[string][]string{herp: {usersWrite}, herp2: {usersWrite}, kupain: {usersWrite}}
	set := func(cs, key, path string, allow bool) DocumentsInput {
		return docsIn(func(in *DocumentsInput) {
			in.ChangeSet, in.AllowWritable = cs, allow
			in.Folders = map[string]*FolderInput{key: {Kind: "orders", Path: path, NamePattern: "IMOVEIN_{n}.doc", Layout: "legacy"}}
		})
	}

	// WinRM: a folder the command adds is refused, unless a human accepted it.
	_, err := ApplyDocuments(context.Background(), env, set("cs-documents-w1", "herp", herp, false), false)
	if r := wantRefusal(t, err, CodeFolderWritable); !strings.Contains(r.Message, usersWrite) || !strings.Contains(r.Fix, "allowWritable") {
		t.Errorf("refusal %+v", r)
	}
	res, err := ApplyDocuments(context.Background(), env, set("cs-documents-w2", "herp", herp, true), false)
	if err != nil || !warned(res, "folder_writable: folder herp") || len(res.WritableFolders) != 1 {
		t.Fatalf("allowWritable over WinRM: %v %+v %v", res.Warnings, res.WritableFolders, err)
	}
	// A receipts folder set earlier (allowWritable) and bound now: newly bound, so checked.
	if _, err := ApplyDocuments(context.Background(), env, docsIn(func(in *DocumentsInput) {
		in.ChangeSet, in.AllowWritable = "cs-documents-w3", true
		in.Folders = map[string]*FolderInput{"herp_receipts": {Kind: "receipts", Path: kupain, NamePattern: "KUPAIN_{n}.doc"}}
	}), false); err != nil {
		t.Fatal(err)
	}
	_, err = ApplyDocuments(context.Background(), env, docsIn(func(in *DocumentsInput) {
		in.ChangeSet = "cs-documents-w4"
		in.Receipts = &DocumentsReceiptsInput{Folder: strp("herp_receipts"), Counter: strp("receipts")}
	}), false)
	wantRefusal(t, err, CodeFolderWritable)

	// Behind the link: a folder the command adds or moves is refused — allowWritable is no way out;
	// one set again where it was is a warning (review R1a).
	linkBox(t, sys)
	_, err = ApplyDocuments(context.Background(), env, set("cs-documents-w5", "herp2", herp2, false), false)
	if r := wantRefusal(t, err, CodeFolderWritable); strings.Contains(r.Fix, "allowWritable") {
		t.Errorf("behind the link the fix offers allowWritable: %+v", r)
	}
	_, err = ApplyDocuments(context.Background(), env, set("cs-documents-w6", "herp", herp2, false), false) // moved
	wantRefusal(t, err, CodeFolderWritable)
	_, err = ApplyDocuments(context.Background(), env, set("cs-documents-w7", "herp2", herp2, true), false)
	wantRefusal(t, err, CodeSpecInvalid)
	res, err = ApplyDocuments(context.Background(), env, docsIn(func(in *DocumentsInput) {
		in.ChangeSet = "cs-documents-w8"
		in.Folders = map[string]*FolderInput{"herp": {Kind: "orders", Path: herp, NamePattern: "ORD_{n}.doc", Layout: "legacy"}} // same path
	}), false)
	if err != nil || !warned(res, "folder_writable: folder herp") {
		t.Fatalf("a folder left where it was: %v %v", res, err)
	}
	// Locked down: accepted, nothing reported.
	sys.writers = nil
	if res, err := ApplyDocuments(context.Background(), env, set("cs-documents-w9", "herp2", herp2, false), false); err != nil || len(res.WritableFolders) != 0 {
		t.Fatalf("a folder only the administrators can write to: %+v %v", res, err)
	}
}

// Review R1a, the review's case: on a linked box whose pickup and receipt
// folders the ERP's scheduler writes into, an orders command changing only
// sendOrderFormat and a receipts command changing only receiptCardNames go
// through — the folders are reported, not refused; setting or moving a
// writable folder is still refused.
func TestReceiptsAndOrders_FolderWriters(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	env := testEnv(sys, fdb)
	kupain, pickup, pickup2 := mkdir(t, "kupain"), mkdir(t, "herp"), mkdir(t, "herp2")
	const scheduler = `BFL\wizsched`

	// WinRM: setting a writable receipt folder is refused, allowWritable accepts it.
	sys.writers = map[string][]string{kupain: {scheduler}}
	receipts := func(cs string, allow bool) ReceiptsInput {
		return receiptsInput(func(in *ReceiptsInput) { in.ChangeSet, in.SendReceiptDir, in.AllowWritable = cs, &kupain, allow })
	}
	_, err := ApplyReceipts(context.Background(), env, receipts("cs-receipts-w1", false), false)
	wantRefusal(t, err, CodeFolderWritable)
	if res, err := ApplyReceipts(context.Background(), env, receipts("cs-receipts-w2", true), false); err != nil || !warned(res, "folder_writable: the receipt folder") {
		t.Fatalf("allowWritable: %v %v", res, err)
	}
	// Setting a writable pickup folder is refused; a writable sendOrderDir the command leaves is reported.
	sys.writers = map[string][]string{orders: {scheduler}, pickup: {scheduler}, kupain: {scheduler}}
	legacy := "legacy"
	_, err = ApplyOrders(context.Background(), env, OrdersInput{Version: 1, ChangeSet: "cs-orders-w1", SendOrderPickupDir: &pickup, SendOrderFormat: &legacy}, false)
	if r := wantRefusal(t, err, CodeFolderWritable); !strings.Contains(r.Message, "sendOrderPickupDir") {
		t.Errorf("refusal %+v", r)
	}
	res, err := ApplyOrders(context.Background(), env, OrdersInput{Version: 1, ChangeSet: "cs-orders-w2", SendOrderPickupDir: &pickup, SendOrderFormat: &legacy, AllowWritable: true}, false)
	if err != nil || !warned(res, "folder_writable: sendOrderPickupDir") || !warned(res, "folder_writable: sendOrderDir") {
		t.Fatalf("allowWritable: %v %v", res, err)
	}

	// Behind the link (BFL): changes that touch no folder go through, reported.
	linkBox(t, sys)
	official := "official"
	res, err = ApplyOrders(context.Background(), env, OrdersInput{Version: 1, ChangeSet: "cs-orders-w3", SendOrderFormat: &official}, false)
	if err != nil || len(res.WritableFolders) != 3 {
		t.Fatalf("an orders command changing only the layout: %v %+v %v", res.Warnings, res.WritableFolders, err)
	}
	res, err = ApplyReceipts(context.Background(), env, receiptsInput(func(in *ReceiptsInput) {
		in.ChangeSet, in.ReceiptCardNames = "cs-receipts-w3", &config.ReceiptCardNamesConfig{}
	}), false)
	if err != nil || !warned(res, "folder_writable: sendReceiptDir") {
		t.Fatalf("a receipts command changing only the card names: %v %v", res, err)
	}
	// Moving the pickup folder to another writable one is refused; allowWritable is refused with it.
	sys.writers[pickup2] = []string{scheduler}
	in := OrdersInput{Version: 1, ChangeSet: "cs-orders-w4", SendOrderPickupDir: &pickup2}
	_, err = ApplyOrders(context.Background(), env, in, false)
	wantRefusal(t, err, CodeFolderWritable)
	in.AllowWritable = true
	_, err = ApplyOrders(context.Background(), env, in, false)
	wantRefusal(t, err, CodeSpecInvalid)

	// state lists them all.
	st, err := State(context.Background(), env, StateInput{})
	if err != nil || len(st.WritableFolders) != 3 {
		t.Fatalf("state writableFolders %+v %v", st.WritableFolders, err)
	}
}

// apply only warns (review R1a).
func TestApply_WritableFoldersAreAWarning(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	sys.writers = map[string][]string{orders: {usersWrite}}
	spec := handInstallSpec(orders)
	res, err := ApplyCheck(context.Background(), testEnv(sys, fdb), spec)
	if err != nil || len(res.WritableFolders) != 1 || !warned(res, "folder_writable: sendOrderDir") {
		t.Fatalf("apply --check: %+v %v", res, err)
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

// Review R1b: BFL's scheduler writes into herp by design. documents names it
// in trustedWriters — resolved on the box, shown by --check, stored by SID —
// and then the folder is accepted behind the link, and state shows the
// account as trusted. A name that does not resolve is writer_unknown; a
// group, an alias or a well-known SID is writer_too_broad.
func TestDocuments_TrustedWriters(t *testing.T) {
	sys, env, _ := countersBox(t)
	const sched = `BFL\wizsched`
	sys.accounts = map[string]dataacl.Account{
		strings.ToLower(sched):         {SID: "S-1-5-21-1-2-3-1105", Name: sched, Type: dataacl.AccountUser},
		strings.ToLower(`BFL\erp`):     {SID: "S-1-5-21-1-2-3-1106", Name: `BFL\erp`, Type: dataacl.AccountUser},
		`bfl\domain users`:             {SID: "S-1-5-21-1-2-3-513", Name: `BFL\Domain Users`, Type: "group"},
		`bfl\erp operators`:            {SID: "S-1-5-21-1-2-3-1200", Name: `BFL\ERP Operators`, Type: "group"},
		"everyone":                     {SID: "S-1-1-0", Name: "Everyone", Type: "wellKnownGroup"},
		`builtin\users`:                {SID: "S-1-5-32-545", Name: `BUILTIN\Users`, Type: "alias"},
		`nt authority\network service`: {SID: "S-1-5-20", Name: `NT AUTHORITY\NETWORK SERVICE`, Type: "wellKnownGroup"},
		`bfl\host$`:                    {SID: "S-1-5-21-1-2-3-1300", Name: `BFL\HOST$`, Type: "computer"},
	}
	herp := mkdir(t, "herp")
	sys.writers = map[string][]string{herp: {sched}}
	linkBox(t, sys)
	setHerp := func(cs string) DocumentsInput {
		return docsIn(func(in *DocumentsInput) {
			in.ChangeSet = cs
			in.Folders = map[string]*FolderInput{"herp": {Kind: "orders", Path: herp, NamePattern: "IMOVEIN_{n}.doc", Layout: "legacy"}}
		})
	}
	_, err := ApplyDocuments(context.Background(), env, setHerp("cs-trust-01"), false)
	wantRefusal(t, err, CodeFolderWritable)

	trust := func(cs string, names ...string) DocumentsInput {
		return docsIn(func(in *DocumentsInput) { in.ChangeSet, in.TrustedWriters = cs, &names })
	}
	chk, err := ApplyDocuments(context.Background(), env, trust("cs-trust-02", sched), true)
	if err != nil || chk.Check == nil || len(chk.Check.TrustedWriters) != 1 {
		t.Fatalf("--check: %+v %v", chk, err)
	}
	if got := chk.Check.TrustedWriters[0]; got.SID != "S-1-5-21-1-2-3-1105" || got.Type != "user" || got.Resolved != sched {
		t.Errorf("--check shows %+v", got)
	}
	if _, err := ApplyDocuments(context.Background(), env, trust("cs-trust-02", sched), false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := readConfig(t, sys.dir)
	if len(cfg.Documents.TrustedWriters) != 1 || cfg.Documents.TrustedWriters[0] != (config.TrustedWriter{SID: "S-1-5-21-1-2-3-1105", Name: sched}) {
		t.Fatalf("stored %+v", cfg.Documents.TrustedWriters)
	}
	if _, err := ApplyDocuments(context.Background(), env, setHerp("cs-trust-03"), false); err != nil {
		t.Fatalf("herp with its scheduler trusted: %v", err)
	}
	st, err := State(context.Background(), env, StateInput{})
	if err != nil || !slices.ContainsFunc(st.WritableFolders, func(w StateWritableFolder) bool {
		return w.Name == "folder herp" && len(w.Writers) == 0 && slices.Equal(w.Trusted, []string{sched})
	}) {
		t.Fatalf("state writableFolders %+v %v", st.WritableFolders, err)
	}

	for name, tc := range map[string]struct {
		names []string
		code  string
	}{
		"unknown":         {[]string{`BFL\nobody`}, CodeWriterUnknown},
		"a domain group":  {[]string{`BFL\Domain Users`}, CodeWriterTooBroad},
		"a created group": {[]string{`BFL\ERP Operators`}, CodeWriterTooBroad},
		"Everyone":        {[]string{"Everyone"}, CodeWriterTooBroad},
		"Users":           {[]string{`BUILTIN\Users`}, CodeWriterTooBroad},
		"NETWORK SERVICE": {[]string{`NT AUTHORITY\NETWORK SERVICE`}, CodeWriterTooBroad},
		"a computer":      {[]string{`BFL\HOST$`}, CodeWriterTooBroad},
		"twice":           {[]string{sched, strings.ToUpper(sched)}, CodeSpecInvalid},
		"nine":            {[]string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}, CodeSpecInvalid},
	} {
		_, err := ApplyDocuments(context.Background(), env, trust("cs-trust-x1", tc.names...), false)
		if r := (*Refusal)(nil); !errors.As(err, &r) || r.Code != tc.code {
			t.Errorf("%s: %v, want %s", name, err, tc.code)
		}
	}
	// An ordinary documents change: restore puts the list back.
	if _, err := Restore(context.Background(), env, RestoreInput{ChangeSet: "cs-trust-03"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background(), env, RestoreInput{ChangeSet: "cs-trust-02"}); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := readConfig(t, sys.dir); len(cfg.Documents.TrustedWriters) != 0 {
		t.Errorf("restore kept %+v", cfg.Documents.TrustedWriters)
	}
}
