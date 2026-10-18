package main

// The GUI's half of the data-directory DACL (internal/platform/dataacl), kept
// free of walk so it is tested on every platform. dataacl does nothing off
// Windows.

import (
	"errors"
	"fmt"
	"os"

	"github.com/digitrade-e/digi-erp-connector/internal/logger"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
)

// Seams for the test.
var (
	guiProtectTreeFn = dataacl.ProtectTree
	guiProtectFileFn = dataacl.ProtectFile
	guiFileTrustedFn = dataacl.FileTrusted
	guiMoveAsideFn   = dataacl.MoveAside
)

// dataDirMissing reports whether dir does not exist yet — so the save about to
// run is the one that creates it. Any other stat error counts as present.
func dataDirMissing(dir string) bool {
	_, err := os.Stat(dir)
	return errors.Is(err, os.ErrNotExist)
}

// protectDirBeforeSave protects the data directory (dataacl.ProtectTree)
// before EVERY save writes anything — so secrets\*.bin (DPAPI machine scope:
// anyone who can read them can decrypt them), atomicfile's temps and
// config.yaml are only ever created inside a tree the walk accepted. A
// steady-state walk only reads, and the elevated GUI writes the very DACL the
// daemon does (no ACE of its own), so running it on every save costs nothing
// and closes the window in which a user could plant a link before a save.
//
// It fails closed (R3-2): whatever the walk refuses or fails on — a link, an
// untrusted owner, an access error — the save is refused and nothing is
// written; the error says what to do.
func protectDirBeforeSave(dataDir string, log logger.LoggerService) error {
	err := os.MkdirAll(dataDir, 0o700)
	if err == nil {
		var rep dataacl.Report
		rep, err = guiProtectTreeFn(dataDir, dataacl.TreeOptions{})
		if err == nil && (rep.Reset > 0 || rep.RootSet) {
			log.Warn(fmt.Sprintf("data directory: the protected DACL was (re)applied to the directory=%v and %d entries", rep.RootSet, rep.Reset))
		}
	}
	if err != nil {
		log.Error(fmt.Sprintf("could not protect the data directory %s; the save was refused", dataDir), err)
		return fmt.Errorf("Not saved: the data directory %s could not be protected, so nothing was written (%w). "+
			"See \"The service will not start\" in docs/troubleshooting.md for this message, fix it, and save again", dataDir, err)
	}
	return nil
}

// protectFileAfterSave gives the saved config.yaml the inherited DACL every
// entry of the data directory carries, by handle. A failure is a warning.
func protectFileAfterSave(configPath string, log logger.LoggerService) string {
	if err := guiProtectFileFn(configPath); err != nil {
		log.Error(fmt.Sprintf("could not set the protected DACL on %s", configPath), err)
		return "Saved, but could not restrict access to config.yaml (" + err.Error() +
			"). The service sets it again when it starts; until then other local users may be able to read it."
	}
	return ""
}

// prepareLog decides whether the GUI may append to one of its log files
// (ui.log, server.log): a file that is not exactly ours — another ACL, an
// untrusted owner, a link — is moved aside first, so the GUI never appends to
// a file object someone else may hold; if it cannot be moved, the GUI does not
// log into it. A file that cannot even be checked (the GUI not elevated) is
// left to the open, which then fails the same way.
func prepareLog(path string) bool {
	ok, err := guiFileTrustedFn(path)
	if err != nil || ok {
		return true
	}
	if _, err := guiMoveAsideFn(path); err != nil {
		return false
	}
	return true
}
