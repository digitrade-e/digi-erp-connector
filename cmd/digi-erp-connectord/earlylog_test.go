package main

import (
	"errors"
	"testing"

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
	want := []string{"INFO one", "WARN two", "ERROR three"}
	if len(r.lines) != 3 {
		t.Fatalf("replayed %v; want %v", r.lines, want)
	}
	for i := range want {
		if r.lines[i] != want[i] {
			t.Errorf("line %d = %q; want %q", i, r.lines[i], want[i])
		}
	}
}

// A log file the walk had to rewrite was not ours as it stood — its file
// object may be held open by whoever could read it before — so it is moved
// aside and a fresh one started.
func TestLogNeedsFreshFile(t *testing.T) {
	const log = `C:\ProgramData\digi-erp-connector\server.log`
	for name, tc := range map[string]struct {
		rep  dataacl.Report
		want bool
	}{
		"untouched":         {dataacl.Report{Entries: 5}, false},
		"other files reset": {dataacl.Report{Reset: 1, ResetPaths: []string{`C:\ProgramData\digi-erp-connector\config.yaml`}}, false},
		"the log reset":     {dataacl.Report{Reset: 1, ResetPaths: []string{log}}, true},
		"the log, case":     {dataacl.Report{Reset: 1, ResetPaths: []string{`c:\programdata\DIGI-ERP-CONNECTOR\SERVER.LOG`}}, isWindows},
	} {
		if got := logNeedsFreshFile(log, tc.rep); got != tc.want {
			t.Errorf("%s: logNeedsFreshFile = %v; want %v", name, got, tc.want)
		}
	}
}

// The service handler finds the checkpoint hook through this interface.
var _ autostart.StartProgressSetter = (*serverApp)(nil)
