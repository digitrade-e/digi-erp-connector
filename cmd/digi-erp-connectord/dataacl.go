package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/logger"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/atomicfile"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/autostart"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/paths"
)

// Seams for the tests; dataacl's protection is a no-op outside Windows.
var (
	protectTreeFn   = dataacl.ProtectTree
	protectFileFn   = dataacl.ProtectFile
	moveAsideFn     = dataacl.MoveAside
	fileTrustedFn   = dataacl.FileTrusted
	fileIDFn        = dataacl.FileID
	setLogPathFn    = logger.SetFilePath
	newBootstrapLog = logger.NewBootstrap
	reportRefusalFn = reportRefusal
	ensureDataDirFn = ensureDataDir
)

// startRefused is Start's error when the data directory could not be
// created or protected: the service exits with ExitDataDirRefused.
type startRefused struct{ err error }

func (e *startRefused) Error() string    { return e.err.Error() }
func (e *startRefused) Unwrap() error    { return e.err }
func (e *startRefused) ExitCode() uint32 { return autostart.ExitDataDirRefused }

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
		log.Warn(fmt.Sprintf("data directory: could not list the Administrators members (%v); each owner's membership was resolved on its own instead", rep.AdminsErr))
	}
	if rep.Claimed > 0 {
		log.Info(fmt.Sprintf("data directory: %d entries owned by administrators were handed to BUILTIN\\Administrators", rep.Claimed))
	}
	if rep.ClaimErr != nil {
		log.Warn(fmt.Sprintf("data directory: could not hand an entry to Administrators (%v); its owner is still trusted, and will be resolved again next start", rep.ClaimErr))
	}
	return rep, nil
}

// logIDName is the marker beside server.log: the file ID (dataacl.FileID) of
// the server.log object the daemon last opened fresh.
const logIDName = ".server-log-id"

// logNeedsFreshFile decides from the file itself (R3-4) whether server.log
// gets a fresh file object before the daemon appends to it: unless it is
// exactly ours (trusted), is the very object a previous start created fresh
// (id == marker) and this run's walk did not have to reset it, it is moved
// aside. A start that failed to move it aside therefore tries again the next
// time, whatever the walk did meanwhile.
func logNeedsFreshFile(exists, trusted, resetThisRun bool, id, marker string) bool {
	if !exists {
		return false
	}
	return !trusted || resetThisRun || id == "" || id != marker
}

// freshServerLog runs after the walk and before the bootstrap logger opens
// server.log (Windows only). A server.log that is not provably ours is moved
// aside, retrying a few times; if it still cannot be moved (a program holds
// it), the daemon logs to a new, uniquely named file instead of refusing —
// an order node must not stay down over a log — and tries again next start.
// It reports whether the marker should be written once the log is open.
func freshServerLog(dataDir string, rep dataacl.Report, log logger.LoggerService) (markAfterOpen bool) {
	if !isWindows {
		return false
	}
	logPath, err := paths.LoggerFilePath()
	if err != nil {
		return false
	}
	_, statErr := os.Lstat(logPath)
	exists := statErr == nil
	trusted, _ := fileTrustedFn(logPath)
	id, _ := fileIDFn(logPath)
	marker, _ := os.ReadFile(filepath.Join(dataDir, logIDName))
	reset := false
	for _, p := range rep.ResetPaths {
		if strings.EqualFold(p, logPath) {
			reset = true
		}
	}
	if !logNeedsFreshFile(exists, trusted, reset, id, strings.TrimSpace(string(marker))) {
		return !exists
	}
	var to string
	for attempt := 0; attempt < 4; attempt++ {
		if to, err = moveAsideFn(logPath); err == nil {
			log.Warn(fmt.Sprintf("server.log was not provably this service's own file; moved it to %s and started a fresh one", to))
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	alt := filepath.Join(dataDir, "server-"+time.Now().UTC().Format("20060102T150405Z")+".log")
	setLogPathFn(alt)
	log.Error(fmt.Sprintf("server.log is not provably this service's own file and could not be moved aside (a program holds it open?); logging to %s this time — close that program and restart the service", alt), err)
	return false
}

// markServerLog records the file ID of the server.log just opened fresh.
func markServerLog(dataDir string, log logger.LoggerService) {
	logPath, err := paths.LoggerFilePath()
	if err != nil {
		return
	}
	id, err := fileIDFn(logPath)
	if err == nil {
		err = atomicfile.Write(filepath.Join(dataDir, logIDName), []byte(id+"\n"), 0o600)
	}
	if err != nil {
		log.Warn(fmt.Sprintf("could not record server.log's identity (%v); it will be moved aside again at the next start", err))
	}
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
