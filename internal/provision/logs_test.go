package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// logsBox is a box whose config.yaml holds the API password and signing secret, with the DB password in the store.
func logsBox(t *testing.T) *fakeSys {
	t.Helper()
	sys, _, _ := handInstallBox(t)
	return sys
}

func writeLog(t *testing.T, dir, name string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runLogs(t *testing.T, sys *fakeSys, in string) (Envelope, *LogsResult, []byte) {
	t.Helper()
	out, _ := Run(context.Background(), testEnv(sys, &fakeDB{}), []string{"logs", "--spec", "-"}, strings.NewReader(in))
	var e Envelope
	var res LogsResult
	e.Result = &res
	if err := json.Unmarshal(out, &e); err != nil {
		t.Fatalf("logs printed %s: %v", out, err)
	}
	return e, &res, out
}

func TestLogs_TheNewestLinesOldestFirst(t *testing.T) {
	sys := logsBox(t)
	writeLog(t, sys.dir, "server.log",
		"2026/11/15 08:00:00 [INFO] started",
		"2026/11/15 08:00:01 [INFO] config loaded",
		"2026/11/15 08:00:02 [ERROR] order 17: has.exe failed",
		"  exit status 3",
		"2026/11/15 08:00:03 [INFO] order 18 written")
	e, res, _ := runLogs(t, sys, `{"source":"server","maxLines":3}`)
	if !e.OK {
		t.Fatalf("refused: %+v", e.Refusal)
	}
	want := []string{"2026/11/15 08:00:02 [ERROR] order 17: has.exe failed", "  exit status 3", "2026/11/15 08:00:03 [INFO] order 18 written"}
	if strings.Join(res.Lines, "\n") != strings.Join(want, "\n") || !res.Truncated || res.Source != "server" {
		t.Fatalf("lines %q truncated %v", res.Lines, res.Truncated)
	}
	if len(res.Files) != 1 || res.Files[0] != "server.log" {
		t.Fatalf("files %v", res.Files)
	}
	_, all, _ := runLogs(t, sys, `{"source":"server"}`)
	if len(all.Lines) != 5 || all.Truncated {
		t.Fatalf("the whole file: %d lines, truncated %v", len(all.Lines), all.Truncated)
	}
}

func TestLogs_RedactsOnTheBox(t *testing.T) {
	sys := logsBox(t)
	cfg, _ := readConfig(t, sys.dir)
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJkaWdpIn0.c2lnbmF0dXJl"
	dpapi := "AQAAANCMnd8BFdERjHoAwE/Cl+sBAAAAexampleblob=="
	secretsInLog := []string{cfg.Auth.Password, cfg.Auth.Secret, testSaPass, "bearer-token-value", jwt, "conn-pass", "url-pass",
		"json-pass", "tsql-pass", dpapi, "PAIRC-ODE99"}
	writeLog(t, sys.dir, "server.log",
		"2026/11/15 08:00:00 [WARN] rejected /auth/token: user=digi-hub password "+cfg.Auth.Password,
		"2026/11/15 08:00:01 [INFO] secret now "+cfg.Auth.Secret+" and db "+testSaPass,
		"2026/11/15 08:00:02 [INFO] GET /api/ping Authorization: Bearer bearer-token-value",
		"2026/11/15 08:00:03 [INFO] token "+jwt,
		"2026/11/15 08:00:04 [INFO] dsn Server=x;User Id=sa;Password=conn-pass;Database=erp",
		"2026/11/15 08:00:05 [INFO] dsn sqlserver://sa:url-pass@127.0.0.1:1433?database=erp",
		`2026/11/15 08:00:06 [INFO] body {"username":"digi","password":"json-pass"}`,
		"2026/11/15 08:00:07 [INFO] CREATE LOGIN x WITH PASSWORD = N'tsql-pass'",
		"2026/11/15 08:00:08 [INFO] blob "+dpapi,
		"2026/11/15 08:00:09 [INFO] pairCode: PAIRC-ODE99")
	e, res, out := runLogs(t, sys, `{"source":"server"}`)
	if !e.OK || len(res.Lines) != 10 {
		t.Fatalf("%s", out)
	}
	for _, s := range secretsInLog {
		if bytes.Contains(out, []byte(s)) {
			t.Errorf("%q left the box: %s", s, out)
		}
	}
	if !strings.Contains(res.Lines[2], "Authorization: "+Redacted) || !strings.Contains(res.Lines[0], "user=digi-hub") {
		t.Fatalf("redaction took too much or too little: %q", res.Lines[:3])
	}
}

func TestLogs_SinceAndGrep(t *testing.T) {
	sys := logsBox(t)
	at := func(h int) string {
		return time.Date(2026, 11, 15, h, 0, 0, 0, time.Local).Format("2006/01/02 15:04:05")
	}
	writeLog(t, sys.dir, "server.log",
		at(6)+" [ERROR] early failure",
		at(7)+" [INFO] fine",
		at(8)+" [ERROR] Order 19 FAILED",
		at(9)+" [INFO] fine again")
	since := time.Date(2026, 11, 15, 7, 0, 0, 0, time.Local).UTC().Format(time.RFC3339)
	_, res, _ := runLogs(t, sys, `{"source":"server","since":"`+since+`"}`)
	if len(res.Lines) != 3 || res.Truncated || strings.Contains(res.Lines[0], "early") {
		t.Fatalf("since: %q truncated %v", res.Lines, res.Truncated)
	}
	_, res, _ = runLogs(t, sys, `{"source":"server","grep":"failed"}`)
	if len(res.Lines) != 1 || !strings.Contains(res.Lines[0], "Order 19") {
		t.Fatalf("grep is a literal without case: %q", res.Lines)
	}
	_, res, _ = runLogs(t, sys, `{"source":"server","grep":"fail.*"}`)
	if len(res.Lines) != 0 {
		t.Fatalf("grep is not a pattern: %q", res.Lines)
	}
}

func TestLogs_LinkLogAcrossItsRotation(t *testing.T) {
	sys := logsBox(t)
	writeLog(t, sys.dir, "link.log.1", "2026-11-15T06:00:00Z [INFO] link: started", "2026-11-15T06:00:01Z [INFO] link: connected")
	writeLog(t, sys.dir, "link.log", "2026-11-15T07:00:00Z [WARN] link: connection closed")
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(filepath.Join(sys.dir, "link.log.1"), old, old)
	_, res, _ := runLogs(t, sys, `{"source":"link","maxLines":2}`)
	if strings.Join(res.Lines, "|") != "2026-11-15T06:00:01Z [INFO] link: connected|2026-11-15T07:00:00Z [WARN] link: connection closed" || !res.Truncated {
		t.Fatalf("%q truncated %v", res.Lines, res.Truncated)
	}
	if strings.Join(res.Files, ",") != "link.log.1,link.log" {
		t.Fatalf("files %v", res.Files)
	}
	_, res, _ = runLogs(t, sys, `{"source":"link","since":"2026-11-15T06:00:01Z"}`)
	if len(res.Lines) != 2 || res.Truncated {
		t.Fatalf("since across files: %q", res.Lines)
	}
}

func TestLogs_ServerLogMovedAside(t *testing.T) {
	sys := logsBox(t)
	writeLog(t, sys.dir, "server.log.20261114T010203.000000000Z.old", "2026/11/14 23:00:00 [INFO] before the move")
	writeLog(t, sys.dir, "server.log", "2026/11/15 08:00:00 [WARN] server.log was not provably this service's own file")
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(filepath.Join(sys.dir, "server.log.20261114T010203.000000000Z.old"), old, old)
	_, res, _ := runLogs(t, sys, `{"source":"server"}`)
	if len(res.Lines) != 2 || !strings.Contains(res.Lines[0], "before the move") {
		t.Fatalf("%q", res.Lines)
	}
}

func TestLogs_NoLogYetIsEmpty(t *testing.T) {
	sys := logsBox(t)
	e, res, _ := runLogs(t, sys, `{"source":"ui"}`)
	if !e.OK || len(res.Lines) != 0 || res.Truncated || res.Lines == nil {
		t.Fatalf("%+v %+v", e, res)
	}
}

func TestLogs_Refusals(t *testing.T) {
	sys := logsBox(t)
	for _, in := range []string{
		`{"source":"config"}`, `{"source":"server","maxLines":2001}`, `{"source":"server","maxLines":-1}`,
		`{"source":"server","since":"yesterday"}`, `{"source":"server","grep":"a\u0000b"}`, `{"source":"server","path":"C:\\x"}`,
		`{"source":"server"} {}`, ``,
	} {
		e, _, out := runLogs(t, sys, in)
		if e.OK || e.Refusal == nil || e.Refusal.Code != CodeSpecInvalid {
			t.Errorf("%s: %s", in, out)
		}
	}
	if out, code := Run(context.Background(), testEnv(sys, &fakeDB{}), []string{"logs"}, strings.NewReader(`{}`)); code != ExitUsage {
		t.Errorf("logs without --spec -: %d %s", code, out)
	}
}

// A link (or anything not a plain file of the data directory) in a log's place is refused, never followed.
func TestLogs_NeverFollowsALink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need a privilege on Windows; dataacl's own tests cover the handle checks")
	}
	sys := logsBox(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	_ = os.WriteFile(outside, []byte("2026/11/15 08:00:00 [INFO] outside-the-data-dir\n"), 0o600)
	if err := os.Symlink(outside, filepath.Join(sys.dir, "server.log")); err != nil {
		t.Fatal(err)
	}
	e, _, out := runLogs(t, sys, `{"source":"server"}`)
	if e.OK || e.Refusal.Code != CodeLogUnreadable || bytes.Contains(out, []byte("outside-the-data-dir")) {
		t.Fatalf("a symlink was followed: %s", out)
	}
	_ = os.Remove(filepath.Join(sys.dir, "server.log"))
	if err := os.Link(filepath.Join(sys.dir, configFile), filepath.Join(sys.dir, "server.log")); err != nil {
		t.Skip("no hard links here:", err)
	}
	e, _, out = runLogs(t, sys, `{"source":"server"}`)
	if e.OK || e.Refusal.Code != CodeLogUnreadable || bytes.Contains(out, []byte("apiListen")) {
		t.Fatalf("a hard link to config.yaml was read: %s", out)
	}
}

// The answer fits the link's 1 MiB stdout frame whatever the log holds.
func TestLogs_TheAnswerIsCapped(t *testing.T) {
	sys := logsBox(t)
	long := strings.Repeat("ש", 3000) // non-ASCII: six bytes each once escaped
	var lines []string
	for i := range 3000 {
		lines = append(lines, fmt.Sprintf("2026/11/15 08:%02d:%02d [INFO] %d %s", i/60%60, i%60, i, long))
	}
	writeLog(t, sys.dir, "server.log", lines...)
	e, res, out := runLogs(t, sys, `{"source":"server","maxLines":2000}`)
	if !e.OK || !res.Truncated || len(out) > 1<<20 || len(out) < 800<<10 {
		t.Fatalf("answer of %d bytes, %d lines, truncated %v", len(out), len(res.Lines), res.Truncated)
	}
	if !strings.HasSuffix(res.Lines[0], logLineCut) || !strings.Contains(res.Lines[len(res.Lines)-1], " 2999 ") {
		t.Fatalf("not the newest lines, or not cut: %.80q ... %.80q", res.Lines[0], res.Lines[len(res.Lines)-1])
	}
}

func TestLogs_EventLog(t *testing.T) {
	sys := logsBox(t)
	cfg, _ := readConfig(t, sys.dir)
	var asked []string
	readEvents = func(source string, since time.Time, max int) ([]EventRecord, error) {
		asked = append(asked, fmt.Sprintf("%s %d %v", source, max, since.IsZero()))
		at := time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)
		return []EventRecord{ // newest first, as Get-WinEvent answers
			{Time: at.Add(2 * time.Minute), Level: "Warning", ID: 2, Message: "link: command \"apply\" refused: admin_off"},
			{Time: at.Add(time.Minute), Level: "Information", ID: 1, Message: "link: started\r\nfingerprint x password " + cfg.Auth.Password},
			{Time: at, Level: "Error", ID: 3, Message: "link: cannot start"},
		}, nil
	}
	t.Cleanup(func() { readEvents = platformEvents })
	_, res, out := runLogs(t, sys, `{"source":"events-link","maxLines":2}`)
	want := "2026-11-15T08:01:00Z [INFORMATION] (event 1) link: started | fingerprint x password " + Redacted
	if len(res.Lines) != 2 || res.Lines[0] != want || !res.Truncated || bytes.Contains(out, []byte(cfg.Auth.Password)) {
		t.Fatalf("%q truncated %v", res.Lines, res.Truncated)
	}
	if asked[0] != "digi-erp-connector-link 3 true" {
		t.Fatalf("asked %v", asked)
	}
	_, res, _ = runLogs(t, sys, `{"source":"events-daemon","grep":"REFUSED"}`)
	if len(res.Lines) != 1 || asked[1] != "digi-erp-connectord 5000 true" {
		t.Fatalf("%q %v", res.Lines, asked)
	}
	readEvents = func(string, time.Time, int) ([]EventRecord, error) {
		return nil, errors.New("the Event Log service is stopped")
	}
	e, _, _ := runLogs(t, sys, `{"source":"events-daemon"}`)
	if e.OK || e.Refusal.Code != CodeLogUnreadable {
		t.Fatalf("%+v", e.Refusal)
	}
}

func TestReverseLines(t *testing.T) {
	for _, c := range []struct{ name, text, want string }{
		{"empty", "", ""},
		{"one line, no newline", "a", "a"},
		{"newline at the end", "a\nb\n", "b|a"},
		{"CRLF", "a\r\nb\r\n", "b|a"},
		{"lines across chunks", strings.Repeat("x", reverseChunk-1) + "\n" + "tail", "tail|" + strings.Repeat("x", reverseChunk-1)},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "f")
			_ = os.WriteFile(p, []byte(c.text), 0o600)
			f, _ := os.Open(p)
			defer f.Close()
			fi, _ := f.Stat()
			r := &reverseLines{f: f, pos: fi.Size()}
			var got []string
			for {
				l, ok, err := r.next()
				if err != nil {
					t.Fatal(err)
				}
				if !ok {
					break
				}
				got = append(got, l)
			}
			if strings.Join(got, "|") != c.want {
				t.Fatalf("got %.60q", strings.Join(got, "|"))
			}
		})
	}
}
