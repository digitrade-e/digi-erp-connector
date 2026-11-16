package link

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Open connector windows before a setup (digi-mcp G-143, GROOPY's failed upgrade). The setup runs as LocalSystem in
// session 0; a connector window open in someone's RDP session holds digi-erp-connector.exe, which the setup must
// replace. Inno's /CLOSEAPPLICATIONS asks the Restart Manager which applications to close and closes only those it
// reports with no reboot reason — a process in another Terminal Services session comes back as
// RmRebootReasonSessionMismatch, so Inno leaves it open and the setup fails. The runner therefore ends, before each
// setup (the upgrade's and the rollback's), every process whose image is the INSTALLED digi-erp-connector.exe — by full
// path, not by name — in every session, and waits up to closeAppsWait for them to be gone. Each is ended through the
// one handle its image was read on (review m5): a PID another process took meanwhile is never ended. What was open is
// lost unsaved. One still open then means the setup cannot replace the file: it is not run, and the result says why.

const (
	guiExe        = "digi-erp-connector.exe"
	closeAppsWait = 15 * time.Second
	closeAppsPoll = 500 * time.Millisecond
)

// RunningProcess is one process the runner may end before a setup.
type RunningProcess struct {
	PID     uint32
	Session uint32
	Image   string // the full path of its executable ("" when it cannot be read)
}

// ClosedApps is what the runner found and ended before a setup (in the result file): the installed connector's windows.
type ClosedApps struct {
	Found     int      `json:"found"`
	Closed    int      `json:"closed"`
	StillOpen int      `json:"stillOpen"`
	Sessions  []uint32 `json:"sessions,omitempty"` // the sessions they were open in
	Error     string   `json:"error,omitempty"`    // the processes could not be listed
}

// errNotThatProcess: the PID no longer names the installed connector (another process took it): it is not ended.
var errNotThatProcess = errors.New("not the installed connector")

// samePath compares two Windows paths: cleaned, without case.
func samePath(a, b string) bool {
	return a != "" && strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// closeConnectorWindows ends the installed connector's windows in every session (see the top of this file). Nil when
// none was open (or there is no process lister: a test's runner).
func (r UpgradeRunner) closeConnectorWindows(installDir string) *ClosedApps {
	if r.ListProcesses == nil || installDir == "" {
		return nil
	}
	exe := filepath.Join(installDir, guiExe)
	installed := func() ([]RunningProcess, error) {
		all, err := r.ListProcesses(guiExe)
		var out []RunningProcess
		for _, p := range all {
			if samePath(p.Image, exe) {
				out = append(out, p)
			}
		}
		return out, err
	}
	open, err := installed()
	if err != nil && len(open) == 0 {
		r.Log.Warn(fmt.Sprintf("link upgrade %s: the running processes could not be listed (%v); an open connector window would fail the setup", r.Ticket, err))
		return &ClosedApps{Error: "listing the processes: " + err.Error()}
	}
	if len(open) == 0 {
		return nil
	}
	c := &ClosedApps{Found: len(open)}
	for _, p := range open {
		if !slices.Contains(c.Sessions, p.Session) {
			c.Sessions = append(c.Sessions, p.Session)
		}
		if err := r.KillProcess(p.PID, exe); err != nil {
			r.Log.Warn(fmt.Sprintf("link upgrade %s: ending the connector window %d (session %d): %v", r.Ticket, p.PID, p.Session, err))
		}
	}
	deadline := r.Now().Add(closeAppsWait)
	still := open
	for {
		if now, err := installed(); err == nil {
			still = slices.DeleteFunc(still, func(p RunningProcess) bool {
				return !slices.ContainsFunc(now, func(q RunningProcess) bool { return q.PID == p.PID })
			})
		}
		if len(still) == 0 || !r.Now().Before(deadline) {
			break
		}
		r.Sleep(closeAppsPoll)
	}
	c.Closed, c.StillOpen = c.Found-len(still), len(still)
	r.Log.Info(fmt.Sprintf("link upgrade %s: %d connector window(s) open in session(s) %v: %d ended, %d still open (unsaved changes there are lost)",
		r.Ticket, c.Found, c.Sessions, c.Closed, c.StillOpen))
	return c
}

// stillOpenError is the result's error when a window outlived closeAppsWait: the setup could not replace the file.
func stillOpenError(c *ClosedApps, what string) string {
	return fmt.Sprintf("%d connector window(s) (the installed %s, session(s) %v) did not close within %s: %s could not replace it, so it was not run",
		c.StillOpen, guiExe, c.Sessions, closeAppsWait, what)
}
