package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
	"github.com/digitrade-e/digi-erp-connector/internal/provision"
)

// folderLog records warnings and errors.
type folderLog struct {
	mu          sync.Mutex
	warns, errs []string
	quietLogger
}

func (l *folderLog) Warn(msg string) { l.mu.Lock(); l.warns = append(l.warns, msg); l.mu.Unlock() }
func (l *folderLog) Error(msg string, _ error) {
	l.mu.Lock()
	l.errs = append(l.errs, msg)
	l.mu.Unlock()
}

// Plan 087 review M1: the daemon's own check of who can write to the folders
// it writes documents into. Off the link every such folder is a warning;
// behind it a typed folder others can write to — or whose writers cannot be
// read — is refused; the folders that predate plan 087 only warn (v1.0.29
// boxes upgrade without losing orders).
func TestCheckWriteFolders(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{
		ERP: config.ERPHasavshevet, SendOrderDir: filepath.Join(root, "orders"), SendOrderPickupDir: filepath.Join(root, "pickup"),
		SendOrderFormat: config.OrderFormatLegacy,
		Documents: config.DocumentsConfig{
			Folders: map[string]config.FolderConfig{
				"herp":          {Kind: config.DocumentKindOrders, Path: filepath.Join(root, "herp"), NamePattern: "IMOVEIN_{n}.doc", Layout: config.OrderFormatLegacy},
				"herp_test":     {Kind: config.DocumentKindOrders, Path: filepath.Join(root, "herp_test"), NamePattern: "IMOVEIN_{n}.doc", Layout: config.OrderFormatLegacy, Test: true},
				"herp_clean":    {Kind: config.DocumentKindOrders, Path: filepath.Join(root, "clean"), NamePattern: "IMOVEIN_{n}.doc", Layout: config.OrderFormatLegacy},
				"herp_receipts": {Kind: config.DocumentKindReceipts, Path: filepath.Join(root, "kupain"), NamePattern: "KUPAIN_{n}.doc"},
			},
			Receipts: config.DocumentReceiptsConfig{Folder: "herp_receipts"},
		},
	}
	users := []dataacl.Writer{{SID: "S-1-5-32-545", Name: `BUILTIN\Users`}}
	scheduler := dataacl.Writer{SID: "S-1-5-21-1-2-3-1105", Name: `BFL\wizsched`}
	old := folderWritersFn
	t.Cleanup(func() { folderWritersFn = old })
	folderWritersFn = func(dir string) ([]dataacl.Writer, error) {
		switch filepath.Base(dir) {
		case "orders", "pickup", "herp", "kupain":
			return users, nil
		case "herp_test":
			return nil, errors.New("access denied")
		case "clean":
			return []dataacl.Writer{scheduler}, nil // trusted below: never counted
		}
		return []dataacl.Writer{}, nil
	}
	cfg.Documents.TrustedWriters = []config.TrustedWriter{{SID: scheduler.SID, Name: scheduler.Name}}

	log := &folderLog{}
	orders, receipts := checkWriteFolders(cfg, false, log)
	if len(orders) != 0 || receipts != "" || len(log.errs) != 0 {
		t.Fatalf("off the link: refused %v %q, errors %v", orders, receipts, log.errs)
	}
	if len(log.warns) != 5 {
		t.Errorf("off the link: %d warnings, want one per folder others can write to (5): %v", len(log.warns), log.warns)
	}

	log = &folderLog{}
	orders, receipts = checkWriteFolders(cfg, true, log)
	if len(orders) != 2 || !strings.Contains(orders["herp"], `BUILTIN\Users`) || !strings.Contains(orders["herp_test"], "access denied") {
		t.Errorf("behind the link: orders folders refused %v, want herp and herp_test (unreadable: fail closed)", orders)
	}
	if _, ok := orders["herp_clean"]; ok {
		t.Error("a folder only the administrators can write to was refused")
	}
	if !strings.Contains(receipts, `BUILTIN\Users`) {
		t.Errorf("behind the link: the bound receipt folder is not refused: %q", receipts)
	}
	if len(log.errs) != 3 || len(log.warns) != 2 {
		t.Errorf("behind the link: errors %v, warnings %v; want the 3 typed folders refused, sendOrderDir and the pickup folder warned", log.errs, log.warns)
	}
}

// linkedBox reads link.yaml as provision.LinkMode does.
func TestLinkedBoxIsProvisionsLinkMode(t *testing.T) {
	for _, content := range []string{"", "box: bfl-read\n", "pairCode: ABCDE-FGHJK\n", "relay: wss://x/link\n", "box: ''\n",
		"box: bfl-read\nfuture: 1\n", "{not yaml", "admin: false\n"} {
		dir := t.TempDir()
		if content != "" {
			if err := os.WriteFile(filepath.Join(dir, "link.yaml"), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if got, want := linkedBox(dir), provision.LinkMode(dir); got != want {
			t.Errorf("link.yaml %q: linkedBox %v, provision.LinkMode %v", content, got, want)
		}
	}
}
