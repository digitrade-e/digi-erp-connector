package main

// The GUI's half of the data-directory DACL (internal/platform/dataacl), kept
// free of walk so it is tested on every platform. dataacl does nothing off
// Windows.

import (
	"errors"
	"fmt"
	"os"
	"sync/atomic"

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

// dataDirMissingAtLaunch is set at the top of main, before ui.log is opened
// (which creates the data directory): on a launch that found no data directory
// the first save protects it. It is taken once — a later save in the same
// session does not re-apply the directory's DACL, which would add the
// operator's own account to it.
var dataDirMissingAtLaunch atomic.Bool

func takeDataDirMissingAtLaunch() bool { return dataDirMissingAtLaunch.Swap(false) }

// saveProtectsTheDirectory decides, per save and before anything is written,
// whether that save also protects the data directory: when the directory was
// missing at launch (taken once), or when config.yaml does not exist yet — this
// save creates it, so it is a first configuration whatever ui.log did.
func saveProtectsTheDirectory(configPath string) bool {
	atLaunch := takeDataDirMissingAtLaunch()
	_, err := os.Stat(configPath)
	return atLaunch || errors.Is(err, os.ErrNotExist)
}

// dataDirMissing reports whether dir does not exist yet — so the save about to
// run is the one that creates it. Any other stat error counts as present.
func dataDirMissing(dir string) bool {
	_, err := os.Stat(dir)
	return errors.Is(err, os.ErrNotExist)
}

// protectDirBeforeSave protects the data directory (dataacl.ProtectTree)
// before a save that creates config.yaml writes anything — so secrets\*.bin
// (DPAPI machine scope: any local user who can read them can decrypt them)
// and atomicfile's temps are created inside the protected tree, never
// Users-readable. The elevated GUI writes the very DACL the daemon does: no
// ACE of its own, Administrators covers it.
//
// A failure is logged and returned as a warning, never as an error: the save
// goes on, and the daemon applies the DACL again at its next start — failing
// closed on the directory.
func protectDirBeforeSave(dataDir string, log logger.LoggerService) string {
	err := os.MkdirAll(dataDir, 0o700)
	if err == nil {
		var rep dataacl.Report
		rep, err = guiProtectTreeFn(dataDir, dataacl.TreeOptions{})
		if err == nil && rep.Reset > 0 {
			log.Warn(fmt.Sprintf("data directory: %d entries carried an ACL of their own; reset to the inherited DACL", rep.Reset))
		}
	}
	if err != nil {
		log.Error(fmt.Sprintf("could not set the protected DACL on the data directory %s", dataDir), err)
		return "Note: could not restrict access to the data directory (" + err.Error() + "); the service sets it when it starts."
	}
	return ""
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
