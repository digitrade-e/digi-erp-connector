package link

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/atomicfile"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/autostart"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/filelock"
	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// The upgrade runner (plan 086 C8): `digi-erp-connector-upgrade.exe upgrade-run <ticket>`, a copy of the link's own
// executable in DataDir\upgrade\<ticket>\, started detached by the link (spawnDetached). It outlives the link, which
// the setup stops, and the install folder, which the setup replaces.

// DaemonServiceName is the connector's own service, which the runner starts again if the setup left it stopped.
const DaemonServiceName = "digi-erp-connectord"

const (
	// answerGrace: the runner waits this long before the setup stops the link, so the link's "accepted" leaves first.
	answerGrace = 10 * time.Second
	// setupTimeout bounds the wait for the setup: two services' stops of up to 180 s each, the copy, the starts. A
	// setup still running then is left running (never killed half-way) and the result says so.
	setupTimeout = 45 * time.Minute
	// lockWait is how long the runner waits for the lock a link's check may hold for a moment.
	lockWait = 30 * time.Second
)

// setupArgs are the setup's silent flags (Inno Setup): no window, no message box (each takes its default answer), no
// reboot, no "This will install..." prompt; close (by force) the applications using the files it replaces, with that
// logged — in its own session only, which is why the runner ends the connector's windows in the others first
// (upgrade_close.go); and its log in the ticket's folder (/LOG, added by runSetupSilent).
const setupArgs = `/VERYSILENT /SUPPRESSMSGBOXES /NORESTART /SP- /CLOSEAPPLICATIONS /FORCECLOSEAPPLICATIONS /LOGCLOSEAPPLICATIONS`

// UpgradeRunner is the runner's work, its effects behind functions so a test can play the setup and the services.
type UpgradeRunner struct {
	DataDir string
	Ticket  string
	Log     Logger
	Now     func() time.Time
	Sleep   func(time.Duration)
	// Protect gives the data directory its DACL before the result is written (best effort).
	Protect func() error
	// RunSetup runs setup silently with its log at logPath and returns its exit code. A setup that outlives timeout is
	// left running, with an error.
	RunSetup     func(setup, logPath string, timeout time.Duration) (int, error)
	ServiceState func(name string) (string, error)
	StartService func(name string) error
	// StopService stops a service (the daemon a person had stopped before the upgrade, which the setup started).
	StopService func(name string) error
	// Version is the installed provision.exe's build ("" when it does not answer).
	Version func(installDir string) string

	// Plan 088 D5, the health check and the one rollback (upgrade_health.go). Probe runs the installed provision's
	// state; Session reads the link's session marker; Fetch downloads a release's setup to dest and returns its
	// SHA-256 once it matches the release's SHA256SUMS; PickupDir is config.yaml's sendOrderPickupDir (DIG-1297).
	Probe      func(installDir string) (stateProbe, error)
	Session    func() (SessionMarker, error)
	Fetch      func(version, dest string) (string, error)
	PickupDir  func() string
	HTTPClient *http.Client  // Fetch's (link.yaml's proxy)
	HealthPoll time.Duration // between health rounds; 0 = 15 s
	// ListProcesses lists the processes named name, every session; KillProcess ends process pid only when its image is
	// image, checked on the handle that ends it (upgrade_close.go: the connector's windows are ended before each
	// setup). Nil ListProcesses: none is looked for.
	ListProcesses func(name string) ([]RunningProcess, error)
	KillProcess   func(pid uint32, image string) error
	// FormatFloor reads DataDir\format-floor (plan 088 D11): "" when none is recorded. Nil = formatfloor.Read. The
	// runner never rolls back below it, nor below its own compiled formatfloor.RollbackFloor.
	FormatFloor func() (string, error)
	// RelayReady asks the relay's /readyz from this box, through its proxy (plan 088 D12, review N5): nil only when it
	// answers 200 (its registry answering). Nil = relayReady on link.yaml's relay.
	RelayReady func() error
}

// NewUpgradeRunner is the runner on this machine.
func NewUpgradeRunner(dataDir, ticket string, log Logger) UpgradeRunner {
	ur := UpgradeRunner{
		DataDir: dataDir, Ticket: ticket, Log: log, Now: time.Now, Sleep: time.Sleep,
		Protect: func() error {
			_, err := dataacl.ProtectTree(dataDir, dataacl.TreeOptions{})
			return err
		},
		RunSetup: runSetupSilent,
		ServiceState: func(name string) (string, error) {
			info, err := autostart.WindowsServiceInfo(name)
			if err == nil && !info.Installed {
				return "not_installed", nil
			}
			return info.State, err
		},
		StartService:  autostart.StartWindowsService,
		StopService:   func(name string) error { return autostart.StopWindowsService(name, autostart.StopTimeout) },
		Version:       installedVersion,
		Probe:         probeState,
		Session:       func() (SessionMarker, error) { return ReadSessionMarker(dataDir) },
		PickupDir:     func() string { return pickupDirOf(dataDir) },
		HTTPClient:    runnerHTTPClient(dataDir),
		ListProcesses: listProcesses,
		KillProcess:   killProcess,
	}
	ur.Fetch = ur.fetchRelease
	ur.RelayReady = func() error { return relayReady(dataDir, ur.HTTPClient) }
	return ur
}

// RunUpgrade is `upgrade-run <ticket>`: the runner, logging to its ticket's runner.log and the Event Log. The exit code
// is 0 when a result was written.
func RunUpgrade(dataDir, ticket string) int {
	if !ticketRe.MatchString(ticket) {
		return 2
	}
	log, err := OpenFileLog(filepath.Join(dataDir, upgradeDirs.tickets, ticket, "runner.log"))
	if err != nil {
		return 1
	}
	defer func() { _ = log.Close() }()
	if _, err := NewUpgradeRunner(dataDir, ticket, log).Run(); err != nil {
		log.Error("link upgrade: no result was written", err)
		return 1
	}
	return 0
}

// Run does the upgrade and writes its result, which it also returns.
func (r UpgradeRunner) Run() (UpgradeResult, error) {
	res := UpgradeResult{Ticket: r.Ticket, ExitCode: -1, StartedAt: r.Now().UTC()}
	if !ticketRe.MatchString(r.Ticket) {
		return res, errors.New("not a ticket")
	}
	tdir := filepath.Join(r.DataDir, upgradeDirs.tickets, r.Ticket)
	held, err := r.lock()
	if err != nil {
		res.Error = "another upgrade runner holds the lock: " + err.Error()
		return res, r.write(res)
	}
	defer func() { _ = held.Unlock() }()
	// Review M1: provision's run lock, from before the setup until the services run again and the result is written
	// (the deferred unlock runs after write). The link let it go a moment ago; a provisioning run holding it now means
	// no setup.
	prov, err := lockProvision(r.DataDir, lockWait, r.Now, r.Sleep)
	if err != nil {
		res.Error = "a provisioning run holds DataDir\\provision\\.lock, so the setup was not run: " + err.Error()
		return res, r.write(res)
	}
	defer func() { _ = prov.Unlock() }()

	var job UpgradeJob
	b, err := os.ReadFile(filepath.Join(tdir, jobFile))
	if err == nil {
		err = json.Unmarshal(b, &job)
	}
	if err == nil && (job.Ticket != r.Ticket || filepath.Base(job.Setup) != job.Setup || job.Setup == "") {
		err = errors.New("the job names another ticket or a setup outside its folder")
	}
	if err != nil {
		res.Error = "the job cannot be read: " + err.Error()
		return res, r.write(res)
	}
	res.From, res.To = job.From, job.To
	res.SHA256, res.Auto, res.Policy = job.SHA256, job.Auto, job.Policy
	// Review m7: a restart of the server cut this ticket short after its setup; the link started its runner again.
	if m, found, err := readHealthMarker(tdir); found {
		return r.resume(job, tdir, res, m, err)
	}
	r.Log.Info(fmt.Sprintf("link upgrade %s: %s -> %s, the setup starts in %s", r.Ticket, job.From, job.To, answerGrace))
	r.Sleep(answerGrace)

	setup := filepath.Join(tdir, job.Setup)
	if got, err := fileSHA256(setup); err != nil || got != job.SHA256 {
		res.Error = fmt.Sprintf("the setup is not the one checked (%s, %v): not run", got, err)
		return res, r.write(res)
	}
	// Review m6: the link checked the order queue up to a quarter of an hour ago (the download, the grace): again now,
	// right before the setup restarts the connector — unless the person said assumeIdle.
	if !job.AssumeIdle {
		if n := r.queuedAfter(job, setupIdleWait); n > 0 {
			res.Error = fmt.Sprintf("orders_pending: %d order(s) queued right before the setup, still after %s: the setup was not run, "+
				"and nothing changed (the upgrade goes again at the next check, or with a new command)", n, setupIdleWait)
			res.Outcome = linkproto.OutcomeDeferred
			_ = os.Remove(setup)
			return res, r.write(res)
		}
	}
	// G-143: the connector's windows in other sessions, which the setup cannot close itself
	if res.ClosedApps = r.closeConnectorWindows(job.InstallDir); res.ClosedApps != nil && res.ClosedApps.StillOpen > 0 {
		res.Error = stillOpenError(res.ClosedApps, "the setup")
		_ = os.Remove(setup)
		return res, r.write(res)
	}
	logPath := filepath.Join(tdir, "setup.log")
	setupStarted := r.Now()
	code, err := r.RunSetup(setup, logPath, setupTimeout)
	setupEnded := err == nil // a setup still running is never followed by a health check, let alone a second setup
	res.ExitCode = code
	switch {
	case err != nil:
		res.Error = "the setup: " + err.Error()
	case code != 0:
		res.Error = fmt.Sprintf("the setup ended with %d: %s", code, innoExit(code))
	}
	r.Log.Info(fmt.Sprintf("link upgrade %s: the setup ended with %d", r.Ticket, code))

	var notAsBefore []string
	res.Services, notAsBefore = r.restoreServices(job)
	res.InstalledVersion = r.Version(job.InstallDir)
	if res.Error == "" {
		if v, ok := parseVersion(buildRe, res.InstalledVersion); !ok || v.String() != job.To {
			res.Error = fmt.Sprintf("the setup ended with 0 but the installed build is %q, not %s", res.InstalledVersion, job.To)
		}
	}
	if len(notAsBefore) > 0 {
		msg := "after the upgrade " + strings.Join(notAsBefore, "; ") + ": read setup.log, and logs (server, link, events-daemon) once the link answers"
		if res.Error == "" {
			res.Error = msg
		} else {
			res.Error += "; " + msg
		}
	}
	if res.Error != "" {
		res.SetupLogTail = tailLines(logPath, 40, 300)
	}
	_ = os.Remove(setup) // tens of MB; the log, the job and the result stay
	if setupEnded && job.HealthMinutes > 0 {
		r.afterSetup(job, tdir, &res, setupStarted, 0)
	}
	return r.finish(tdir, res)
}

// finish writes the result, then lets the health marker go: a ticket with a result is never resumed.
func (r UpgradeRunner) finish(tdir string, res UpgradeResult) (UpgradeResult, error) {
	res.FinishedAt = r.Now().UTC()
	res.Outcome = resultOutcome(res)
	err := r.write(res)
	if err == nil {
		_ = os.Remove(filepath.Join(tdir, healthMarkerFile))
	}
	return res, err
}

// afterSetup is plan 088 D5: healthy within healthMinutes, or the build it replaced once more (upgrade_health.go) — when
// nothing in §2b keeps the new build in place (noRollback). Its progress is in the ticket's health marker, so a restart
// of the server in the meantime is resumed (review m7) — the health check, never a cut rollback.
func (r UpgradeRunner) afterSetup(job UpgradeJob, tdir string, res *UpgradeResult, setupStarted time.Time, resumes int) {
	r.saveHealthMarker(tdir, healthMarker{Phase: markerHealth, SetupStarted: setupStarted, Resumes: resumes, Result: *res})
	res.Health = r.checkHealth(job, []string{job.To, job.From}, setupStarted)
	if res.Health.OK {
		return
	}
	msg := fmt.Sprintf("not healthy within %d min after the setup (%s)", job.HealthMinutes, failedChecks(res.Health))
	if outcome, why := r.noRollback(job, res.Health); outcome != "" {
		res.Outcome = outcome
		msg += "; " + why
		r.Log.Warn(fmt.Sprintf("link upgrade %s: no rollback: %s", r.Ticket, why))
	} else {
		r.saveHealthMarker(tdir, healthMarker{Phase: markerRollback, SetupStarted: setupStarted, Resumes: resumes, Result: *res})
		if res.Rollback = r.rollback(job, tdir, res); res.Rollback.OK {
			msg += fmt.Sprintf("; %s, the build it replaced, was installed again and is healthy", res.Rollback.To)
		} else {
			msg += fmt.Sprintf("; the rollback to %s failed: %s", res.Rollback.To, res.Rollback.Error)
		}
	}
	if res.Error == "" {
		res.Error = msg
	} else {
		res.Error += "; " + msg
	}
	if res.SetupLogTail == nil {
		res.SetupLogTail = tailLines(filepath.Join(tdir, "setup.log"), 40, 300)
	}
}

// The health marker (review m7): DataDir\upgrade\<ticket>\health.json, the runner's progress once the setup ended. A
// server restarted in the health window (Windows Update in the same night window) loses the runner; the link, at its
// next start, starts the ticket's runner again (resumeUpgrades), which goes on with the health check — once. A restart
// during the rollback, or a second one, is not resumed: the result says so, and a person decides.
const (
	healthMarkerFile = "health.json"
	markerHealth     = "health"
	markerRollback   = "rollback"
	maxResumes       = 1
)

type healthMarker struct {
	Phase        string        `json:"phase"`
	SetupStarted time.Time     `json:"setupStarted"`
	Resumes      int           `json:"resumes,omitempty"`
	Result       UpgradeResult `json:"result"` // as far as it got
}

func (r UpgradeRunner) saveHealthMarker(tdir string, m healthMarker) {
	b, _ := json.Marshal(m)
	if err := atomicfile.Write(filepath.Join(tdir, healthMarkerFile), b, 0o600); err != nil {
		r.Log.Error("link upgrade: writing the health marker (a restart now would not be resumed)", err)
	}
}

// readHealthMarker reads a ticket's health marker: found is true when one is there, even one that does not read.
func readHealthMarker(tdir string) (healthMarker, bool, error) {
	b, err := os.ReadFile(filepath.Join(tdir, healthMarkerFile))
	if errors.Is(err, os.ErrNotExist) {
		return healthMarker{}, false, nil
	}
	if err != nil {
		return healthMarker{}, true, err
	}
	var m healthMarker
	if err := json.Unmarshal(b, &m); err != nil {
		return healthMarker{}, true, err
	}
	return m, true, nil
}

// resume goes on with a ticket a restart cut short after its setup: the health check again, with its whole budget,
// and what follows. Not after a cut rollback, nor a second time.
func (r UpgradeRunner) resume(job UpgradeJob, tdir string, res UpgradeResult, m healthMarker, err error) (UpgradeResult, error) {
	if err == nil {
		res = m.Result
		res.Ticket, res.From, res.To = r.Ticket, job.From, job.To
	}
	why := ""
	switch {
	case err != nil:
		why = "its health marker does not read (" + err.Error() + ")"
	case m.Phase == markerRollback:
		why = "the rollback was under way"
	case m.Phase != markerHealth:
		why = fmt.Sprintf("the health marker names phase %q", m.Phase)
	case m.Resumes >= maxResumes:
		why = "it was resumed once already"
	}
	if why != "" {
		msg := "the server restarted while the runner was at work and " + why + ": the runner does not go on; read state, runner.log and " +
			"setup.log on the box — a person decides"
		if res.Error == "" {
			res.Error = msg
		} else {
			res.Error += "; " + msg
		}
		res.Outcome = linkproto.OutcomeUnhealthy
		r.Log.Warn(fmt.Sprintf("link upgrade %s: %s", r.Ticket, msg))
		return r.finish(tdir, res)
	}
	r.Log.Info(fmt.Sprintf("link upgrade %s: the server restarted after the setup; the health check starts again (%d min)", r.Ticket, job.HealthMinutes))
	if job.HealthMinutes > 0 {
		r.afterSetup(job, tdir, &res, m.SetupStarted, m.Resumes+1)
	}
	return r.finish(tdir, res)
}

// restoreServices puts the services back as they were before the upgrade, whatever the setup did (review m3): the link
// always running, the daemon running if it ran before — the setup starts both when it succeeds; when it fails after
// stopping them, nothing else would — and a daemon a person had stopped (a cutover, maintenance) stopped again, since
// the setup's [Run] starts it anyway. It returns each service's state and what is not as it was.
func (r UpgradeRunner) restoreServices(job UpgradeJob) (map[string]string, []string) {
	services := map[string]string{}
	var notAsBefore []string
	for _, s := range []struct {
		name string
		want bool
	}{{ServiceName, true}, {DaemonServiceName, job.DaemonWasRunning}} {
		st, err := r.ServiceState(s.name)
		switch {
		case err != nil:
		case s.want && st != "running" && st != "not_installed":
			// StartService also waits out a start_pending one
			if serr := r.StartService(s.name); serr != nil {
				r.Log.Error("link upgrade: starting "+s.name, serr)
			}
			st, err = r.ServiceState(s.name)
		case !s.want && st != "stopped" && st != "not_installed" && r.StopService != nil:
			if serr := r.StopService(s.name); serr != nil {
				r.Log.Error("link upgrade: stopping "+s.name+" again", serr)
			} else {
				r.Log.Info(fmt.Sprintf("link upgrade %s: %s was stopped before the upgrade; the setup started it, so it was stopped again", r.Ticket, s.name))
			}
			st, err = r.ServiceState(s.name)
		}
		if err != nil {
			st = "unknown: " + err.Error()
		}
		services[s.name] = st
		// Review m4: a setup that exits 0 (its sc.exe start always does) with a service not as it should be is a failure.
		if (s.want && st != "running") || (!s.want && st != "stopped" && st != "not_installed") {
			want := "running"
			if !s.want {
				want = "stopped"
			}
			notAsBefore = append(notAsBefore, fmt.Sprintf("%s is %s, not %s", s.name, st, want))
		}
	}
	return services, notAsBefore
}

func (r UpgradeRunner) lock() (*filelock.Held, error) {
	deadline := r.Now().Add(lockWait)
	for {
		h, err := filelock.Lock(lockPath(r.DataDir))
		if err == nil || !errors.Is(err, filelock.ErrLocked) || !r.Now().Before(deadline) {
			return h, err
		}
		r.Sleep(time.Second)
	}
}

// write puts the result in DataDir\results, never through a link: the data directory and results\ must be plain
// folders.
func (r UpgradeRunner) write(res UpgradeResult) error {
	if res.FinishedAt.IsZero() {
		res.FinishedAt = r.Now().UTC()
	}
	res.Outcome = resultOutcome(res)
	if r.Protect != nil {
		if err := r.Protect(); err != nil {
			r.Log.Error("link upgrade: protecting the data directory", err)
		}
	}
	dir := filepath.Join(r.DataDir, upgradeDirs.results)
	if err := dataacl.PlainDir(r.DataDir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := dataacl.PlainDir(dir); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	if err := atomicfile.Write(resultPath(r.DataDir, r.Ticket), append(b, '\n'), 0o600); err != nil {
		return err
	}
	if res.Error != "" {
		r.Log.Warn(fmt.Sprintf("link upgrade %s: %s -> %s failed: %s", r.Ticket, res.From, res.To, res.Error))
	} else {
		r.Log.Info(fmt.Sprintf("link upgrade %s: %s -> %s done", r.Ticket, res.From, res.To))
	}
	return nil
}

// innoExit says what an Inno Setup exit code means (its documented codes).
func innoExit(code int) string {
	switch code {
	case 1:
		return "the setup failed to initialize"
	case 2:
		return "the setup was cancelled before it installed anything"
	case 3:
		return "a fatal error while preparing the next step"
	case 4:
		return "a fatal error during the installation"
	case 5:
		return "the installation was cancelled"
	case 6:
		return "the setup was terminated"
	case 7:
		return "Preparing to Install stopped it: a service did not stop within 180 s, nothing was replaced (setupLogTail)"
	case 8:
		return "Preparing to Install needs a restart of the server first"
	}
	return "unknown"
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// tailLines is the last n lines of a file, each cut to width, without URL credentials.
func tailLines(path string, n, width int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err == nil && fi.Size() > 1<<20 {
		_, _ = f.Seek(fi.Size()-1<<20, io.SeekStart)
	}
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if len(line) > width {
			line = line[:width]
		}
		lines = append(lines, withoutCredentials(line))
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	return lines
}

// installedVersion asks the installed provision.exe for its build.
func installedVersion(installDir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, filepath.Join(installDir, "digi-erp-connector-provision.exe"), "version").Output()
	if err != nil {
		return ""
	}
	var e struct {
		Result struct {
			Version string `json:"version"`
		} `json:"result"`
	}
	if json.Unmarshal(out, &e) != nil {
		return ""
	}
	return e.Result.Version
}
