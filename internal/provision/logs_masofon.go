package provision

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
)

// The Masofon sources of the logs command (research/reverse-engineering/hasavshevet-run-logs.md): what Hashavshevet's
// importer wrote about the connector's own BAT, read where config.yaml and masofon.ini say — never a path the caller
// names. They are the ERP's text (Windows-1255), outside the data directory: each file is read only when it is a
// regular file (no link is followed), its tail only, every line redacted and measured like the connector's own.
//
//	masofon         the BAT's run logs (<bat>.ok|err.DMY-….LOG, newest last, and a run's unfinished <bat>.err) in
//	                hasRunLogDir, else masofon.ini's ErrDataPath; each line as "<file name>| <line>", timed by its run
//	masofon-errors  <has.exe folder>\DataImpExpLogs\WizImportError*.log: the importer's system errors and the refusals it
//	                logs there instead (a second run, a reduce) — lines tagged with another profile left out
const (
	LogSourceMasofon       = "masofon"
	LogSourceMasofonErrors = "masofon-errors"
	// maxMasofonFiles bounds the run logs one answer reads (each is a run; the budgets cut first).
	maxMasofonFiles = 200
	// maxMasofonFileBytes bounds what is read of one file (a run log is ~1 KiB; WizImportError grows for years).
	maxMasofonFileBytes int64 = 1 << 20
)

func isMasofonSource(s string) bool { return s == LogSourceMasofon || s == LogSourceMasofonErrors }

// masofonLogs fills res from a Masofon source, newest lines first into sel.
func masofonLogs(env Env, in LogsInput, since time.Time, sel *lineSelector, res *LogsResult) error {
	b, err := os.ReadFile(filepath.Join(env.Sys.DataDir(), configFile))
	if err != nil {
		return refuse(CodeLogUnreadable, "provision the connector first", "config.yaml cannot be read: %v", err)
	}
	cfg, err := config.Parse(b)
	if err != nil {
		return refuse(CodeLogUnreadable, "fix config.yaml", "config.yaml does not parse: %v", err)
	}
	bat := strings.TrimSpace(cfg.HasBatFile)
	if cfg.ERP != config.ERPHasavshevet || bat == "" {
		return refuse(CodeLogUnreadable, "the Masofon logs are the connector's own BAT's: set the order importer to a BAT (orders, importer mode bat)",
			"no Masofon BAT is configured (hasBatFile)")
	}
	dirs := hasavshevet.ResolveRunLogDirs(cfg)
	if in.Source == LogSourceMasofon {
		if dirs.LogDir == "" {
			return refuse(CodeLogUnreadable, "set hasRunLogDir (orders) to Masofon2's error-log folder",
				"no run-log folder: masofon.ini (%s) gives no ErrDataPath and hasRunLogDir is not set", dirs.INI)
		}
		files, err := hasavshevet.RunLogFiles(dirs.LogDir, bat, since, maxMasofonFiles)
		if err != nil {
			return refuse(CodeLogUnreadable, "check that the service can read the folder", "%s cannot be listed: %v", dirs.LogDir, err)
		}
		for i := len(files) - 1; i >= 0 && !sel.stopped; i-- {
			f := files[i]
			res.Files = append([]string{f.Path}, res.Files...)
			text, err := hasavshevet.ReadMasofonText(f.Path, maxMasofonFileBytes)
			if err != nil {
				continue // moved to .BAK, rotated, or not a plain file: the next run's log is there
			}
			lines := splitLines(text)
			for j := len(lines) - 1; j >= 0; j-- {
				if !sel.take(filepath.Base(f.Path)+"| "+lines[j], f.At) {
					break
				}
			}
		}
		res.Truncated = sel.truncated
		return nil
	}
	files, err := hasavshevet.ImporterErrorLogs(dirs.HasDir)
	if err != nil {
		return refuse(CodeLogUnreadable, "check that the service can read the folder", "the importer's error logs cannot be listed: %v", err)
	}
	name := strings.ToLower(filepath.Base(bat))
	for i := len(files) - 1; i >= 0 && !sel.stopped; i-- {
		res.Files = append([]string{files[i]}, res.Files...)
		text, err := hasavshevet.ReadMasofonText(files[i], maxMasofonFileBytes)
		if err != nil {
			continue
		}
		lines := splitLines(text)
		for j := len(lines) - 1; j >= 0; j-- {
			at, tag := hasavshevet.ImporterLogLineTime(lines[j])
			if tag != "" && strings.ToLower(tag) != name {
				continue // another profile's
			}
			if !sel.take(lines[j], at) {
				break
			}
		}
	}
	res.Truncated = sel.truncated
	return nil
}

// splitLines splits text into its non-empty lines, trailing spaces and CRs dropped.
func splitLines(text string) []string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if l = strings.TrimRight(l, " \t\r"); l != "" {
			out = append(out, l)
		}
	}
	return out
}
