package main

// The GUI's half of the data-directory DACL (internal/platform/dataacl), kept
// free of walk so it is tested on every platform. dataacl does nothing off
// Windows.

import (
	"fmt"
	"os"

	"github.com/digitrade-e/digi-erp-connector/internal/logger"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
)

// Seams for the test.
var (
	guiProtectTreeFn = dataacl.ProtectTree
	guiFileTrustedFn = dataacl.FileTrusted
	guiMoveAsideFn   = dataacl.MoveAside
)

// protectDirBeforeSave protects the data directory (dataacl.ProtectTree)
// before EVERY save writes anything — so secrets\*.bin (DPAPI machine scope:
// anyone who can read them can decrypt them), atomicfile's temps and
// config.yaml are only ever created inside a tree the walk accepted. A
// steady-state walk only reads, and the elevated GUI writes the very DACL the
// daemon does (no ACE of its own), so running it on every save costs nothing
// and closes the window in which a user could plant a link before a save.
// The walk also hands what this GUI created to Administrators (R3-5).
//
// It fails closed (R3-2): whatever the walk refuses or fails on — a link, an
// untrusted owner, an access error — the save is refused and nothing is
// written; the error says what to do.
func protectDirBeforeSave(dataDir string, log logger.LoggerService) (dataacl.Report, error) {
	var rep dataacl.Report
	err := os.MkdirAll(dataDir, 0o700)
	if err == nil {
		rep, err = guiProtectTreeFn(dataDir, dataacl.TreeOptions{ClaimForAdministrators: true})
		if err == nil && (rep.Reset > 0 || rep.RootSet) {
			log.Warn(fmt.Sprintf("data directory: the protected DACL was (re)applied to the directory=%v and %d entries", rep.RootSet, rep.Reset))
		}
	}
	if err != nil {
		log.Error(fmt.Sprintf("could not protect the data directory %s; the save was refused", dataDir), err)
		return rep, fmt.Errorf("Not saved: the data directory %s could not be protected, so nothing was written (%w). "+
			"See \"The service will not start\" in docs/troubleshooting.md for this message, fix it, and save again", dataDir, err)
	}
	return rep, nil
}

// protectAfterSave walks the data directory again once the save has written:
// the new config.yaml and secret get the inherited DACL and, created by the
// operator's account, Administrators as owner — which the daemon trusts
// whether or not it can resolve how the operator is an administrator (R3-5).
// A failure is a warning: the save is done, and the daemon re-applies the
// DACL at its next start, failing closed.
func protectAfterSave(dataDir string, log logger.LoggerService) string {
	if _, err := guiProtectTreeFn(dataDir, dataacl.TreeOptions{ClaimForAdministrators: true}); err != nil {
		log.Error(fmt.Sprintf("could not protect the data directory %s after the save", dataDir), err)
		return "Saved, but could not restrict access to what was saved (" + err.Error() +
			"). The service sets it again when it starts — and refuses to start if it cannot."
	}
	return ""
}
