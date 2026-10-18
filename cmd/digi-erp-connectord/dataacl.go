package main

import (
	"fmt"

	"github.com/digitrade-e/digi-erp-connector/internal/logger"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
)

// Seams for the tests; dataacl is a no-op outside Windows.
var (
	protectTreeFn   = dataacl.ProtectTree
	protectFileFn   = dataacl.ProtectFile
	newBootstrapLog = logger.NewBootstrap
)

// protectDataDir gives the data directory the protected DACL and resets
// whatever under it carries an ACL of its own (dataacl.ProtectTree). Start
// calls it right after the bootstrap logger, BEFORE config.Load: a first
// install's service start (the installer's sc start) has no config yet, and
// returning on ErrNotFound first would leave the directory — and any
// DPAPI-machine secrets\*.bin already in it — readable by local users.
//
// It fails closed: the receipt index and history beneath hold customers'
// personal data. A failed owner change (running as SYSTEM) is only logged.
func protectDataDir(dataDir string, log logger.LoggerService) error {
	rep, err := protectTreeFn(dataDir)
	if err != nil {
		log.Error(fmt.Sprintf("could not set the protected DACL on the data directory %s; not starting", dataDir), err)
		return err
	}
	if rep.Reset > 0 {
		log.Warn(fmt.Sprintf("data directory: %d of %d entries carried an ACL of their own; reset to the directory's inherited DACL", rep.Reset, rep.Entries))
	}
	if rep.OwnerErr != nil {
		log.Error(fmt.Sprintf("could not make SYSTEM the owner of the data directory %s; continuing", dataDir), rep.OwnerErr)
	}
	return nil
}

// protectConfigFile sets the protected DACL on config.yaml itself, after it
// loaded. It only logs: the file already inherits the directory's DACL (every
// save is a temp file created in the directory and renamed over it), and a
// file an operator locked down by hand must not keep the service down.
func protectConfigFile(configPath string, log logger.LoggerService) {
	if err := protectFileFn(configPath); err != nil {
		log.Error(fmt.Sprintf("could not set the protected DACL on config.yaml (%s); continuing — it inherits the data directory's", configPath), err)
	}
}
