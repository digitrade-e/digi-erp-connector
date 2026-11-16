package link

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/formatfloor"
	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// The runner's health check and its one rollback (plan 088 D5; closes plan 086's review m10). After the setup the
// runner waits up to the job's healthMinutes for the box to be healthy: both services as they were before, the daemon
// proving itself the way provision's state does (it logs in with config.yaml's credential and answers /api/ping with
// the installed build), and the installed build's link connected to the relay again (link\session.json, written by
// the link). Not healthy: it installs exactly the build it replaced, from that release (the compiled-in prefix only) —
// and only when a SIGNED digest names that build's setup (D8, review B1: the policy's rollbacks for an automatic
// upgrade, the command's spec for a manual one), which the release's SHA256SUMS line and the download must both equal.
// The digest this box recorded when that build was installed is a cross-check, never an authority: without a signed
// one the new build stays (unhealthy_no_rollback) and a person decides. That is the only downgrade the link ever
// makes, only for the ticket's own rollback, at most once: a second failure leaves the box as the rollback's setup
// left it, and the result says so.
//
// DIG-1297: a box that writes orders to a pickup folder is never taken below pickupMinVersion (1.0.25), which ignores
// it. The runner's own build is the build replaced and holds this code, so it is from 1.0.31 on: the guard is the rule
// written down, for the day that changes.

const (
	healthPoll       = 15 * time.Second
	sessionFresh     = 3 * time.Minute // the link confirms its connection every LiveEvery (60 s)
	rollbackIdleWait = 5 * time.Minute // a rollback waits this long for the order queue to empty before its setup
	setupIdleWait    = 5 * time.Minute // the upgrade's setup waits this long for it, then is deferred (review m6)
)

// stateProbe is the part of provision's state (`state --spec -`) the health check reads. These fields are a contract
// with every later build: the build before it runs that build's provision.exe to judge it.
type stateProbe struct {
	OK     bool `json:"ok"`
	Result struct {
		Daemon struct {
			Asked   bool   `json:"asked"`
			Version string `json:"version"`
			Health  int    `json:"health"`
			Queue   *struct {
				Queued  int `json:"queued"`
				Running int `json:"running"`
			} `json:"queue"`
			Error string `json:"error"`
		} `json:"daemon"`
	} `json:"result"`
}

// probeState runs the installed provision.exe's state.
func probeState(installDir string) (stateProbe, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(installDir, "digi-erp-connector-provision.exe"), "state", "--spec", "-")
	cmd.Stdin = strings.NewReader("{}")
	var out limited
	out.max = linkproto.MaxPayload
	cmd.Stdout = &out
	err := cmd.Run()
	var p stateProbe
	if jerr := json.Unmarshal(out.b.Bytes(), &p); jerr != nil {
		if err == nil {
			err = jerr
		}
		return stateProbe{}, err
	}
	return p, nil
}

func (r UpgradeRunner) probe(installDir string) (stateProbe, error) {
	if r.Probe == nil {
		return probeState(installDir)
	}
	return r.Probe(installDir)
}

func (r UpgradeRunner) session() (SessionMarker, error) {
	if r.Session == nil {
		return ReadSessionMarker(r.DataDir)
	}
	return r.Session()
}

// checkHealth waits up to job.HealthMinutes for a healthy round: the installed build one of accept, the services as
// before the upgrade, the daemon proved, the link connected since since.
func (r UpgradeRunner) checkHealth(job UpgradeJob, accept []string, since time.Time) *linkproto.UpgradeHealth {
	h := &linkproto.UpgradeHealth{Minutes: job.HealthMinutes}
	start := r.Now()
	deadline := start.Add(time.Duration(job.HealthMinutes) * time.Minute)
	poll := r.HealthPoll
	if poll <= 0 {
		poll = healthPoll
	}
	for {
		h.Checks = r.healthRound(job, accept, since)
		h.OK = !slices.ContainsFunc(h.Checks, func(c linkproto.HealthCheck) bool { return !c.OK })
		if h.OK || !r.Now().Before(deadline) {
			break
		}
		r.Sleep(poll)
	}
	h.Seconds = int64(r.Now().Sub(start).Seconds())
	if h.OK {
		r.Log.Info(fmt.Sprintf("link upgrade %s: healthy after %d s", r.Ticket, h.Seconds))
	} else {
		r.Log.Warn(fmt.Sprintf("link upgrade %s: not healthy within %d min: %s", r.Ticket, job.HealthMinutes, failedChecks(h)))
	}
	return h
}

// healthRound is one look at the box.
func (r UpgradeRunner) healthRound(job UpgradeJob, accept []string, since time.Time) []linkproto.HealthCheck {
	checks := make([]linkproto.HealthCheck, 0, 4)
	add := func(name string, ok bool, format string, a ...any) {
		checks = append(checks, linkproto.HealthCheck{Name: name, OK: ok, Detail: withoutCredentials(fmt.Sprintf(format, a...))})
	}
	installed := releaseOf(r.Version(job.InstallDir))
	add("build", installed != "" && slices.Contains(accept, installed), "installed %s (expected %s)", orNone(installed), strings.Join(accept, " or "))

	linkState, lerr := r.ServiceState(ServiceName)
	daemonState, derr := r.ServiceState(DaemonServiceName)
	daemonOK := daemonState == "running"
	if !job.DaemonWasRunning {
		daemonOK = daemonState == "stopped" || daemonState == "not_installed"
	}
	add("services", lerr == nil && linkState == "running" && derr == nil && daemonOK, "%s %s, %s %s (it was %s)",
		ServiceName, stateOr(linkState, lerr), DaemonServiceName, stateOr(daemonState, derr), runningOr(job.DaemonWasRunning))

	if job.DaemonWasRunning {
		p, err := r.probe(job.InstallDir)
		d := p.Result.Daemon
		switch {
		case err != nil:
			add("daemon", false, "provision state: %v", err)
		case !p.OK:
			add("daemon", false, "provision state refused")
		case d.Error != "" || !d.Asked:
			add("daemon", false, "not proved: %s", orNone(d.Error))
		case releaseOf(d.Version) != installed:
			add("daemon", false, "it answers as %q, not the installed %s", d.Version, orNone(installed))
		default:
			add("daemon", true, "logged in; /api/ping %s, /api/health %d", d.Version, d.Health)
		}
	} else {
		add("daemon", true, "stopped before the upgrade: not asked")
	}

	m, err := r.session()
	now := r.Now()
	switch {
	case err != nil:
		add("link", false, "link\\session.json: %v", err)
	case !m.Connected:
		add("link", false, "not connected to the relay")
	case releaseOf(m.Build) != installed:
		add("link", false, "the link that last connected is build %q, not the installed %s", m.Build, orNone(installed))
	case m.LiveAt.Before(since):
		add("link", false, "no word from the relay since the setup")
	case now.Sub(m.LiveAt) > sessionFresh:
		add("link", false, "the relay last confirmed the connection %s ago", now.Sub(m.LiveAt).Round(time.Second))
	default:
		add("link", true, "connected to %s (epoch %d) since %s", m.Relay, m.Epoch, m.ConnectedAt.UTC().Format(time.RFC3339))
	}
	return checks
}

// noRollback says why an unhealthy upgrade keeps the new build, in plan 088 §2b's words: the outcome and the reason,
// or "" when the rollback may run.
func (r UpgradeRunner) noRollback(job UpgradeJob, h *linkproto.UpgradeHealth) (outcome, why string) {
	// D12: when only the new link failed, the build is to blame only if the relay is ready from this box — its registry
	// answering (review N5: /healthz says nothing about whether the relay can admit a box); a relay that is down or not
	// ready (an outage, GKE maintenance, its registry gone) or a way out that is cut must not roll back every box
	// upgrading in its window.
	if linkOnly(h) {
		check := r.RelayReady
		if check == nil {
			check = func() error { return relayReady(r.DataDir, r.HTTPClient) }
		}
		if err := check(); err != nil {
			return linkproto.OutcomeLinkInconclusive, fmt.Sprintf("only the new link failed, and the relay's /readyz did not answer 200 from this box "+
				"(%v): the relay, its registry or the way to it is down — not proven the new build (D12); the new build stays", withoutCredentials(err.Error()))
		}
		r.Log.Info(fmt.Sprintf("link upgrade %s: only the new link failed, and the relay is ready: the new build is to blame", r.Ticket))
	}
	pin := job.Rollback
	switch {
	case pin == nil || releaseOf(pin.Version) != job.From || !sha256Re.MatchString(pin.SHA256):
		return linkproto.OutcomeNoRollback, fmt.Sprintf("no signed digest names the setup of %s, the build it replaced (D8): "+
			"the new build stays, and a person decides", orNone(job.From))
	case job.FromSHA256 != "" && job.FromSHA256 != pin.SHA256:
		return linkproto.OutcomeNoRollback, fmt.Sprintf("the signed digest of %s's setup (%s) is not the one that installed it here (%s): "+
			"the new build stays, and a person decides", job.From, pin.SHA256, job.FromSHA256)
	}
	// D11: never below the format floor — the build replaced may not read what the new one wrote meanwhile.
	floor, err := r.formatFloor()
	switch {
	case err != nil:
		return linkproto.OutcomeBelowFloor, fmt.Sprintf("DataDir\\format-floor cannot be read (%v), so no rollback can be judged safe for the data (D11): "+
			"the new build stays, and a person decides", err)
	case formatfloor.Below(job.From, floor):
		return linkproto.OutcomeBelowFloor, fmt.Sprintf("%s, the build it replaced, is below the format floor %s (D11): it may not read what the "+
			"new build wrote; the new build stays, and a person decides", job.From, floor)
	}
	return "", ""
}

// linkOnly reports whether the link is the only check of h that failed.
func linkOnly(h *linkproto.UpgradeHealth) bool {
	if h == nil || h.OK {
		return false
	}
	failed := 0
	for _, c := range h.Checks {
		if !c.OK {
			if c.Name != "link" {
				return false
			}
			failed++
		}
	}
	return failed > 0
}

// relayReadyTimeout bounds the runner's question to the relay (D12).
const relayReadyTimeout = 30 * time.Second

// relayReadyURL is the relay's /readyz for its /link URL: wss → https, ws → http, the same host, no user or query. The
// relay answers it 200 only when its registry answers (review N5); it is never its liveness probe.
func relayReadyURL(relay string) (string, error) {
	u, err := url.Parse(relay)
	if err != nil {
		return "", fmt.Errorf("the relay URL does not parse: %w", errors.New(withoutCredentials(err.Error())))
	}
	switch u.Scheme {
	case "wss", "https":
		u.Scheme = "https"
	case "ws", "http":
		u.Scheme = "http"
	default:
		return "", fmt.Errorf("the relay URL's scheme %q is not ws or wss", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("the relay URL names no host")
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/readyz"}).String(), nil
}

// relayReady asks link.yaml's relay for /readyz through client (the link's proxy): nil only when it answers 200.
// Anything else — another status (a relay before /readyz answers 404), no answer — is "not ready": D12 then keeps the
// new build.
func relayReady(dataDir string, client *http.Client) error {
	st, err := (&Store{Dir: dataDir}).LoadState()
	if err != nil {
		return fmt.Errorf("link.yaml: %w", err)
	}
	target, err := relayReadyURL(st.RelayURL())
	if err != nil {
		return err
	}
	if client == nil {
		client = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(context.Background(), relayReadyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s answered %d", target, resp.StatusCode)
	}
	return nil
}

// formatFloor is the floor a rollback may not go below: DataDir\format-floor or this runner's compiled one, the higher.
func (r UpgradeRunner) formatFloor() (string, error) {
	read := r.FormatFloor
	if read == nil {
		read = func() (string, error) { return formatfloor.Read(r.DataDir) }
	}
	recorded, err := read()
	if err != nil {
		return "", err
	}
	return formatfloor.Max(recorded, formatfloor.RollbackFloor), nil
}

// rollback installs the build job replaced again, once (see the top of this file), to the signed digest noRollback
// found. The connector windows it ended before its setup go to res.RollbackClosedApps.
func (r UpgradeRunner) rollback(job UpgradeJob, tdir string, res *UpgradeResult) *linkproto.UpgradeRollback {
	rb := &linkproto.UpgradeRollback{To: job.From, ExitCode: -1, StartedAt: r.Now().UTC()}
	done := func() *linkproto.UpgradeRollback {
		rb.FinishedAt = r.Now().UTC()
		rb.OK = rb.Error == ""
		if rb.OK {
			r.Log.Info(fmt.Sprintf("link upgrade %s: rolled back to %s, healthy", r.Ticket, rb.To))
		} else {
			r.Log.Warn(fmt.Sprintf("link upgrade %s: the rollback to %s failed: %s", r.Ticket, rb.To, rb.Error))
		}
		return rb
	}
	to, ok := parseVersion(versionRe, job.From)
	if !ok {
		rb.Error = fmt.Sprintf("the build replaced (%q) is not a release: there is nothing to install", job.From)
		return done()
	}
	if to.less(pickupMinVersion) && r.PickupDir != nil && strings.TrimSpace(r.PickupDir()) != "" {
		rb.Error = fmt.Sprintf("this box writes orders to a pickup folder (DIG-1297), which %s ignores: not installed", to)
		return done()
	}
	r.Log.Info(fmt.Sprintf("link upgrade %s: rolling back to %s", r.Ticket, to))
	setup := filepath.Join(tdir, "rollback-"+setupFile(to.String()))
	defer func() { _ = os.Remove(setup) }()
	fetch := r.Fetch
	if fetch == nil {
		fetch = r.fetchRelease
	}
	sha, err := fetch(to.String(), setup)
	if err != nil {
		rb.Error = withoutCredentials(fmt.Sprintf("release v%s: %v", to, err))
		return done()
	}
	if job.Rollback == nil || sha != job.Rollback.SHA256 {
		want := ""
		if job.Rollback != nil {
			want = job.Rollback.SHA256
		}
		rb.Error = fmt.Sprintf("release v%s's setup has SHA-256 %s, not %s, the signed digest: not run", to, sha, orNone(want))
		return done()
	}
	rb.SHA256 = sha
	if note := r.waitIdle(job); note != "" {
		r.Log.Warn(fmt.Sprintf("link upgrade %s: %s", r.Ticket, note))
	}
	if got, err := fileSHA256(setup); err != nil || got != sha {
		rb.Error = fmt.Sprintf("the rollback's setup is not the one checked (%s, %v): not run", got, err)
		return done()
	}
	if res.RollbackClosedApps = r.closeConnectorWindows(job.InstallDir); res.RollbackClosedApps != nil && res.RollbackClosedApps.StillOpen > 0 {
		rb.Error = stillOpenError(res.RollbackClosedApps, "the rollback's setup")
		return done()
	}
	started := r.Now()
	logPath := filepath.Join(tdir, "rollback-setup.log")
	defer func() {
		if !rb.OK && rb.ExitCode != -1 {
			res.RollbackSetupLogTail = tailLines(logPath, 40, 300)
		}
	}()
	code, err := r.RunSetup(setup, logPath, setupTimeout)
	rb.ExitCode = code
	if err != nil {
		rb.Error = "the rollback's setup: " + err.Error()
		return done()
	}
	services, notAsBefore := r.restoreServices(job)
	rb.Services = services
	rb.InstalledVersion = r.Version(job.InstallDir)
	rb.Health = r.checkHealth(job, []string{to.String()}, started)
	var why []string
	if code != 0 {
		why = append(why, fmt.Sprintf("its setup ended with %d: %s", code, innoExit(code)))
	}
	if releaseOf(rb.InstalledVersion) != to.String() {
		why = append(why, fmt.Sprintf("the installed build is %q, not %s", rb.InstalledVersion, to))
	}
	why = append(why, notAsBefore...)
	if !rb.Health.OK {
		why = append(why, "not healthy after it either: "+failedChecks(rb.Health))
	}
	rb.Error = strings.Join(why, "; ")
	return done()
}

// waitIdle waits up to rollbackIdleWait for the daemon's order queue to empty (the setup restarts it). A daemon that
// does not answer holds nothing. The note says why it went ahead anyway.
func (r UpgradeRunner) waitIdle(job UpgradeJob) string {
	if n := r.queuedAfter(job, rollbackIdleWait); n > 0 {
		return fmt.Sprintf("%d order(s) still queued after %s; the rollback goes ahead (the connector's stop drains its queue, up to 120 s)",
			n, rollbackIdleWait)
	}
	return ""
}

// queuedAfter waits up to wait for the daemon's order queue to empty and returns what is still queued then: 0 when it
// is idle, when the daemon was stopped before the upgrade, or when it does not answer (it holds nothing then).
func (r UpgradeRunner) queuedAfter(job UpgradeJob, wait time.Duration) int {
	if !job.DaemonWasRunning {
		return 0
	}
	deadline := r.Now().Add(wait)
	for {
		p, err := r.probe(job.InstallDir)
		q := p.Result.Daemon.Queue
		if err != nil || !p.OK || q == nil || q.Queued+q.Running == 0 {
			return 0
		}
		if !r.Now().Before(deadline) {
			return q.Queued + q.Running
		}
		r.Sleep(healthPoll)
	}
}

// fetchRelease downloads release version's setup to dest and returns its SHA-256, which must equal the release's
// SHA256SUMS line (D2's check, for the rollback).
func (r UpgradeRunner) fetchRelease(version, dest string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), downloadDeadline)
	defer cancel()
	base := releasePrefix + "v" + version + "/"
	listed, err := releaseSum(ctx, r.HTTPClient, base+sumsFile, setupFile(version))
	if err != nil {
		return "", err
	}
	_ = os.Remove(dest) // a copy left by an earlier try
	got, err := download(ctx, r.HTTPClient, base+setupFile(version), dest)
	if err != nil {
		return "", err
	}
	if got != listed {
		_ = os.Remove(dest)
		return "", fmt.Errorf("the downloaded setup has SHA-256 %s, but the release's SHA256SUMS lists %s", got, listed)
	}
	return got, nil
}

// runnerHTTPClient is the link's way out for the runner: link.yaml's proxy, else the machine's.
func runnerHTTPClient(dataDir string) *http.Client {
	st, _ := (&Store{Dir: dataDir}).LoadState()
	if c, err := HTTPClientVia(&ProxyFinder{Explicit: st.Proxy}); err == nil {
		return c
	}
	c, _ := HTTPClientVia(&ProxyFinder{})
	return c
}

// pickupDirOf is config.yaml's sendOrderPickupDir ("" when there is none or it cannot be read).
func pickupDirOf(dataDir string) string {
	b, err := os.ReadFile(filepath.Join(dataDir, "config.yaml"))
	if err != nil {
		return ""
	}
	cfg, err := config.Parse(b)
	if err != nil {
		return ""
	}
	return cfg.SendOrderPickupDir
}

// failedChecks names the checks of h that failed, in one line.
func failedChecks(h *linkproto.UpgradeHealth) string {
	var out []string
	for _, c := range h.Checks {
		if !c.OK {
			out = append(out, c.Name+": "+c.Detail)
		}
	}
	return strings.Join(out, "; ")
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func stateOr(state string, err error) string {
	if err != nil {
		return "unknown (" + err.Error() + ")"
	}
	return orNone(state)
}

func runningOr(running bool) string {
	if running {
		return "running"
	}
	return "stopped"
}
