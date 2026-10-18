package main

import (
	"fmt"

	"github.com/digitrade-e/digi-erp-connector/internal/logger"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
)

// Seams for the test; dataacl is a no-op outside Windows.
var (
	protectDirFn  = dataacl.ProtectDir
	protectFileFn = dataacl.ProtectFile
)

// protectDataDir gives the data directory, then config.yaml, the protected
// DACL (SYSTEM, Administrators, this process's account; nothing inherited).
//
// The directory fails closed: its error stops the daemon, because the receipt
// index and history beneath it hold customers' personal data and config.yaml
// the credentials. config.yaml only logs: it already inherits the directory's
// DACL (every save is a temp file created in the directory and renamed over
// it), so the explicit set is belt and braces, and a file an operator owns and
// locked down by hand must not keep the service down.
func protectDataDir(dataDir, configPath string, log logger.LoggerService) error {
	if err := protectDirFn(dataDir); err != nil {
		log.Error(fmt.Sprintf("could not set the protected DACL on the data directory %s; not starting", dataDir), err)
		return err
	}
	if err := protectFileFn(configPath); err != nil {
		log.Error(fmt.Sprintf("could not set the protected DACL on config.yaml (%s); continuing — it inherits the data directory's", configPath), err)
	}
	return nil
}
