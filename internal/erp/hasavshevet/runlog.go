package hasavshevet

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/encoding/charmap"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// Masofon run logs (research/reverse-engineering/hasavshevet-run-logs.md,
// P9–P12; FAST_IMP.HDS, IMP_LIB.HDS).
//
// A run of a Masofon BAT appends everything to <ErrDataPath>\<bat>.err while
// it works. When it ends it copies that file to
// <bat>.ok|err.DMY-dd.mm.yy.HMS-hh.mm.ss.LOG (the time is the end's; .ok only
// when a command ran and nothing failed anywhere in the run) and deletes it.
// Inside:
//
//	[<station>.<bat>] ---><command>          one block per command of the profile
//	[dd/mm/yy hh:mm:ss] <input file>:        one section per file the command read
//	[…] קליטת תנועות מלאי הסתיימה בהצלחה      the file imported
//	[…] <value>\t<Stock.NNN message>\t<row>  one line per refusal
//	<pattern> : File Not Found(SetDsn)       the command found no input
//	[…] WizImportError.log  תקלת מערכת…      a system error, detail in
//	                                         <has.exe folder>\DataImpExpLogs
//
// A wildcard command expands to one section per file, so one log can carry
// several orders' results, and .ok / .err is the run's, not one file's.
// Refusals stopped before the log is opened — a second run of the same BAT
// ("double click"), a database being reduced — are written only to
// DataImpExpLogs\WizImportError_<computer>.log as "<date time>  WizIni.dll
// #<bat>#  ERROR …".

// Outcomes of the order's own section of a run log.
const (
	RunLogImported  = "imported"
	RunLogRefused   = "refused"
	RunLogDuplicate = "duplicate" // only Stock.063: the document number was imported before
	RunLogRetry     = "retry"     // refused for a reason that passes (RunLog.Transient)
	RunLogNoInput   = "no_input"  // the command that reads our file found no file
	// RunLogPartial: "חלק מהרשומות לא נקלט בהצלחה" — Masofon2's "partial import
	// despite errors" ([general] ImportWithErrors=YES) imported the good lines
	// and dropped the refused ones (the section's refusals).
	RunLogPartial = "partial"
)

// partialImportLine is the line MLAYIN.HDS ends a partial import with.
const partialImportLine = "חלק מהרשומות לא נקלט בהצלחה"

// RunLogRefusal is one refusal line: the value Hashavshevet refused, its
// message (with the Stock.NNN code when it has one) and the file's row.
type RunLogRefusal struct {
	Value   string `json:"value,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
	Row     int    `json:"row,omitempty"`
}

// RunLog is what a Masofon run logged about the connector's file.
type RunLog struct {
	File string `json:"file"`
	OK   bool   `json:"ok"` // <bat>.ok.…LOG: the whole run, not only our file
	// Live: the run never finished its log (it was stopped): this is the
	// <bat>.err it was writing.
	Live bool `json:"live,omitempty"`
	// Section is the outcome of our file's section (RunLog* constants); empty
	// when the log has no file sections at all and was read whole.
	Section string `json:"section,omitempty"`
	// NoInput: the run found no input file ("File Not Found(SetDsn)").
	NoInput  bool            `json:"noInput,omitempty"`
	Summary  string          `json:"summary,omitempty"`
	Errors   []string        `json:"errors,omitempty"` // "value | message | row", as before
	Refusals []RunLogRefusal `json:"refusals,omitempty"`
	// Transient: the run was refused for a reason that passes — another
	// station issuing or editing definitions (the "Inv" semaphore), a second
	// run of the same BAT, a database being reduced, a deadlock. Never moved
	// aside.
	Transient bool `json:"transient,omitempty"`
	// SystemError: the run reported a system error (WizImportError.log);
	// SystemErrors are that log's lines written during the run (P12).
	SystemError  bool     `json:"systemError,omitempty"`
	SystemErrors []string `json:"systemErrors,omitempty"`
	// OtherFiles counts the other files' sections in the same log: their
	// contents are not reported.
	OtherFiles int `json:"otherFiles,omitempty"`
}

// transientRunLogMarkers are the texts of refusals that pass by themselves
// (MLAYIN.HDS / FAST_IMP.HDS; research/reverse-engineering hds/).
var transientRunLogMarkers = []string{
	"תחנה אחרת מעדכנת קובץ הגדרות או מפיקה", // another station is updating definitions or issuing (Semap "Inv")
	"הקלקה כפולה",      // a second run of the same BAT (<bat>.IsWorking)
	"קיצוץ בתחנה אחרת", // another station is reducing the database
	"deadlock",
	"Semap FAIL",
}

const (
	maxRunLogErrors   = 20
	maxRunLogLineRune = 300
	maxRunLogBytes    = 2 << 20
	maxSysLogTail     = 256 << 10
	// runLogSlack absorbs clock rounding between the connector and the
	// file system (FAT/SMB keep 2 s).
	runLogSlack = 2 * time.Second
)

var (
	runLogStamp = regexp.MustCompile(`^\[\d{2}/\d{2}/\d{2} \d{1,2}:\d{2}:\d{2}\]\s*`)
	// runLogCode is Hashavshevet's rule code as written: "Stock.063B" on a
	// live box, "Stock.002" in the rule table.
	runLogCode   = regexp.MustCompile(`\b([A-Za-z]+\.\d{3}[A-Za-z]?)(?:[^A-Za-z0-9]|$)`)
	sysLogStamp  = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2}) (\d{1,2}):(\d{2}):(\d{2})`)
	sysLogBatTag = regexp.MustCompile(`#([^#\s]+)#`)
)

// isDuplicateCode: Stock.063 — "a document with this number was already
// imported" — with or without its letter.
func isDuplicateCode(code string) bool { return strings.HasPrefix(code, "Stock.063") }

// runLogSection is one file's part of a run log (or, for the lines outside
// any section, the run's).
type runLogSection struct {
	path      string
	pattern   bool // a "File Not Found" pattern, not a file read
	noInput   bool
	imported  bool
	partial   bool // ended partialImportLine: the good lines were imported
	transient bool
	summary   string
	errors    []string
	refusals  []RunLogRefusal
}

// parsedRunLog is a run log split into sections.
type parsedRunLog struct {
	sections    []*runLogSection
	run         runLogSection // lines outside any section
	systemError bool
}

// parseRunLog splits a decoded run log into its file sections.
func parseRunLog(text string) parsedRunLog {
	var p parsedRunLog
	var cur *runLogSection
	for _, raw := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(runLogStamp.ReplaceAllString(strings.TrimSpace(raw), ""))
		switch {
		case line == "" || strings.HasPrefix(line, "==="):
			continue
		case strings.HasPrefix(line, "[") && strings.Contains(line, "] --->"):
			cur = nil // a new command block
			continue
		case strings.Contains(line, "File Not Found(SetDsn)"):
			pat := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(line[:strings.Index(line, "File Not Found(SetDsn)")]), ":"))
			p.sections = append(p.sections, &runLogSection{path: pat, pattern: true, noInput: true})
			cur = nil
			continue
		case strings.Contains(line, "WizImportError.log"):
			p.systemError = true
			continue
		case !strings.Contains(line, "\t") && strings.HasSuffix(line, ":") && strings.ContainsAny(line, `\/`):
			path := strings.TrimSpace(strings.TrimSuffix(line, ":"))
			if cur != nil && sameImportFile(cur.path, path) {
				continue // the header written twice (seen on a live box): one section
			}
			cur = &runLogSection{path: path}
			p.sections = append(p.sections, cur)
			continue
		}
		target := cur
		if target == nil {
			target = &p.run
		}
		target.add(line)
	}
	return p
}

// add files one line of a section.
func (s *runLogSection) add(line string) {
	lower := strings.ToLower(line)
	for _, m := range transientRunLogMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			s.transient = true
			break
		}
	}
	switch {
	case strings.Contains(line, "\t"):
		r := parseRefusal(line)
		s.refusals = append(s.refusals, r)
		if len(s.errors) < maxRunLogErrors {
			s.errors = append(s.errors, cutRunes(strings.Join(strings.Fields(strings.ReplaceAll(line, "\t", " | ")), " "), maxRunLogLineRune))
		}
	case strings.Contains(line, "הסתיימה בהצלחה"):
		s.imported = true
		s.summary = cutRunes(line, maxRunLogLineRune)
	case strings.Contains(line, partialImportLine):
		s.partial = true
		s.summary = cutRunes(line, maxRunLogLineRune)
	case strings.Contains(line, "קליטת") || strings.Contains(line, "נקלט"):
		s.summary = cutRunes(line, maxRunLogLineRune)
	case strings.Contains(line, "ERROR"):
		s.refusals = append(s.refusals, RunLogRefusal{Message: cutRunes(line, maxRunLogLineRune)})
		if len(s.errors) < maxRunLogErrors {
			s.errors = append(s.errors, cutRunes(line, maxRunLogLineRune))
		}
	}
}

// parseRefusal reads "value \t message \t row \t": the last numeric field is
// the row, a field before the message is the value.
func parseRefusal(line string) RunLogRefusal {
	var fields []string
	for _, f := range strings.Split(line, "\t") {
		if f = strings.TrimSpace(f); f != "" {
			fields = append(fields, f)
		}
	}
	var r RunLogRefusal
	if n := len(fields); n > 1 {
		if row, err := strconv.Atoi(fields[n-1]); err == nil {
			r.Row = row
			fields = fields[:n-1]
		}
	}
	if len(fields) > 1 {
		r.Value = cutRunes(fields[0], maxRunLogLineRune)
		fields = fields[1:]
	}
	r.Message = cutRunes(strings.Join(fields, " "), maxRunLogLineRune)
	if m := runLogCode.FindStringSubmatch(r.Message); m != nil {
		r.Code = m[1]
	}
	return r
}

// outcome is a section's verdict (RunLog* constants; "" = nothing decisive).
// A refusal beside a success line is still a refusal: an order wrongly
// reported refused is checked again, one wrongly reported imported is lost.
func (s *runLogSection) outcome() string {
	switch {
	case s.transient:
		return RunLogRetry
	case s.partial:
		return RunLogPartial // the refusals are the dropped lines
	case len(s.refusals) > 0:
		for _, r := range s.refusals {
			if !isDuplicateCode(r.Code) {
				return RunLogRefused
			}
		}
		return RunLogDuplicate
	case s.noInput:
		return RunLogNoInput
	case s.imported:
		return RunLogImported
	case s.summary != "":
		return RunLogRefused // "לא נקלטה כל רשומה" and the like
	}
	return ""
}

// forFile returns the run log as it concerns ourFile, or nil when the log
// has file sections and none is ours (another run's log). A log without any
// section is read whole, as before.
func (p parsedRunLog) forFile(path, ourFile string, ok bool) *RunLog {
	rl := &RunLog{File: path, OK: ok, SystemError: p.systemError}
	if len(p.sections) == 0 {
		s := p.run
		rl.NoInput, rl.Summary, rl.Errors, rl.Refusals, rl.Transient = s.noInput, s.summary, s.errors, s.refusals, s.transient
		return rl
	}
	var mine, pattern *runLogSection
	for _, s := range p.sections {
		switch {
		case !s.pattern && sameImportFile(s.path, ourFile):
			mine = s // the last read of our file wins
		case s.pattern && dataFileMatches(s.path, ourFile):
			pattern = s
		case !s.pattern:
			rl.OtherFiles++
		}
	}
	if mine == nil {
		mine = pattern
	}
	if mine == nil {
		return nil
	}
	rl.Section = mine.outcome()
	// A refusal logged outside every section (a semaphore before the first
	// file) concerns the whole run.
	if p.run.transient && rl.Section != RunLogImported {
		rl.Section = RunLogRetry
	}
	rl.NoInput = mine.noInput
	rl.Summary, rl.Errors, rl.Refusals = mine.summary, mine.errors, mine.refusals
	rl.Transient = rl.Section == RunLogRetry
	if len(rl.Refusals) > maxRunLogErrors {
		rl.Refusals = rl.Refusals[:maxRunLogErrors]
	}
	return rl
}

// FindRunLog returns what the runs of bat logged in logDir since since about
// ourFile: of the logs written since then, the earliest with a section for
// ourFile (the run that read it — not the newest: another run of the same
// BAT may have ended after it). A log without sections is taken only when no
// log has ours. When no finished log has it, the unfinished <bat>.err of a
// stopped run is read (Live). Only since's and today's dates are listed: the
// folder keeps tens of thousands of logs.
func FindRunLog(logDir, bat, ourFile string, since time.Time) (*RunLog, error) {
	name := filepath.Base(strings.TrimSpace(bat))
	if strings.TrimSpace(logDir) == "" || name == "" || name == "." {
		return nil, nil
	}
	type cand struct {
		path string
		at   time.Time
	}
	var cands []cand
	seen := map[string]bool{}
	for _, d := range []time.Time{since, time.Now()} {
		m, err := filepath.Glob(filepath.Join(logDir, name+".*.DMY-"+d.Format("02.01.06")+".HMS-*.LOG"))
		if err != nil {
			return nil, err
		}
		for _, p := range m {
			if seen[p] {
				continue
			}
			seen[p] = true
			if st, err := os.Stat(p); err == nil && !st.ModTime().Before(since.Add(-runLogSlack)) {
				cands = append(cands, cand{p, st.ModTime()})
			}
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if !cands[i].at.Equal(cands[j].at) {
			return cands[i].at.Before(cands[j].at)
		}
		return cands[i].path < cands[j].path
	})
	var whole *RunLog
	for _, c := range cands {
		text, err := readWin1255(c.path, maxRunLogBytes)
		if err != nil {
			return nil, err
		}
		ok := strings.HasPrefix(strings.ToLower(filepath.Base(c.path)), strings.ToLower(name)+".ok.")
		p := parseRunLog(text)
		rl := p.forFile(c.path, ourFile, ok)
		switch {
		case rl == nil:
		case rl.Section != "":
			return rl, nil
		case whole == nil:
			whole = rl
		}
	}
	if live, err := liveRunLog(logDir, name, ourFile, since); err != nil || live != nil {
		return live, err
	}
	return whole, nil
}

// liveRunLog reads <logDir>\<bat>.err — the log of a run that is still
// working or was stopped before it could stamp it — when it was written
// since since and has a section for ourFile.
func liveRunLog(logDir, name, ourFile string, since time.Time) (*RunLog, error) {
	path := filepath.Join(logDir, name+".err")
	st, err := os.Stat(path)
	if err != nil || st.ModTime().Before(since.Add(-runLogSlack)) {
		return nil, nil
	}
	text, err := readWin1255(path, maxRunLogBytes)
	if err != nil {
		return nil, err
	}
	rl := parseRunLog(text).forFile(path, ourFile, false)
	if rl == nil || rl.Section == "" {
		return nil, nil
	}
	rl.Live = true
	return rl, nil
}

// SystemErrorLines returns the lines of hasDir\DataImpExpLogs\WizImportError*.log
// time-stamped within [from, to] (local time, with slack) that are bat's or
// carry no profile tag: the system errors and the refusals Masofon logs
// there instead of the run log (a second run of the BAT, a reduce). The
// files grow for years: only their tails are read.
func SystemErrorLines(hasDir, bat string, from, to time.Time) ([]string, error) {
	if strings.TrimSpace(hasDir) == "" {
		return nil, nil
	}
	files, err := filepath.Glob(filepath.Join(hasDir, "DataImpExpLogs", "WizImportError*.log"))
	if err != nil || len(files) == 0 {
		return nil, err
	}
	sort.Strings(files)
	name := filepath.Base(strings.TrimSpace(bat))
	from, to = from.Add(-runLogSlack), to.Add(runLogSlack)
	var out []string
	for _, f := range files {
		if st, err := os.Stat(f); err != nil || st.ModTime().Before(from) {
			continue
		}
		text, err := readWin1255Tail(f, maxSysLogTail)
		if err != nil {
			return out, err
		}
		for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
			line = strings.TrimSpace(line)
			m := sysLogStamp.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			at := time.Date(atoi(m[1]), time.Month(atoi(m[2])), atoi(m[3]), atoi(m[4]), atoi(m[5]), atoi(m[6]), 0, time.Local)
			if at.Before(from) || at.After(to) {
				continue
			}
			if tag := sysLogBatTag.FindStringSubmatch(line); tag != nil && !strings.EqualFold(tag[1], name) {
				continue // another profile's
			}
			out = append(out, cutRunes(line, maxRunLogLineRune))
			if len(out) == maxRunLogErrors {
				return out, nil
			}
		}
	}
	return out, nil
}

// attachSystemErrors adds the system-error lines to rl (creating a log-less
// RunLog when the run left none) and lets a passing refusal among them make
// the run transient.
func attachSystemErrors(rl *RunLog, lines []string) *RunLog {
	if len(lines) == 0 {
		return rl
	}
	if rl == nil {
		rl = &RunLog{}
	}
	rl.SystemErrors = lines
	for _, l := range lines {
		for _, m := range transientRunLogMarkers {
			if strings.Contains(strings.ToLower(l), strings.ToLower(m)) && rl.Section != RunLogImported {
				rl.Transient = true
				rl.Section = RunLogRetry
			}
		}
	}
	return rl
}

// RunLogDirs is where a box's Masofon runs leave their logs.
type RunLogDirs struct {
	// INI is the masofon.ini read (beside the BAT, else beside has.exe).
	INI string
	// ErrDataPath is that INI's [general] ErrDataPath.
	ErrDataPath string
	// LogDir is the folder used: hasRunLogDir when set, else ErrDataPath.
	LogDir string
	// HasDir is has.exe's folder (masofon.ini's): DataImpExpLogs is there.
	HasDir string
	// INIErr is why masofon.ini could not be read, if it could not.
	INIErr error
}

// ResolveRunLogDirs finds the run-log folder for cfg's BAT (P9): the
// configured hasRunLogDir, else masofon.ini's ErrDataPath. masofon.ini is
// read on every call: the folder can be changed in Masofon2 at any time.
func ResolveRunLogDirs(cfg config.Config) RunLogDirs {
	var d RunLogDirs
	bat, exe := strings.TrimSpace(cfg.HasBatFile), strings.TrimSpace(cfg.HasExePath)
	var dirs []string
	if bat != "" {
		dirs = append(dirs, filepath.Dir(bat))
	}
	if exe != "" {
		dirs = append(dirs, filepath.Dir(exe))
	}
	for _, dir := range dirs {
		ini, path, err := ReadMasofonINI(dir)
		d.INI, d.INIErr = path, err
		if err == nil {
			d.ErrDataPath = strings.TrimSpace(ini.ErrDataPath)
			d.HasDir = dir
			break
		}
	}
	switch {
	case d.HasDir != "":
	case exe != "":
		d.HasDir = filepath.Dir(exe)
	case bat != "":
		d.HasDir = filepath.Dir(bat)
	}
	d.LogDir = d.ErrDataPath
	if o := strings.TrimSpace(cfg.HasRunLogDir); o != "" {
		d.LogDir = o
	}
	return d
}

// readWin1255 reads at most max bytes of a Windows-1255 file as text.
func readWin1255(path string, max int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, max))
	if err != nil {
		return "", err
	}
	text, err := charmap.Windows1255.NewDecoder().Bytes(raw)
	return string(text), err
}

// readWin1255Tail reads the last max bytes of a Windows-1255 file as text
// (the first, possibly cut, line dropped).
func readWin1255Tail(path string, max int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	cut := st.Size() > max
	if cut {
		if _, err := f.Seek(-max, io.SeekEnd); err != nil {
			return "", err
		}
	}
	raw, err := io.ReadAll(io.LimitReader(f, max))
	if err != nil {
		return "", err
	}
	if cut {
		if i := strings.IndexByte(string(raw), '\n'); i >= 0 {
			raw = raw[i+1:]
		}
	}
	text, err := charmap.Windows1255.NewDecoder().Bytes(raw)
	return string(text), err
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func cutRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
