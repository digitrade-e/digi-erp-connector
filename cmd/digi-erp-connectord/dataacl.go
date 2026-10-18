package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/logger"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
)

// Seams for the tests; dataacl's protection is a no-op outside Windows.
var (
	protectTreeFn   = dataacl.ProtectTree
	protectFileFn   = dataacl.ProtectFile
	moveAsideFn     = dataacl.MoveAside
	newBootstrapLog = logger.NewBootstrap
)

// ensureDataDir creates the data directory on Windows when it is missing, so
// it is protected before anything is written in it — and owned by the
// service, not by whoever happens to create it first. (On Linux nothing is
// protected, and the bootstrap logger creates it as before.)
func ensureDataDir(dataDir string) error {
	if !isWindows {
		return nil
	}
	return os.MkdirAll(dataDir, 0o700)
}

// protectDataDir gives the data directory the protected DACL and everything
// under it the inherited one (dataacl.ProtectTree). Start calls it before it
// opens server.log and before config.Load: a first install's service start
// (the installer's sc start) has no config yet, and secrets\*.bin may already
// be there. progress (may be nil) is the service's start checkpoint.
//
// It fails closed: the receipt index and history beneath hold customers'
// personal data. A failed owner change (running as SYSTEM) is only logged.
func protectDataDir(dataDir string, progress func(), log logger.LoggerService) (dataacl.Report, error) {
	rep, err := protectTreeFn(dataDir, dataacl.TreeOptions{Progress: progress})
	if err != nil {
		log.Error(fmt.Sprintf("could not set the protected DACL on the data directory %s; not starting", dataDir), err)
		return rep, err
	}
	if rep.Reset > 0 || rep.RootSet {
		log.Warn(fmt.Sprintf("data directory: the protected DACL was (re)applied to the directory=%v and %d of %d entries", rep.RootSet, rep.Reset, rep.Entries))
	}
	if rep.OwnerErr != nil {
		log.Error(fmt.Sprintf("could not make SYSTEM the owner of the data directory %s; continuing", dataDir), rep.OwnerErr)
	}
	if rep.AdminsErr != nil {
		log.Warn(fmt.Sprintf("data directory: could not list the Administrators members (%v); only the built-in owners were trusted", rep.AdminsErr))
	}
	return rep, nil
}

// logNeedsFreshFile reports whether the walk had to rewrite logPath's DACL:
// then the file was not ours as it stood, and someone who could read it
// before may still hold it open — appending receipt numbers and importer
// output to that same file object would leak them. Start moves it aside.
func logNeedsFreshFile(logPath string, rep dataacl.Report) bool {
	for _, p := range rep.ResetPaths {
		if p == logPath || (isWindows && strings.EqualFold(p, logPath)) {
			return true
		}
	}
	return false
}

// protectConfigFile gives config.yaml the inherited DACL after it loaded (the
// walk already did; this covers a config written between the walk and the
// load). It only logs: a file an operator locked down by hand must not keep
// the service down.
func protectConfigFile(configPath string, log logger.LoggerService) {
	if err := protectFileFn(configPath); err != nil {
		log.Error(fmt.Sprintf("could not set the protected DACL on config.yaml (%s); continuing", configPath), err)
	}
}
