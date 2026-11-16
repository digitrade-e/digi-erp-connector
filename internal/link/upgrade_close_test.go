package link

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProcs plays the machine's processes for the runner (G-143).
type fakeProcs struct {
	mu     sync.Mutex
	procs  []RunningProcess
	killed []uint32
	stuck  map[uint32]bool // ending it fails: it stays
	err    error
}

func (f *fakeProcs) list(name string) ([]RunningProcess, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var out []RunningProcess
	for _, p := range f.procs {
		if p.Image == "" || strings.EqualFold(filepath.Base(p.Image), name) {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakeProcs) kill(pid uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, pid)
	if f.stuck[pid] {
		return errors.New("access is denied")
	}
	f.procs = slices.DeleteFunc(f.procs, func(p RunningProcess) bool { return p.PID == pid })
	return nil
}

func (f *fakeProcs) add(p RunningProcess) { f.mu.Lock(); f.procs = append(f.procs, p); f.mu.Unlock() }

func withProcs(r *UpgradeRunner, f *fakeProcs) { r.ListProcesses, r.KillProcess = f.list, f.kill }

var installedGUI = filepath.Join("install", guiExe) // the runner rig's job installs to "install"

// A connector window open in another session (an RDP user's) holds the file the setup replaces, and Inno cannot close
// it from session 0: the runner ends the installed connector's windows, by full path, in every session — not another
// copy of it, not a process it cannot read.
func TestTheRunnerEndsTheConnectorsWindowsInEverySession(t *testing.T) {
	g := newRunnerRig(t, true)
	f := &fakeProcs{procs: []RunningProcess{
		{PID: 101, Session: 2, Image: installedGUI},
		{PID: 102, Session: 3, Image: filepath.Join("other", guiExe)},
		{PID: 103, Session: 0, Image: strings.ToUpper(installedGUI)},
		{PID: 104, Session: 2, Image: ""},
	}}
	withProcs(&g.r, f)
	if _, err := g.r.Run(); err != nil {
		t.Fatal(err)
	}
	r := g.result(t)
	c := r.ClosedApps
	if c == nil || c.Found != 2 || c.Closed != 2 || c.StillOpen != 0 || !slices.Equal(c.Sessions, []uint32{2, 0}) || r.Error != "" || g.ran != 1 {
		t.Fatalf("closed %+v, result %+v, ran %d", c, r, g.ran)
	}
	if !slices.Equal(f.killed, []uint32{101, 103}) || len(f.procs) != 2 {
		t.Fatalf("killed %v, left %v", f.killed, f.procs)
	}
	if !g.log.has("2 connector window(s) open in session(s) [2 0]: 2 ended, 0 still open") {
		t.Fatal("not logged")
	}
	// none open: nothing in the result
	g = newRunnerRig(t, true)
	withProcs(&g.r, &fakeProcs{})
	_, _ = g.r.Run()
	if r := g.result(t); r.ClosedApps != nil || g.ran != 1 {
		t.Fatalf("%+v", r.ClosedApps)
	}
}

// A window that will not end means the setup cannot replace the file: it is not run, and the services stay as they were.
func TestASetupIsNotRunBesideAWindowThatStaysOpen(t *testing.T) {
	g := newRunnerRig(t, true)
	f := &fakeProcs{procs: []RunningProcess{{PID: 101, Session: 2, Image: installedGUI}}, stuck: map[uint32]bool{101: true}}
	withProcs(&g.r, f)
	_, _ = g.r.Run()
	r := g.result(t)
	if g.ran != 0 || r.ClosedApps == nil || r.ClosedApps.StillOpen != 1 || r.ClosedApps.Closed != 0 ||
		!strings.Contains(r.Error, "did not close within 15s: the setup could not replace it, so it was not run") || len(g.started)+len(g.stopped) != 0 {
		t.Fatalf("ran %d, result %+v", g.ran, r)
	}
	waited := time.Duration(0)
	for _, d := range g.slept {
		if d == closeAppsPoll {
			waited += d
		}
	}
	if waited < closeAppsWait-time.Second {
		t.Fatalf("waited %s", waited)
	}
	if _, err := os.Stat(filepath.Join(g.tdir, setupFile("1.0.30"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the setup that was not run was kept")
	}
}

// Processes that cannot be listed do not stop the setup: it may well find nothing in its way.
func TestUnlistableProcessesDoNotStopTheSetup(t *testing.T) {
	g := newRunnerRig(t, true)
	withProcs(&g.r, &fakeProcs{err: errors.New("snapshot failed")})
	_, _ = g.r.Run()
	if r := g.result(t); g.ran != 1 || r.ClosedApps == nil || !strings.Contains(r.ClosedApps.Error, "snapshot failed") || r.Error != "" {
		t.Fatalf("ran %d, %+v", g.ran, r.ClosedApps)
	}
}

// The rollback's setup replaces the same file: a window opened during the health check is ended before it too.
func TestTheRollbackEndsAWindowOpenedMeanwhile(t *testing.T) {
	g := newHealthRig(t, true, nil)
	g.linkUp = func(installed string, _ time.Time) bool { return installed == "1.0.31" }
	f := &fakeProcs{}
	withProcs(&g.r, f)
	run := g.r.RunSetup
	g.r.RunSetup = func(s, l string, d time.Duration) (int, error) {
		code, err := run(s, l, d)
		if !strings.HasPrefix(filepath.Base(s), "rollback-") {
			f.add(RunningProcess{PID: 300, Session: 4, Image: installedGUI}) // someone opens the new build's window
		}
		return code, err
	}
	r := g.run(t)
	if r.ClosedApps != nil || r.RollbackClosedApps == nil || r.RollbackClosedApps.Closed != 1 || !slices.Equal(r.RollbackClosedApps.Sessions, []uint32{4}) ||
		r.Rollback == nil || !r.Rollback.OK || g.ran != 2 {
		t.Fatalf("closed %+v / %+v, rollback %+v, ran %d", r.ClosedApps, r.RollbackClosedApps, r.Rollback, g.ran)
	}
}

// The setup is asked to close (by force) what holds its files in its own session, and to log it.
func TestTheSetupClosesApplications(t *testing.T) {
	for _, flag := range []string{"/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", "/SP-", "/CLOSEAPPLICATIONS", "/FORCECLOSEAPPLICATIONS", "/LOGCLOSEAPPLICATIONS"} {
		if !slices.Contains(strings.Fields(setupArgs), flag) {
			t.Errorf("the setup is not run with %s", flag)
		}
	}
}
