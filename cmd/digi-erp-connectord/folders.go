package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/logger"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
)

// folderWritersFn reads who besides the administrators can write to a folder
// (dataacl.FolderWriters; tests replace it).
var folderWritersFn = dataacl.FolderWriters

// checkWriteFolders is the daemon's own check of who can write to the folders
// it writes documents into (plan 087 review M1), made once at its start, so a
// config.yaml edited by hand cannot bypass the provision commands' check
// (folder_writable). Every folder others can write to is logged. Behind the
// link a typed folder (the documents command's) others can write to is
// refused: orders whose rule names it, and receipts when it is the bound
// receipt folder, answer 503 FOLDER_WRITABLE with nothing reserved, until the
// folder is locked down and the service restarted. A folder whose writers
// cannot be read is refused the same way (fail closed). The folders that
// predate plan 087 — sendOrderDir, the pickup folder, sendReceiptDir — are
// only warned about: refusing them would stop the orders of every box
// upgraded from v1.0.29 whose folders were never locked down; the orders and
// receipts commands refuse them behind the link. Queries are never held
// hostage to a folder.
func checkWriteFolders(cfg config.Config, linked bool, log logger.LoggerService) (orders map[string]string, receipts string) {
	orders = map[string]string{}
	for _, f := range cfg.WriteFolders() {
		who, err := folderWritersFn(f.Path)
		var why string
		switch {
		case err != nil:
			why = fmt.Sprintf("who can write to it could not be read (%v)", err)
		case len(who) > 0:
			why = "it can be written by " + strings.Join(who, ", ")
		default:
			continue
		}
		if !linked || !f.Typed {
			log.Warn(fmt.Sprintf("FOLDER WRITABLE: %s (%s): %s. Whoever can add a file there can pre-create the next document's name, "+
				"drop a document the importer takes into the books, or plant what an importer running there loads; let only the "+
				"administrators write to it (docs/troubleshooting.md § A folder others can write to).", f.Name, f.Path, why))
			continue
		}
		log.Error(fmt.Sprintf("FOLDER REFUSED: %s (%s): %s, and this box is behind the link: nothing is written there (503 FOLDER_WRITABLE) "+
			"until only the administrators can write to it and the service is restarted (docs/troubleshooting.md § A folder others can write to).",
			f.Name, f.Path, why), nil)
		if f.Receipts {
			receipts = why
		}
		if kind := cfg.Documents.Folders[f.Key].Kind; kind == config.DocumentKindOrders {
			orders[f.Key] = why
		}
	}
	return orders, receipts
}

// linkedBox reports whether the box is behind the link: link.yaml in the
// data directory names a box or a pairing code — provision.LinkMode's rule
// (internal/link's file, read here without the link package).
func linkedBox(dataDir string) bool {
	b, err := os.ReadFile(filepath.Join(dataDir, "link.yaml"))
	if err != nil {
		return false
	}
	var st struct {
		Box      string `yaml:"box"`
		PairCode string `yaml:"pairCode"`
	}
	if yaml.Unmarshal(b, &st) != nil {
		return false
	}
	return st.Box != "" || st.PairCode != ""
}
