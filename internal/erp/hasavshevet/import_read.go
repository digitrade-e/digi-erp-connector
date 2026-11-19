package hasavshevet

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Read-only views of what the order check and Masofon left on the box, for
// provision's state and logs commands (digi-mcp reads a box through them;
// research/reverse-engineering/hasavshevet-run-logs.md).

// LoadImportCheck reads order number's stored import check from the data
// directory; nil when there is none.
func LoadImportCheck(dataDir string, number int64) *ImportCheck {
	return loadImportCheck(OrderImportsDir(dataDir), number)
}

// ImportCheckNumbers lists the order numbers with a stored import check,
// highest first.
func ImportCheckNumbers(dataDir string) ([]int64, error) {
	entries, err := os.ReadDir(OrderImportsDir(dataDir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		if n, err := strconv.ParseInt(strings.TrimSuffix(name, ".json"), 10, 64); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] > out[j] })
	return out, nil
}

// RunLogFile is one Masofon run log of a BAT.
type RunLogFile struct {
	Path string
	// At is when the run ended (the name's DMY/HMS stamp); for the live
	// <bat>.err, its modification time.
	At   time.Time
	Live bool
}

var runLogNameStamp = regexp.MustCompile(`\.DMY-(\d{2})\.(\d{2})\.(\d{2})\.HMS-(\d{2})\.(\d{2})\.(\d{2})\.LOG$`)

// RunLogFiles lists bat's run logs in logDir — <bat>.ok|err.DMY-….LOG, timed
// by their names (the folder keeps tens of thousands: no file is opened or
// stat'ed to sort them) — at or after since (zero: any), oldest first, the
// newest max, then the unfinished <bat>.err of a run in progress or stopped.
func RunLogFiles(logDir, bat string, since time.Time, max int) ([]RunLogFile, error) {
	name := filepath.Base(strings.TrimSpace(bat))
	if strings.TrimSpace(logDir) == "" || name == "" || name == "." {
		return nil, nil
	}
	matches, err := filepath.Glob(filepath.Join(logDir, name+".*.DMY-*.HMS-*.LOG"))
	if err != nil {
		return nil, err
	}
	var out []RunLogFile
	for _, p := range matches {
		m := runLogNameStamp.FindStringSubmatch(filepath.Base(p))
		if m == nil {
			continue
		}
		at := time.Date(2000+atoi(m[3]), time.Month(atoi(m[2])), atoi(m[1]), atoi(m[4]), atoi(m[5]), atoi(m[6]), 0, time.Local)
		if !since.IsZero() && at.Before(since.Add(-runLogSlack)) {
			continue
		}
		out = append(out, RunLogFile{Path: p, At: at})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	if max > 0 && len(out) > max {
		out = out[len(out)-max:]
	}
	live := filepath.Join(logDir, name+".err")
	if st, err := os.Lstat(live); err == nil && st.Mode().IsRegular() && (since.IsZero() || !st.ModTime().Before(since)) {
		out = append(out, RunLogFile{Path: live, At: st.ModTime(), Live: true})
	}
	return out, nil
}

// ImporterErrorLogs lists hasDir\DataImpExpLogs\WizImportError*.log, the
// importer's system-error logs, oldest first by modification time.
func ImporterErrorLogs(hasDir string) ([]string, error) {
	if strings.TrimSpace(hasDir) == "" {
		return nil, nil
	}
	matches, err := filepath.Glob(filepath.Join(hasDir, "DataImpExpLogs", "WizImportError*.log"))
	if err != nil {
		return nil, err
	}
	type f struct {
		p  string
		at time.Time
	}
	var fs []f
	for _, p := range matches {
		if st, err := os.Lstat(p); err == nil && st.Mode().IsRegular() {
			fs = append(fs, f{p, st.ModTime()})
		}
	}
	sort.SliceStable(fs, func(i, j int) bool { return fs[i].at.Before(fs[j].at) })
	out := make([]string, 0, len(fs))
	for _, x := range fs {
		out = append(out, x.p)
	}
	return out, nil
}

// ReadMasofonText reads a Masofon text file (Windows-1255) as text: a
// regular file only (never through a link), its last max bytes, the first
// cut line dropped.
func ReadMasofonText(path string, max int64) (string, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", &os.PathError{Op: "read", Path: path, Err: os.ErrInvalid}
	}
	return readWin1255Tail(path, max)
}

// ImporterLogLineTime is a WizImportError line's time ("yyyy-mm-dd h:mm:ss",
// local), zero without one; tag is the profile it names (#<bat>#), if any.
func ImporterLogLineTime(line string) (at time.Time, tag string) {
	if m := sysLogStamp.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
		at = time.Date(atoi(m[1]), time.Month(atoi(m[2])), atoi(m[3]), atoi(m[4]), atoi(m[5]), atoi(m[6]), 0, time.Local)
	}
	if m := sysLogBatTag.FindStringSubmatch(line); m != nil {
		tag = m[1]
	}
	return at, tag
}
