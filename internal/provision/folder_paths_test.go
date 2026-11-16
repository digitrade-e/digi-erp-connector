package provision

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// realisticRules are a Windows Server's FolderRules, as Windows paths
// whatever OS the tests run on.
var realisticRules = FolderRules{
	Dirs: []string{`C:\Windows`, `C:\Program Files`, `C:\Program Files (x86)`, `C:\ProgramData`, `C:\Users`, `C:\Documents and Settings`,
		`C:\Program Files\digi-erp-connector`, `C:\ProgramData\digi-erp-connector`, `D:\digi\connector`},
	Parts: []string{"AppData", "System Volume Information", "$Recycle.Bin"},
}

// Plan 087 review T3: BFL's folders pass; every folder the review named is
// refused — a drive's root, the system folders, all of ProgramData, every
// profile and any AppData, the install folder and the data directory, a
// drive's System Volume Information and $Recycle.Bin, and a folder holding
// one of them.
func TestDocumentFolderRefusal(t *testing.T) {
	for _, ok := range []string{`C:\xampp\htdocs\herp`, `C:\xampp\htdocs\herp\receipts`, `D:\Hash7\orders`, `C:\Hash7\IMOVEIN`} {
		if why := documentFolderRefusal(ok, realisticRules); why != "" {
			t.Errorf("%s refused: %s", ok, why)
		}
	}
	for _, bad := range []string{
		`C:\`, `D:\`, `c:`,
		`C:\Windows\Temp`, `C:\WINDOWS`, `C:\Program Files\Hash7`, `C:\Program Files (x86)\Wizsoft\Hash`,
		`C:\ProgramData`, `C:\ProgramData\SomeVendor\plugins`,
		`C:\Users`, `C:\Users\Public\Documents`, `C:\Users\Default`, `C:\Users\All Users\Microsoft\Windows\Start Menu\Programs\StartUp`,
		`C:\Users\Administrator\AppData\Roaming\Microsoft\Windows\Start Menu\Programs\Startup`, `D:\work\AppData\x`, `D:\work\appdata`,
		`D:\System Volume Information\x`, `C:\$Recycle.Bin\S-1-5-21-1`, `C:\Documents and Settings\Administrator`,
		`C:\Program Files\digi-erp-connector`, `D:\digi`, `D:\digi\connector\logs`,
	} {
		if documentFolderRefusal(bad, realisticRules) == "" {
			t.Errorf("%s accepted", bad)
		}
	}
}

// reparseAlong finds a link anywhere on the way, the folder's own name
// included, and stops at a part that does not exist.
func TestReparseAlong(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir()) // on macOS /var is a link
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(root, "real", "herp")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(root, "real"), link); err != nil {
		t.Skip("no symbolic links here:", err)
	}
	for p, want := range map[string]string{
		filepath.Join(link, "herp"):              link,
		link:                                     link,
		real:                                     "",
		filepath.Join(real, "missing", "nested"): "",
	} {
		if at, err := reparseAlong(p); err != nil || at != want {
			t.Errorf("reparseAlong(%s) = %q, %v; want %q", p, at, err, want)
		}
	}
	if _, err := reparseAlong("herp"); err == nil {
		t.Error("a relative path was walked")
	}
}

// Review T3 in the commands: behind the link a typed folder, the pre-087
// folders a command sets, and every folder-acl target are refused under the
// rules and with a reparse point on the way; folder-acl judges the path the
// handle resolves to too. BFL's folders pass.
func TestFolderPaths_Commands(t *testing.T) {
	sys, env, orders, herp := aclBox(t)
	linkBox(t, sys)
	sys.writers = nil
	sys.parentWriters = nil
	programData := mkdir(t, "ProgramData", "Vendor")
	sys.folderRules = FolderRules{Dirs: []string{filepath.Dir(programData)}, Parts: []string{"AppData"}}
	appData := mkdir(t, "Users", "admin", "AppData", "Roaming")
	receipts := mkdir(t, "herp", "receipts")

	typed := func(p string) DocumentsInput {
		return docsIn(func(in *DocumentsInput) {
			in.Folders = map[string]*FolderInput{"herp2": {Kind: "orders", Path: p, NamePattern: "IMOVEIN_{n}.doc", Layout: "legacy"}}
		})
	}
	for _, p := range []string{programData, appData} {
		_, err := ApplyDocuments(context.Background(), env, typed(p), true)
		wantRefusal(t, err, CodeFolderForbidden)
	}
	sys.reparse = map[string]string{receipts: filepath.Dir(receipts)}
	_, err := ApplyDocuments(context.Background(), env, typed(receipts), true)
	wantRefusal(t, err, CodeFolderReparse)
	sys.reparse = nil
	if _, err := ApplyDocuments(context.Background(), env, typed(receipts), true); err != nil {
		t.Fatalf("herp\\receipts behind the link: %v", err)
	}

	// orders behind the link: sendOrderDir moved into ProgramData, or through a junction.
	_, err = ApplyOrders(context.Background(), env, ordersInput(func(in *OrdersInput) { in.SendOrderDir = &programData }), true)
	wantRefusal(t, err, CodeFolderForbidden)
	moved := mkdir(t, "orders2")
	sys.reparse = map[string]string{moved: moved}
	_, err = ApplyOrders(context.Background(), env, ordersInput(func(in *OrdersInput) { in.SendOrderDir = &moved }), true)
	wantRefusal(t, err, CodeFolderReparse)
	sys.reparse = nil
	// receipts behind the link: sendReceiptDir in an AppData.
	_, err = ApplyReceipts(context.Background(), env, receiptsInput(func(in *ReceiptsInput) { in.SendReceiptDir = &appData }), true)
	wantRefusal(t, err, CodeFolderForbidden)

	// folder-acl: the configured path through a junction; a handle that
	// resolves elsewhere, or into a forbidden folder.
	lock := aclIn("lockdown", func(in *FolderACLInput) { in.Folders = []string{"herp"} })
	sys.reparse = map[string]string{herp: filepath.Dir(herp)}
	_, err = ApplyFolderACL(context.Background(), env, lock, true)
	wantRefusal(t, err, CodeFolderReparse)
	sys.reparse = nil
	sys.finalPath = map[string]string{herp: `\\?\` + filepath.Join(filepath.Dir(herp), "elsewhere")}
	_, err = ApplyFolderACL(context.Background(), env, lock, true)
	wantRefusal(t, err, CodeFolderReparse)
	sys.finalPath = map[string]string{herp: `\\?\` + programData}
	_, err = ApplyFolderACL(context.Background(), env, lock, true)
	wantRefusal(t, err, CodeFolderForbidden)
	sys.finalPath = nil
	if _, err := ApplyFolderACL(context.Background(), env, lock, true); err != nil {
		t.Fatalf("herp: %v", err)
	}
	_ = orders
}
