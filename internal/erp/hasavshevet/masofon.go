package hasavshevet

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/text/encoding/charmap"
)

// Masofon: how Hashavshevet's importer finds what to import, and what it
// leaves behind (research/reverse-engineering/hasavshevet-importer.md P2, P3).
//
// A Masofon BAT holds no parameters: it runs
//   has.exe /"<dir>\importsql.prj -batch" /<server> -p"<bat file name>"
// and has.exe reads the import command from the [commands] line named after
// the BAT in masofon.ini (beside has.exe). That line is split on '-'
// (Hashavshevet's GetParam): 1 company, 2 import type, 3 data file (wildcards
// allowed), 4 FixedLength / Delimited, 5 Ansi2Ansi, 6 PRM file, 7 unused,
// 8 user, 9 station, 10 round totals, 11 rates to all companies, 12 batch,
// 13 print, 14 disqualify duplicates (YES / NO / empty = [stock] AvoidDouble),
// 15 pull rates. Several commands are separated by ';'. [commandsV2] is only
// Masofon2.exe's editor's copy. [general] ErrDataPath is where every run
// leaves <bat>.ok|err.DMY-dd.mm.yy.HMS-hh.mm.ss.LOG.

// MasofonININame is the file has.exe reads, in its own folder.
const MasofonININame = "masofon.ini"

// MasofonCommand is one import command of a profile.
type MasofonCommand struct {
	Company     string `json:"company"`
	ImportType  string `json:"importType"`
	DataFile    string `json:"dataFile"`
	Structure   string `json:"structure"`
	PrmFile     string `json:"prmFile,omitempty"`
	User        string `json:"user,omitempty"`
	AvoidDouble string `json:"avoidDouble,omitempty"` // arg 14 as written: YES, NO or ""
}

// MasofonINI is the part of masofon.ini an integrator needs.
type MasofonINI struct {
	// Profiles maps a BAT file name, lower-cased, to its commands.
	Profiles map[string][]MasofonCommand
	// Names keeps each profile's name as written.
	Names       map[string]string
	ErrDataPath string
	// StockAvoidDouble is [stock] AvoidDouble, the default of arg 14.
	StockAvoidDouble string
}

// ReadMasofonINI reads and parses dir\masofon.ini (Windows-1255).
func ReadMasofonINI(dir string) (MasofonINI, string, error) {
	path := filepath.Join(dir, MasofonININame)
	b, err := os.ReadFile(path)
	if err != nil {
		return MasofonINI{}, path, err
	}
	ini, err := ParseMasofonINI(b)
	return ini, path, err
}

// ParseMasofonINI parses masofon.ini's bytes (Windows-1255).
func ParseMasofonINI(raw []byte) (MasofonINI, error) {
	text, err := charmap.Windows1255.NewDecoder().Bytes(raw)
	if err != nil {
		return MasofonINI{}, err
	}
	ini := MasofonINI{Profiles: map[string][]MasofonCommand{}, Names: map[string]string{}}
	section := ""
	sc := bufio.NewScanner(bytes.NewReader(text))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch section {
		case "commands":
			ini.Profiles[strings.ToLower(key)] = parseMasofonCommands(value)
			ini.Names[strings.ToLower(key)] = key
		case "general":
			if strings.EqualFold(key, "ErrDataPath") {
				ini.ErrDataPath = value
			}
		case "stock":
			if strings.EqualFold(key, "AvoidDouble") {
				ini.StockAvoidDouble = value
			}
		}
	}
	return ini, sc.Err()
}

// parseMasofonCommands splits a [commands] value into its ';'-separated
// commands and each into its '-'-separated arguments, as has.exe does.
func parseMasofonCommands(value string) []MasofonCommand {
	var out []MasofonCommand
	for _, c := range strings.Split(value, ";") {
		if strings.TrimSpace(c) == "" {
			continue
		}
		args := strings.Split(c, "-")
		arg := func(n int) string { // 1-based, as Hashavshevet counts
			if n-1 < len(args) {
				return strings.TrimSpace(args[n-1])
			}
			return ""
		}
		out = append(out, MasofonCommand{
			Company: arg(1), ImportType: arg(2), DataFile: arg(3), Structure: arg(4),
			PrmFile: arg(6), User: arg(8), AvoidDouble: arg(14),
		})
	}
	return out
}

// Profile returns the commands of the profile named after bat (its file name).
func (ini MasofonINI) Profile(bat string) ([]MasofonCommand, bool) {
	cmds, ok := ini.Profiles[strings.ToLower(filepath.Base(strings.TrimSpace(bat)))]
	return cmds, ok
}

// dataFileMatches reports whether a command's data file (possibly a
// wildcard, compared case-insensitively) takes the file path.
func dataFileMatches(pattern, path string) bool {
	norm := func(p string) string {
		return strings.ToLower(filepath.Clean(strings.ReplaceAll(strings.Trim(strings.TrimSpace(p), `"`), "/", `\`)))
	}
	pat, p := norm(pattern), norm(path)
	if pat == "." || p == "." {
		return false
	}
	if filepath.Dir(pat) != filepath.Dir(p) {
		return false
	}
	ok, err := filepath.Match(filepath.Base(pat), filepath.Base(p))
	return err == nil && ok
}

// Masofon profile findings (discover).
const (
	MasofonProfileMissing  = "masofon_profile_missing"
	MasofonDataFileForeign = "masofon_data_file_mismatch"
	MasofonPrmMismatch     = "masofon_prm_mismatch"
	MasofonNotFixedLength  = "masofon_not_fixed_length"
	MasofonNoIssue         = "masofon_import_type"
	MasofonDuplicateOff    = "masofon_duplicate_guard_off"
	MasofonSharedInput     = "masofon_shared_input"
	MasofonDashInPath      = "masofon_dash_in_path"
)

// MasofonFinding is one finding about the connector's Masofon profile.
// Defect true: orders would not be imported as configured.
type MasofonFinding struct {
	Code    string
	Defect  bool
	Message string
	Fix     string
}

// CheckMasofonProfile checks the profile named after bat against what the
// connector writes: dataFile (model (b): <sendOrderDir>\IMOVEIN.doc) and
// prmFile (its IMOVEIN.prm). It never reads the box; the caller reads the ini.
func CheckMasofonProfile(ini MasofonINI, bat, dataFile, prmFile string) []MasofonFinding {
	var out []MasofonFinding
	add := func(code string, defect bool, fix, format string, args ...any) {
		out = append(out, MasofonFinding{Code: code, Defect: defect, Message: fmt.Sprintf(format, args...), Fix: fix})
	}
	name := filepath.Base(strings.TrimSpace(bat))
	for _, p := range []string{dataFile, prmFile} {
		if strings.Contains(p, "-") {
			add(MasofonDashInPath, true, "use a folder and file names without '-'",
				"%s contains '-': has.exe splits the import command on '-', so the path would be cut", p)
		}
	}
	cmds, ok := ini.Profile(bat)
	if !ok {
		add(MasofonProfileMissing, true, "create the profile in Masofon2.exe for this BAT (the BAT's file name is the key)",
			"masofon.ini has no [commands] line named %s: the BAT imports nothing", name)
		return out
	}
	var mine *MasofonCommand
	for i := range cmds {
		if dataFileMatches(cmds[i].DataFile, dataFile) {
			mine = &cmds[i]
			break
		}
	}
	if mine == nil {
		var files []string
		for _, c := range cmds {
			files = append(files, c.DataFile)
		}
		add(MasofonDataFileForeign, true, "point the profile's data file at "+dataFile+" (Masofon2.exe), or the connector at the profile's folder",
			"the profile %s reads %s, not the connector's %s: orders would never be imported", name, strings.Join(files, ", "), dataFile)
		return out
	}
	if !strings.EqualFold(mine.Structure, "FixedLength") {
		add(MasofonNotFixedLength, true, "set the profile's file structure to FixedLength",
			"the profile %s reads its file as %q; IMOVEIN is fixed-length", name, mine.Structure)
	}
	if prmFile != "" && !dataFileMatches(mine.PrmFile, prmFile) {
		add(MasofonPrmMismatch, true, "point the profile's PRM file at "+prmFile,
			"the profile %s reads the PRM %s, not the connector's %s", name, mine.PrmFile, prmFile)
	}
	switch mine.ImportType {
	case "6":
	case "118":
		add(MasofonNoIssue, false, "use import type 6 to issue the documents, if that is the tenant's choice",
			"the profile %s imports with type 118: documents stay drafts (not issued)", name)
	default:
		add(MasofonNoIssue, true, "use import type 6 (stock documents) for orders",
			"the profile %s imports with type %s, not stock documents (6)", name, mine.ImportType)
	}
	avoid := mine.AvoidDouble
	if avoid == "" {
		avoid = ini.StockAvoidDouble
	}
	if !strings.EqualFold(avoid, "YES") {
		add(MasofonDuplicateOff, false, "turn on 'disqualify duplicated documents' in the profile if the tenant wants Hashavshevet to refuse a resend",
			"the profile %s does not refuse duplicates (DISQUALIFICATIONOFDUPLICATION is %q)", name, avoid)
	}
	var others []string
	for key, cs := range ini.Profiles {
		if strings.EqualFold(key, name) {
			continue
		}
		for _, c := range cs {
			if dataFileMatches(c.DataFile, dataFile) ||
				strings.EqualFold(filepath.Dir(filepath.Clean(c.DataFile)), filepath.Dir(filepath.Clean(dataFile))) {
				others = append(others, ini.Names[key])
				break
			}
		}
	}
	if len(others) > 0 {
		sort.Strings(others)
		add(MasofonSharedInput, true, "give the connector a folder no other profile reads",
			"other profiles read the connector's file or folder too: %s (they race its imports; the importer writes temp files into that folder)", strings.Join(others, ", "))
	}
	return out
}

// RunLog is what a Masofon run left in ErrDataPath for one BAT.
type RunLog struct {
	File string `json:"file"`
	OK   bool   `json:"ok"` // <bat>.ok.…LOG
	// NoInput: the run found no input file ("File Not Found(SetDsn)").
	NoInput bool     `json:"noInput,omitempty"`
	Summary string   `json:"summary,omitempty"`
	Errors  []string `json:"errors,omitempty"`
}

const (
	maxRunLogErrors   = 20
	maxRunLogLineRune = 300
	maxRunLogBytes    = 256 * 1024
)

var runLogStamp = regexp.MustCompile(`^\[\d{2}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}\]\s*`)

// FindRunLog returns the newest run log of bat in errDataPath written at or
// after since (a little slack for clock rounding), or nil when there is none.
// Only the names of since's and today's dates are looked at: the folder
// keeps tens of thousands of logs.
func FindRunLog(errDataPath, bat string, since time.Time) (*RunLog, error) {
	name := filepath.Base(strings.TrimSpace(bat))
	if errDataPath == "" || name == "" {
		return nil, nil
	}
	var candidates []string
	seen := map[string]bool{}
	for _, d := range []time.Time{since, time.Now()} {
		pattern := filepath.Join(errDataPath, name+".*.DMY-"+d.Format("02.01.06")+".HMS-*.LOG")
		m, err := filepath.Glob(pattern)
		if err != nil {
			return nil, err
		}
		for _, p := range m {
			if !seen[p] {
				seen[p] = true
				candidates = append(candidates, p)
			}
		}
	}
	var best string
	var bestTime time.Time
	for _, p := range candidates {
		st, err := os.Stat(p)
		if err != nil || st.ModTime().Before(since.Add(-2*time.Second)) {
			continue
		}
		if best == "" || st.ModTime().After(bestTime) {
			best, bestTime = p, st.ModTime()
		}
	}
	if best == "" {
		return nil, nil
	}
	return readRunLog(best, name)
}

func readRunLog(path, bat string) (*RunLog, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw := make([]byte, maxRunLogBytes)
	n, _ := f.Read(raw)
	text, err := charmap.Windows1255.NewDecoder().Bytes(raw[:n])
	if err != nil {
		return nil, err
	}
	rl := &RunLog{File: path, OK: strings.HasPrefix(strings.ToLower(filepath.Base(path)), strings.ToLower(bat)+".ok.")}
	for _, line := range strings.Split(strings.ReplaceAll(string(text), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(runLogStamp.ReplaceAllString(line, ""))
		switch {
		case line == "" || strings.HasPrefix(line, "===") || strings.HasPrefix(line, "["):
			// blank, separator, the "[station.bat] --->command" line
		case strings.Contains(line, "File Not Found(SetDsn)"):
			rl.NoInput = true
		case strings.Contains(line, "\t"):
			if len(rl.Errors) < maxRunLogErrors {
				rl.Errors = append(rl.Errors, cutRunes(strings.Join(strings.Fields(strings.ReplaceAll(line, "\t", " | ")), " "), maxRunLogLineRune))
			}
		case strings.Contains(line, "קליטת") || strings.Contains(line, "נקלט"):
			rl.Summary = cutRunes(line, maxRunLogLineRune) // the run's last word
		}
	}
	return rl, nil
}

func cutRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
