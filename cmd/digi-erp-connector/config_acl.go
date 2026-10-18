package main

// The GUI's half of the data-directory DACL (internal/platform/dataacl), kept
// free of walk so it is tested on every platform. dataacl does nothing off
// Windows.

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/logger"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
)

// Seams for the test.
var (
	guiProtectDirFn  = dataacl.ProtectDir
	guiProtectFileFn = dataacl.ProtectFile
)

// guiCreatedDataDir is set at the top of main, before ui.log is opened: on a
// first install the GUI creates the data directory at launch, before any save,
// so a save checking for the directory then would always find it.
var guiCreatedDataDir bool

// dataDirMissing reports whether dir does not exist yet — so the save about to
// run is the one that creates it. Any other stat error counts as present.
func dataDirMissing(dir string) bool {
	_, err := os.Stat(dir)
	return errors.Is(err, os.ErrNotExist)
}

// protectSavedConfig gives a freshly saved config.yaml — and the data
// directory, when this save created it — the protected DACL the daemon sets
// at start (SYSTEM, Administrators, this process's account, OWNER RIGHTS read
// only). Without it a first GUI save leaves config.yaml under %ProgramData%'s
// default ACL, readable by every local user, until the service first starts.
//
// Failures are logged and returned as a warning, never as an error: by now
// config.yaml is saved, refusing the save would hide that, and the daemon
// applies the DACL again at its next start — failing closed on the directory.
// A directory that already existed is left to the daemon: re-applying it from
// the GUI would add the operator's own account to it until the next start.
func protectSavedConfig(dataDir, configPath string, createdDir bool, log logger.LoggerService) string {
	var problems []string
	if createdDir {
		if err := guiProtectDirFn(dataDir); err != nil {
			log.Error(fmt.Sprintf("could not set the protected DACL on the data directory %s", dataDir), err)
			problems = append(problems, "the data directory ("+err.Error()+")")
		}
	}
	if err := guiProtectFileFn(configPath); err != nil {
		log.Error(fmt.Sprintf("could not set the protected DACL on %s", configPath), err)
		problems = append(problems, "config.yaml ("+err.Error()+")")
	}
	if len(problems) == 0 {
		return ""
	}
	return "Saved, but could not restrict access to " + strings.Join(problems, " and ") +
		". The service sets it again when it starts; until then other local users may be able to read it."
}
