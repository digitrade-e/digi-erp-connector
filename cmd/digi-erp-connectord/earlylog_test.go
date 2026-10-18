package main

import (
	"errors"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/logger"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/autostart"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
)

type recordLogger struct{ lines []string }

func (r *recordLogger) Info(m string)           { r.lines = append(r.lines, "INFO "+m) }
func (r *recordLogger) Error(m string, _ error) { r.lines = append(r.lines, "ERROR "+m) }
func (r *recordLogger) Warn(m string)           { r.lines = append(r.lines, "WARN "+m) }
func (r *recordLogger) Success(m string)        { r.lines = append(r.lines, "OK "+m) }
func (r *recordLogger) Close() error            { return nil }

// What Start logs before server.log may be opened (R2-3: only after the data
// directory is protected) is kept and written into it once it is.
func TestEarlyLog_ReplaysInOrder(t *testing.T) {
	e := newEarlyLog()
	e.Info("one")
	e.Warn("two")
	e.Error("three", errors.New("x"))
	r := &recordLogger{}
	e.replayInto(r)
	want := []string{"INFO one", "WARN two", "ERROR three: x"} // the reason travels in the line
	if len(r.lines) != 3 {
		t.Fatalf("replayed %v; want %v", r.lines, want)
	}
	for i := range want {
		if r.lines[i] != want[i] {
			t.Errorf("line %d = %q; want %q", i, r.lines[i], want[i])
		}
	}
}

// R3-4: whether server.log needs a fresh file object is decided from the
// file, not from what this run's walk did: a log is kept only when it is
// exactly ours (FileTrusted) AND it is the very object a previous start
// opened fresh (its file ID matches the marker) AND this walk did not have to
// reset it.
func TestLogNeedsFreshFile(t *testing.T) {
	for name, tc := range map[string]struct {
		exists, trusted, reset bool
		id, marker             string
		want                   bool
	}{
		"no log yet":                       {false, false, false, "", "", false},
		"ours, marker matches":             {true, true, false, "a", "a", false},
		"no marker (first start, upgrade)": {true, true, false, "a", "", true},
		"another object under the name":    {true, true, false, "b", "a", true},
		"not trusted":                      {true, false, false, "a", "a", true},
		"reset by this walk":               {true, true, true, "a", "a", true},
		"id unreadable":                    {true, true, false, "", "", true},
	} {
		if got := logNeedsFreshFile(tc.exists, tc.trusted, tc.reset, tc.id, tc.marker); got != tc.want {
			t.Errorf("%s: logNeedsFreshFile = %v; want %v", name, got, tc.want)
		}
	}
}

type timedRecorder struct {
	recordLogger
	at []time.Time
}

func (r *timedRecorder) WriteAt(t time.Time, level, msg string) {
	r.at = append(r.at, t)
	r.lines = append(r.lines, level+" "+msg)
}

// Replayed lines keep the time they were logged at; nothing is replayed into
// the stderr fallback, which already showed them.
func TestEarlyLog_KeepsTimestampsAndSkipsStderr(t *testing.T) {
	e := newEarlyLog()
	e.Info("one")
	first := e.entries[0].at
	time.Sleep(20 * time.Millisecond)
	r := &timedRecorder{}
	e.replayInto(r)
	if len(r.at) != 1 || !r.at[0].Equal(first) || r.lines[0] != "INFO one" {
		t.Errorf("replayed %v at %v; want INFO one at %v", r.lines, r.at, first)
	}
	if !shouldReplay(r) || shouldReplay(logger.NewStderr()) {
		t.Error("shouldReplay: want the file logger replayed into, the stderr fallback skipped")
	}
}

// R3-1: a refusal never opens a file log in the data directory (a junctioned
// or swapped root would take it elsewhere): the early lines and the reason go
// to the refusal report (the Event Log on Windows), and Start's error carries
// the service-specific exit code the service manager shows.
func TestStart_RefusalOpensNoFileLog(t *testing.T) {
	stubProtect(t, dataacl.Report{}, dataacl.ErrReparsePoint, nil)
	opened := 0
	oldB, oldR := newBootstrapLog, reportRefusalFn
	newBootstrapLog = func() logger.LoggerService { opened++; return &quietLogger{} }
	var reported []string
	var reportedErr error
	reportRefusalFn = func(lines []string, err error) { reported, reportedErr = lines, err }
	t.Cleanup(func() { newBootstrapLog, reportRefusalFn = oldB, oldR })

	err := (&serverApp{}).Start()
	if !errors.Is(err, dataacl.ErrReparsePoint) {
		t.Fatalf("Start = %v; want the walk's error", err)
	}
	var ec autostart.ExitCoder
	if !errors.As(err, &ec) || ec.ExitCode() != autostart.ExitDataDirRefused {
		t.Errorf("Start's error %v has no exit code %d", err, autostart.ExitDataDirRefused)
	}
	if opened != 0 {
		t.Errorf("the bootstrap (file) log was opened %d times on a refusal; want never", opened)
	}
	if !errors.Is(reportedErr, dataacl.ErrReparsePoint) || len(reported) < 2 {
		t.Errorf("refusal reported %v / %v; want the early lines and the reason", reported, reportedErr)
	}
}

// The service handler finds the checkpoint hook through this interface.
var _ autostart.StartProgressSetter = (*serverApp)(nil)
