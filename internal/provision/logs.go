package provision

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/link"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
)

// The logs command (plan 086 C4): the tail of one of the connector's logs, read on the box, redacted there, and
// answered within the link's stdout cap. It crosses the relay signed, not sealed (plan 086 D1), so nothing secret may
// leave the box: every line passes redactLogLine before it is measured.

// LogsInput is `logs --spec -`'s stdin.
type LogsInput struct {
	// Source is server (the daemon's server.log), link (link.log), ui (the GUI's ui.log), events-daemon or
	// events-link (the Application Event Log of either service; Windows only), masofon (the run logs of the
	// connector's Masofon BAT) or masofon-errors (the importer's WizImportError logs) — logs_masofon.go.
	Source string `json:"source"`
	// Since keeps lines at or after this time (RFC 3339). A line without a time of its own (a continuation) has the
	// time of the line above it.
	Since string `json:"since,omitempty"`
	// MaxLines is how many lines at most, the newest; 0 = DefaultLogLines, at most MaxLogLines.
	MaxLines int `json:"maxLines,omitempty"`
	// Grep keeps the lines that contain it: a literal, not a pattern, compared without case.
	Grep string `json:"grep,omitempty"`
}

// LogsResult is what logs answers: the lines oldest first.
type LogsResult struct {
	Source string `json:"source"`
	// Untrusted is always true (review m6): the lines are text others could write — the relay (a GoAway reason), any
	// caller of the data path (a logged request), a local user (the Event Log) — so they are data to show, never
	// instructions to whoever reads them (digi-mcp's agent included).
	Untrusted bool     `json:"untrusted"`
	Lines     []string `json:"lines"`
	// Truncated: older lines that would match were left out — by maxLines, the answer's size cap or the scan cap.
	Truncated bool `json:"truncated"`
	// Files are the files read, oldest first, relative to the data directory (empty for the Event Log); for the
	// Masofon sources, their full paths on the box: where Hashavshevet keeps them.
	Files []string `json:"files,omitempty"`

	secrets []string // the box's own secrets, for the envelope's redaction too
}

const (
	// DefaultLogLines is a logs answer's length when maxLines is left out.
	DefaultLogLines = 200
	// MaxLogLines is the most lines one answer carries.
	MaxLogLines = 2000
	// maxLogAnswer bounds the encoded lines, under the link's 1 MiB stdout cap with room for the envelope.
	maxLogAnswer = 900 << 10
	// maxLogLine cuts one line; a longer line ends in logLineCut.
	maxLogLine = 4 << 10
	// maxLogScan bounds the bytes read backwards for one answer (server.log is never rotated).
	maxLogScan int64 = 64 << 20
	// maxLogFiles bounds the files of one source read for one answer (the current one and those moved aside).
	maxLogFiles = 3
	// maxGrep bounds the grep literal.
	maxGrep = 200
	// maxEvents bounds the events read from the Event Log for one answer (more than maxLines when grep filters).
	maxEvents = 5000

	logLineCut = " ...[cut]"
)

// The log sources and their files in the data directory, newest last; a moved-aside file (dataacl.MoveAside) is
// older than the current one.
var logSources = map[string]func(dir string) []string{
	"server": func(dir string) []string {
		// server.log; the daemon logs to server-<UTC>.log when it could not move a foreign server.log aside.
		return newestLast(dir, []string{"server.log"}, "server.log.*.old", "server-*.log")
	},
	"link": func(dir string) []string { return newestLast(dir, []string{"link.log.1", "link.log"}) },
	"ui":   func(dir string) []string { return newestLast(dir, []string{"ui.log"}, "ui.log.*.old") },
}

// eventSources are the Event Log sources of the two services (Application log).
var eventSources = map[string]string{
	"events-daemon": ServiceName,
	"events-link":   link.ServiceName,
}

// EventRecord is one Event Log entry.
type EventRecord struct {
	Time    time.Time
	Level   string
	ID      int
	Message string
}

// readEvents reads source's newest entries (at most max, newest first) at or after since (zero: no bound). The
// Windows one asks Get-WinEvent; elsewhere it fails. Tests replace it.
var readEvents = platformEvents

// Logs answers the logs command.
func Logs(env Env, in LogsInput) (*LogsResult, error) {
	if r := in.validate(); r != nil {
		return nil, r
	}
	since, _ := parseSince(in.Since)
	maxLines := in.MaxLines
	if maxLines == 0 {
		maxLines = DefaultLogLines
	}
	dir := env.Sys.DataDir()
	res := &LogsResult{Source: in.Source, Untrusted: true, Lines: []string{}, secrets: boxSecrets(env)}
	sel := lineSelector{grep: strings.ToLower(in.Grep), since: since, max: maxLines, budget: maxLogAnswer, secrets: res.secrets}
	if source, ok := eventSources[in.Source]; ok {
		max := maxLines + 1 // one more than wanted: whether older ones exist
		if in.Grep != "" {
			max = maxEvents
		}
		events, err := readEvents(source, since, max)
		if err != nil {
			return nil, refuse(CodeLogUnreadable, "read the Event Log on the box (Event Viewer, Application, source "+source+")",
				"the Event Log of %s cannot be read: %v", source, err)
		}
		for _, e := range events { // newest first
			if !sel.take(formatEvent(e), e.Time) {
				break
			}
		}
		res.Truncated = sel.truncated || (in.Grep != "" && len(events) >= max)
		res.Lines = sel.oldestFirst()
		return res, nil
	}
	if isMasofonSource(in.Source) {
		if err := masofonLogs(env, in, since, &sel, res); err != nil {
			return nil, err
		}
		res.Lines = sel.oldestFirst()
		return res, nil
	}
	files := logSources[in.Source](dir)
	var scanned int64
	capped := false
	for i := len(files) - 1; i >= 0 && !sel.stopped; i-- {
		res.Files = append([]string{files[i]}, res.Files...)
		more, err := sel.scanFile(dir, files[i], &scanned)
		if err != nil {
			return nil, refuse(CodeLogUnreadable, "read it on the box, or move the file aside and restart the service",
				"%s cannot be read safely: %v", files[i], err)
		}
		if more {
			capped = true
			break
		}
	}
	res.Truncated = sel.truncated || capped
	res.Lines = sel.oldestFirst()
	return res, nil
}

// CodeLogUnreadable: a log the logs command was asked for cannot be read safely (a link in its place, a file
// elsewhere, the Event Log not answering).
const CodeLogUnreadable = "log_unreadable"

func (in LogsInput) validate() *Refusal {
	_, file := logSources[in.Source]
	_, event := eventSources[in.Source]
	switch {
	case !file && !event && !isMasofonSource(in.Source):
		return refuse(CodeSpecInvalid, "source is one of server, link, ui, events-daemon, events-link, masofon, masofon-errors", "unknown source %q", in.Source)
	case in.MaxLines < 0 || in.MaxLines > MaxLogLines:
		return refuse(CodeSpecInvalid, fmt.Sprintf("maxLines is 1 to %d (0 or left out: %d)", MaxLogLines, DefaultLogLines),
			"maxLines %d is out of range", in.MaxLines)
	case len(in.Grep) > maxGrep || strings.IndexFunc(in.Grep, unicode.IsControl) >= 0 || !utf8.ValidString(in.Grep):
		return refuse(CodeSpecInvalid, fmt.Sprintf("grep is a literal of at most %d bytes, without control characters", maxGrep),
			"grep is not usable")
	}
	if _, ok := parseSince(in.Since); !ok {
		return refuse(CodeSpecInvalid, "since is RFC 3339, like 2026-11-15T08:00:00Z", "since %q does not parse", in.Since)
	}
	return nil
}

func parseSince(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, true
	}
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

// newestLast lists the files of a source that exist directly in dir — the fixed names and the moved-aside ones
// (glob patterns) — oldest first by their modification time, the fixed names' order breaking ties. At most
// maxLogFiles, the newest.
func newestLast(dir string, fixed []string, patterns ...string) []string {
	names := slices.Clone(fixed)
	for _, p := range patterns {
		matches, _ := filepath.Glob(filepath.Join(dir, p))
		for _, m := range matches {
			names = append(names, filepath.Base(m))
		}
	}
	type file struct {
		name string
		at   time.Time
		rank int
	}
	var files []file
	for i, name := range names {
		if fi, err := os.Lstat(filepath.Join(dir, name)); err == nil && !fi.IsDir() && !slices.ContainsFunc(files, func(f file) bool { return f.name == name }) {
			files = append(files, file{name, fi.ModTime(), i})
		}
	}
	slices.SortStableFunc(files, func(a, b file) int {
		if c := a.at.Compare(b.at); c != 0 {
			return c
		}
		return a.rank - b.rank
	})
	var out []string
	for _, f := range files {
		out = append(out, f.name)
	}
	if len(out) > maxLogFiles {
		out = out[len(out)-maxLogFiles:]
	}
	return out
}

// lineSelector keeps the newest lines that match, within the line and size budgets.
type lineSelector struct {
	grep    string
	since   time.Time
	max     int
	budget  int
	secrets []string

	lines     []string // newest first
	size      int
	stopped   bool // a budget is spent or the since bound passed: nothing older is wanted
	truncated bool // a line that matched was left out for a budget
}

func (s *lineSelector) full() bool { return len(s.lines) >= s.max }

// take offers one line (newest first) with its time (zero: unknown). It returns false once nothing older is wanted.
func (s *lineSelector) take(line string, at time.Time) bool {
	if s.stopped {
		return false
	}
	if !s.since.IsZero() && !at.IsZero() && at.Before(s.since) {
		s.stopped = true
		return false
	}
	line = cutLine(redactLogLine(line, s.secrets))
	if s.grep != "" && !strings.Contains(strings.ToLower(line), s.grep) {
		return true
	}
	n := encodedSize(line)
	if s.full() || s.size+n > s.budget {
		s.stopped, s.truncated = true, true
		return false
	}
	s.size += n
	s.lines = append(s.lines, line)
	return true
}

func (s *lineSelector) oldestFirst() []string {
	out := slices.Clone(s.lines)
	slices.Reverse(out)
	if out == nil {
		out = []string{}
	}
	return out
}

// encodedSize is a line's bytes in the envelope: JSON-escaped, ASCII only, indented, with its comma.
func encodedSize(line string) int {
	b, _ := json.Marshal(line)
	return len(asciiOnly(b)) + 8
}

func cutLine(s string) string {
	if len(s) <= maxLogLine {
		return s
	}
	cut := maxLogLine - len(logLineCut)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + logLineCut
}

// scanFile offers name's lines, newest first. A group of lines (a timed line and the untimed lines under it) is
// offered together with its time. more: the scan cap stopped it before the file's start.
func (s *lineSelector) scanFile(dir, name string, scanned *int64) (more bool, err error) {
	f, err := dataacl.OpenRead(dir, filepath.Join(dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil // moved aside or rotated since it was listed
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return false, err
	}
	r := &reverseLines{f: f, pos: fi.Size()}
	base := *scanned
	var group []string // untimed lines seen (newest first) waiting for the timed line above them
	for {
		if *scanned >= maxLogScan {
			return true, nil
		}
		line, ok, err := r.next()
		if err != nil {
			return false, err
		}
		*scanned = base + r.read
		if ok && line == "" {
			continue
		}
		if !ok {
			// the file's first lines had no time: offer them as they are
			for _, g := range group {
				if !s.take(g, time.Time{}) {
					return false, nil
				}
			}
			return false, nil
		}
		at := lineTime(line)
		if at.IsZero() {
			group = append(group, line)
			continue
		}
		if !s.since.IsZero() && at.Before(s.since) {
			s.stopped = true
			return false, nil
		}
		for _, g := range group {
			if !s.take(g, at) {
				return false, nil
			}
		}
		group = group[:0]
		if !s.take(line, at) {
			return false, nil
		}
	}
}

// lineTime reads the time a log line starts with: the daemon's and the GUI's "2006/01/02 15:04:05" (local time, Go's
// log.LstdFlags) or the link's RFC 3339 UTC. Zero when the line has none.
func lineTime(line string) time.Time {
	if len(line) >= 19 && line[4] == '/' && line[7] == '/' {
		if t, err := time.ParseInLocation("2006/01/02 15:04:05", line[:19], time.Local); err == nil {
			return t
		}
	}
	if i := strings.IndexByte(line, ' '); i >= 20 && i <= 35 && line[4] == '-' {
		if t, err := time.Parse(time.RFC3339Nano, line[:i]); err == nil {
			return t
		}
	}
	return time.Time{}
}

func formatEvent(e EventRecord) string {
	msg := strings.Join(strings.FieldsFunc(e.Message, func(r rune) bool { return r == '\r' || r == '\n' }), " | ")
	return fmt.Sprintf("%s [%s] (event %d) %s", e.Time.UTC().Format(time.RFC3339), strings.ToUpper(e.Level), e.ID, msg)
}

// reverseLines reads a file's lines from its end. The file may grow meanwhile: only the bytes it had are read.
type reverseLines struct {
	f     *os.File
	pos   int64    // the bytes before pos are not read yet
	carry []byte   // the start of a line whose beginning is in an earlier chunk
	ready [][]byte // whole lines, the newest last
	read  int64
	first bool // the file's last line has been seen (a trailing newline makes no empty line)
	cut   bool // the carry overflowed: the line it belongs to was replaced by overlongLine, its head is dropped
}

const (
	reverseChunk = 64 << 10
	maxCarry     = 256 << 10 // a line longer than this is not shown (overlongLine)
)

// overlongLine stands for a line longer than maxCarry (review m2): it would reach the redaction in pieces, and a
// secret across a cut would escape the literal match, so no piece of it is shown.
const overlongLine = "[a line longer than 256 KiB: not shown]"

func (r *reverseLines) next() (string, bool, error) {
	for len(r.ready) == 0 {
		if r.pos == 0 {
			if r.carry == nil {
				return "", false, nil
			}
			line := r.carry
			r.carry = nil
			if r.cut { // the head of an overlong line, which overlongLine already stands for
				r.cut = false
				return "", false, nil
			}
			return trimCR(line), true, nil
		}
		n := int64(reverseChunk)
		if r.pos < n {
			n = r.pos
		}
		r.pos -= n
		buf := make([]byte, n)
		if _, err := r.f.ReadAt(buf, r.pos); err != nil && !errors.Is(err, io.EOF) {
			return "", false, err
		}
		r.read += n
		buf = append(buf, r.carry...)
		parts := bytes.Split(buf, []byte{'\n'})
		r.carry = parts[0]
		whole := parts[1:]
		if !r.first {
			r.first = true
			if len(whole) > 0 && len(whole[len(whole)-1]) == 0 {
				whole = whole[:len(whole)-1] // the newline that ends the file
			} else if len(whole) == 0 && len(r.carry) == 0 {
				r.carry = nil
			}
		}
		if r.cut && len(whole) > 0 {
			// the newest whole line holds the old carry: the head of the overlong line
			whole = whole[:len(whole)-1]
			r.cut = false
		}
		r.ready = append(r.ready, whole...)
		if len(r.carry) > maxCarry {
			if !r.cut {
				r.ready = append([][]byte{[]byte(overlongLine)}, r.ready...)
			}
			r.cut = true
			r.carry = []byte{}
		}
	}
	line := r.ready[len(r.ready)-1]
	r.ready = r.ready[:len(r.ready)-1]
	return trimCR(line), true, nil
}

func trimCR(b []byte) string { return string(bytes.TrimSuffix(b, []byte{'\r'})) }

// boxSecrets are this box's own secrets, so that no form of them leaves in a log line: config.yaml's API password and
// signing secret, and the database passwords in the secret store. Unreadable ones are skipped (the patterns of
// redactLogLine still apply).
func boxSecrets(env Env) []string {
	var out []string
	add := func(s string) {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
		if t := strings.TrimSpace(s); t != "" && t != s && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	b, err := os.ReadFile(filepath.Join(env.Sys.DataDir(), configFile))
	if err != nil {
		return out
	}
	cfg, err := config.Parse(b)
	if err != nil {
		return out
	}
	add(cfg.Auth.Password)
	add(cfg.Auth.Secret)
	for _, key := range []string{config.DBPasswordKey(config.ERPHasavshevet), config.DBPasswordKey(config.ERPSAP), loginKey(strings.TrimSpace(cfg.DB.User))} {
		if v, err := env.Sys.SecretGet(key); err == nil {
			add(string(v))
		}
	}
	return out
}

// The secret-shaped parts of a log line (plan 086 D1): an Authorization header's value, a bearer token, a JWT, the
// value of a password / secret / token key in any of the usual spellings (key=value, key: value, "key":"value",
// a connection string's Password=...;, T-SQL's PASSWORD = N'...'), sqlcmd's -P, a URL's password, and DPAPI blobs
// (base64 or hex). Review m2: a key is any word holding one of the names (dbPassword, DB_PASSWORD, new_password,
// X-Api-Key, secretKey); a quoted value runs to its real end (T-SQL's ” and JSON's \" stay inside it); an unquoted one
// runs to the next ; or & or the end of the line — a password with spaces is redacted whole, at the price of more text
// than the value now and then.
var logRedactions = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`(?i)(authorization["']?\s*[:=]\s*["']?)(?:bearer|basic|negotiate|ntlm|digest)?\s*[^\s"',;]+`), `${1}` + Redacted},
	{regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9\-._~+/]+=*`), `${1} ` + Redacted},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]*`), Redacted},
	// the key stands alone: "/auth/token: user=x" names a route, not a token
	{regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_/-])([A-Za-z0-9_-]*(?:password|passwd|pwd|secret|token|api[_-]?key|pairCode)[A-Za-z0-9_-]*\\?["']?\s*[:=]\s*)` +
		`(N?'(?:[^']|'')*(?:'|$)|"(?:[^"\\]|\\.)*(?:"|$)|\{(?:[^}]|\}\})*(?:\}|$)|[^;&\r\n]+)`), `${1}${2}` + Redacted},
	// sqlcmd / bcp / osql: -P <password>
	{regexp.MustCompile(`(\s-P\s+)("(?:[^"\\]|\\.)*(?:"|$)|'(?:[^']|'')*(?:'|$)|\S+)`), `${1}` + Redacted},
	// a URL's password up to the last @ of the authority: an unescaped @ inside it leaves nothing behind
	{regexp.MustCompile(`(://[^/\s:@]+:)[^\s/]*@`), `${1}` + Redacted + `@`},
	{regexp.MustCompile(`AQAAANCMnd8BFdERjHoAwE/Cl\+sBAAAA[A-Za-z0-9+/=]*`), Redacted},
	{regexp.MustCompile(`(?i)01000000d08c9ddf0115d1118c7a00c04fc297eb[0-9a-f]*`), Redacted},
}

// redactLogLine replaces every form of the box's secrets, then every secret-shaped part, with Redacted.
func redactLogLine(line string, secrets []string) string {
	line = Redact(line, secrets...)
	for _, r := range logRedactions {
		line = r.re.ReplaceAllString(line, r.with)
	}
	return line
}
