package main

// The GUI's half of the data-directory DACL (internal/platform/dataacl), kept
// free of walk so it is tested on every platform. dataacl does nothing off
// Windows.

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"github.com/digitrade-e/digi-erp-connector/internal/logger"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
)

// Seams for the test.
var (
	guiProtectTreeFn = dataacl.ProtectTree
	guiProtectFileFn = dataacl.ProtectFile
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

// protectSavedConfig gives a freshly saved config.yaml — and the data
// directory, when saveProtectsTheDirectory said so (dataacl.ProtectTree) — the
// protected DACL the daemon sets at start (SYSTEM, Administrators, this
// process's account, OWNER RIGHTS read only). Without it a first GUI save leaves config.yaml under %ProgramData%'s
// default ACL, readable by every local user, until the service first starts.
//
// Failures are logged and returned as a warning, never as an error: by now
// config.yaml is saved, refusing the save would hide that, and the daemon
// applies the DACL again at its next start — failing closed on the directory.
// A directory that already existed is left to the daemon: re-applying it from
// the GUI would add the operator's own account to it until the next start.
func protectSavedConfig(dataDir, configPath string, protectDir bool, log logger.LoggerService) string {
	var problems []string
	if protectDir {
		if rep, err := guiProtectTreeFn(dataDir); err != nil {
			log.Error(fmt.Sprintf("could not set the protected DACL on the data directory %s", dataDir), err)
			problems = append(problems, "the data directory ("+err.Error()+")")
		} else if rep.Reset > 0 {
			log.Warn(fmt.Sprintf("data directory: %d entries carried an ACL of their own; reset to the inherited DACL", rep.Reset))
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
