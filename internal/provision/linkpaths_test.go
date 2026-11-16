package provision

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Review m6, the rest (v1.0.32): behind the link an image folder and an
// importer file a command sets are refused with a junction, link or mount
// point on the way, or when they resolve elsewhere than their configured
// path; ones left as they were are not judged again; over WinRM nothing of
// this runs.
func TestLinkedPaths_ImageFoldersAndImporter(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	env := testEnv(sys, fdb)
	linkBox(t, sys)
	images := mkdir(t, "images")
	hash := mkdir(t, "Hash7")
	exe := touch(t, filepath.Join(hash, "has.exe"))
	bat := touch(t, filepath.Join(hash, "kupain.bat"))

	// The image folder.
	options := optionsInput(OptionsSpec{ImageFolders: &[]string{images}})
	sys.reparse = map[string]string{images: filepath.Dir(images)}
	_, err := ApplyOptions(context.Background(), env, options, true)
	wantRefusal(t, err, CodeImageFolderForbidden)
	sys.reparse = nil
	sys.finalPath = map[string]string{images: `\\?\` + filepath.Join(filepath.Dir(images), "elsewhere")}
	_, err = ApplyOptions(context.Background(), env, options, true)
	wantRefusal(t, err, CodeImageFolderForbidden)
	sys.finalPath = map[string]string{images: `\\?\UNC\server\share\images`}
	_, err = ApplyOptions(context.Background(), env, options, true)
	wantRefusal(t, err, CodeImageFolderForbidden)
	sys.finalPath = nil
	if _, err := ApplyOptions(context.Background(), env, options, false); err != nil {
		t.Fatalf("the image folder itself: %v", err)
	}
	// Left as it is, it is not judged again.
	sys.finalPath = map[string]string{images: `\\?\` + filepath.Join(filepath.Dir(images), "elsewhere")}
	if _, err := ApplyOptions(context.Background(), env, options, true); err != nil {
		t.Fatalf("an image folder left as it was: %v", err)
	}
	sys.finalPath = nil

	// has.exe, set by orders.
	orders := ordersInput(func(in *OrdersInput) { in.Importer = &ImporterSpec{Mode: "hasExe", HasExePath: exe} })
	sys.reparse = map[string]string{exe: hash}
	_, err = ApplyOrders(context.Background(), env, orders, true)
	wantRefusal(t, err, CodeImporterNotERP)
	sys.reparse = nil
	sys.finalPath = map[string]string{exe: `\\?\` + filepath.Join(filepath.Dir(hash), "Other", "has.exe")}
	_, err = ApplyOrders(context.Background(), env, orders, true)
	wantRefusal(t, err, CodeImporterNotERP)
	sys.finalPath = nil

	// The receipt BAT, set by receipts.
	kupain := mkdir(t, "kupain")
	receipts := receiptsInput(func(in *ReceiptsInput) { in.SendReceiptDir, in.HasReceiptBatFile = &kupain, &bat })
	sys.finalPath = map[string]string{bat: `\\?\` + filepath.Join(filepath.Dir(hash), "Other", "kupain.bat")}
	_, err = ApplyReceipts(context.Background(), env, receipts, true)
	wantRefusal(t, err, CodeImporterNotERP)
	sys.finalPath = nil
	if _, err := ApplyReceipts(context.Background(), env, receipts, true); err != nil {
		t.Fatalf("the BAT itself: %v", err)
	}

	// Over WinRM none of this runs.
	unlinkBox(t, sys)
	sys.reparse = map[string]string{images: filepath.Dir(images)}
	if _, err := ApplyOptions(context.Background(), env, optionsInput(OptionsSpec{ImageFolders: &[]string{images, mkdir(t, "more")}}), true); err != nil {
		t.Fatalf("over WinRM: %v", err)
	}
}

func unlinkBox(t *testing.T, sys *fakeSys) {
	t.Helper()
	if err := os.Remove(filepath.Join(sys.DataDir(), "link.yaml")); err != nil {
		t.Fatal(err)
	}
}
