package link

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// healthRig is the runner rig with plan 088's health check on: a box on 1.0.31 upgraded to 1.0.32, whose official
// release of 1.0.31 (the rollback's) is served by a stand-in.
type healthRig struct {
	*runnerRig
	mu        sync.Mutex
	installed string // what provision version answers
	setups    []string
	oldSetup  []byte
	old       *fakeRelease
	probes    int
	// linkUp says whether the link of build installed is connected at now; probe answers provision state.
	linkUp func(installed string, now time.Time) bool
	probe  func(installed string) (stateProbe, error)
}

func healthyProbe(installed string) (stateProbe, error) {
	var p stateProbe
	p.OK = true
	p.Result.Daemon.Asked, p.Result.Daemon.Version, p.Result.Daemon.Health = true, "v"+installed, 200
	return p, nil
}

func newHealthRig(t *testing.T, daemonWasRunning bool, edit func(*UpgradeJob)) *healthRig {
	t.Helper()
	g := &healthRig{runnerRig: newRunnerRig(t, daemonWasRunning), installed: "1.0.31", oldSetup: []byte("MZ the 1.0.31 setup"),
		linkUp: func(string, time.Time) bool { return true }, probe: healthyProbe}
	// the runner that rolls back is v1.0.31's or later (plan 088): the rig upgrades 1.0.31 to 1.0.32
	_ = os.WriteFile(filepath.Join(g.tdir, setupFile("1.0.32")), g.setup, 0o600)
	g.old = newFakeRelease(t, "1.0.31", g.oldSetup, sumsFor(setupFile("1.0.31"), g.oldSetup))
	job := UpgradeJob{Ticket: g.ticket, From: "1.0.31", To: "1.0.32", SHA256: sha(g.setup), Setup: setupFile("1.0.32"),
		InstallDir: "install", DaemonWasRunning: daemonWasRunning, HealthMinutes: 10, FromSHA256: sha(g.oldSetup),
		Rollback: &linkproto.RollbackPin{Version: "1.0.31", SHA256: sha(g.oldSetup)}}
	if edit != nil {
		edit(&job)
	}
	b, _ := json.Marshal(job)
	_ = os.WriteFile(filepath.Join(g.tdir, jobFile), b, 0o600)
	g.r.RunSetup = func(setup, logPath string, _ time.Duration) (int, error) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.ran++
		body, _ := os.ReadFile(setup)
		g.setups = append(g.setups, filepath.Base(setup)+"="+string(body))
		_ = os.WriteFile(logPath, []byte("Log opened.\n"), 0o600)
		if strings.HasPrefix(filepath.Base(setup), "rollback-") {
			g.installed = "1.0.31"
		} else {
			g.installed = "1.0.32"
		}
		return 0, nil
	}
	g.r.Version = func(string) string { g.mu.Lock(); defer g.mu.Unlock(); return "v" + g.installed }
	g.r.Probe = func(string) (stateProbe, error) {
		g.mu.Lock()
		g.probes++
		installed := g.installed
		g.mu.Unlock()
		return g.probe(installed)
	}
	g.r.Session = func() (SessionMarker, error) {
		g.mu.Lock()
		installed := g.installed
		g.mu.Unlock()
		now := g.r.Now()
		return SessionMarker{Build: "v" + installed, Connected: g.linkUp(installed, now), Relay: "relay-1", Epoch: 7,
			ConnectedAt: now.Add(-10 * time.Second), LiveAt: now}, nil
	}
	return g
}

func (g *healthRig) run(t *testing.T) UpgradeResult {
	t.Helper()
	if _, err := g.r.Run(); err != nil {
		t.Fatal(err)
	}
	return g.result(t)
}

func TestTheRunnerChecksHealth(t *testing.T) {
	g := newHealthRig(t, true, nil)
	r := g.run(t)
	if r.Error != "" || r.Health == nil || !r.Health.OK || r.Health.Minutes != 10 || len(r.Health.Checks) != 4 || r.Rollback != nil ||
		r.SHA256 != sha(g.setup) || resultOutcome(r) != linkproto.OutcomeOK {
		t.Fatalf("result %+v health %+v", r, r.Health)
	}
	for _, c := range r.Health.Checks {
		if !c.OK || c.Detail == "" {
			t.Errorf("check %+v", c)
		}
	}
	if g.old.hit("v1.0.31/SHA256SUMS") != 0 || g.ran != 1 {
		t.Fatal("a healthy upgrade fetched or ran a rollback")
	}
}

// The new link takes a moment to connect: the runner waits for it, up to healthMinutes.
func TestTheRunnerWaitsForTheLink(t *testing.T) {
	g := newHealthRig(t, true, nil)
	var setupDone time.Time
	run := g.r.RunSetup
	g.r.RunSetup = func(s, l string, d time.Duration) (int, error) {
		c, err := run(s, l, d)
		setupDone = g.r.Now()
		return c, err
	}
	g.linkUp = func(_ string, now time.Time) bool { return now.Sub(setupDone) >= time.Minute }
	r := g.run(t)
	if !r.Health.OK || r.Health.Seconds < 60 || r.Rollback != nil {
		t.Fatalf("health %+v", r.Health)
	}
	polls := 0
	for _, d := range g.slept {
		if d == healthPoll {
			polls++
		}
	}
	if polls < 4 {
		t.Fatalf("slept %v", g.slept)
	}
}

func TestTheRunnerRollsBackAnUnhealthyUpgrade(t *testing.T) {
	g := newHealthRig(t, true, nil)
	g.linkUp = func(installed string, _ time.Time) bool { return installed == "1.0.31" } // 1.0.32's link never connects
	r := g.run(t)
	if r.Health == nil || r.Health.OK || r.Health.Seconds < 600 || r.Rollback == nil || !r.Rollback.OK {
		t.Fatalf("health %+v rollback %+v", r.Health, r.Rollback)
	}
	rb := r.Rollback
	if rb.To != "1.0.31" || rb.SHA256 != sha(g.oldSetup) || rb.ExitCode != 0 || rb.InstalledVersion != "v1.0.31" || rb.Health == nil ||
		!rb.Health.OK || rb.Services[ServiceName] != "running" || rb.StartedAt.IsZero() || rb.FinishedAt.Before(rb.StartedAt) {
		t.Fatalf("rollback %+v", rb)
	}
	if !strings.Contains(r.Error, "not healthy within 10 min after the setup (link: not connected to the relay)") ||
		!strings.Contains(r.Error, "1.0.31, the build it replaced, was installed again and is healthy") || resultOutcome(r) != linkproto.OutcomeRolledBack ||
		r.Outcome != linkproto.OutcomeRolledBack {
		t.Fatalf("error %q", r.Error)
	}
	if len(g.setups) != 2 || g.setups[1] != "rollback-"+setupFile("1.0.31")+"="+string(g.oldSetup) || r.RollbackSetupLogTail != nil {
		t.Fatalf("setups run %v, rollback log tail %q", g.setups, r.RollbackSetupLogTail)
	}
	if _, err := os.Stat(filepath.Join(g.tdir, "rollback-"+setupFile("1.0.31"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the rollback's setup was kept")
	}
	if !g.log.has("rolled back to 1.0.31, healthy") || !g.log.has("not healthy within 10 min") {
		t.Fatal("not logged")
	}
}

// The rollback installs exactly the build it replaced: its setup must match the signed digest (D8) and the release's
// SHA256SUMS. Otherwise nothing runs.
func TestTheRollbackChecksItsSetup(t *testing.T) {
	for _, c := range []struct {
		name string
		tune func(*healthRig)
		want string
	}{
		{"not the signed digest", func(g *healthRig) {}, "not " + strings.Repeat("cd", 32) + ", the signed digest: not run"},
		{"not the release's SHA256SUMS line", func(g *healthRig) {
			g.old.mu.Lock()
			g.old.files["v1.0.31/SHA256SUMS"] = []byte(strings.Repeat("ab", 32) + "  " + setupFile("1.0.31") + "\n")
			g.old.mu.Unlock()
		}, "SHA256SUMS lists"},
		{"no such release", func(g *healthRig) {
			g.old.mu.Lock()
			delete(g.old.files, "v1.0.31/SHA256SUMS")
			g.old.mu.Unlock()
		}, "release v1.0.31"},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newHealthRig(t, true, func(j *UpgradeJob) {
				if c.name == "not the signed digest" {
					j.FromSHA256, j.Rollback.SHA256 = "", strings.Repeat("cd", 32)
				}
			})
			c.tune(g)
			g.linkUp = func(string, time.Time) bool { return false }
			r := g.run(t)
			if r.Rollback == nil || r.Rollback.OK || !strings.Contains(r.Rollback.Error, c.want) || g.ran != 1 || resultOutcome(r) != linkproto.OutcomeUnhealthy ||
				r.Outcome != linkproto.OutcomeUnhealthy {
				t.Fatalf("rollback %+v, ran %d, outcome %q", r.Rollback, g.ran, r.Outcome)
			}
			if !strings.Contains(r.Error, "the rollback to 1.0.31 failed") {
				t.Fatalf("error %q", r.Error)
			}
		})
	}
}

// Review B1 (D8): with no signed digest for the build replaced there is no rollback — not on the digest this box
// recorded, not on the release's SHA256SUMS alone. The new build stays and a person decides.
func TestNoRollbackWithoutASignedDigest(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(*UpgradeJob)
	}{
		{"a box installed by hand: nothing recorded, nothing signed", func(j *UpgradeJob) { j.FromSHA256, j.Rollback = "", nil }},
		{"the recorded digest alone", func(j *UpgradeJob) { j.Rollback = nil }},
		{"a signed digest of another build", func(j *UpgradeJob) { j.Rollback.Version = "1.0.28" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newHealthRig(t, true, c.edit)
			g.linkUp = func(string, time.Time) bool { return false }
			r := g.run(t)
			if r.Rollback != nil || g.ran != 1 || g.old.hit("v1.0.31/SHA256SUMS") != 0 || g.old.hit("v1.0.31/"+setupFile("1.0.31")) != 0 {
				t.Fatalf("a rollback without a signed digest: %+v, ran %d", r.Rollback, g.ran)
			}
			if r.Outcome != linkproto.OutcomeNoRollback || resultOutcome(r) != linkproto.OutcomeNoRollback ||
				!strings.Contains(r.Error, "no signed digest names the setup of 1.0.31") || !g.log.has("no rollback: no signed digest") {
				t.Fatalf("outcome %q, error %q", r.Outcome, r.Error)
			}
		})
	}
}

// D8: the digest this box recorded is a cross-check — a signed digest that disagrees with it rolls back nothing.
func TestTheRecordedDigestIsACrossCheck(t *testing.T) {
	g := newHealthRig(t, true, func(j *UpgradeJob) { j.FromSHA256 = strings.Repeat("ef", 32) })
	g.linkUp = func(string, time.Time) bool { return false }
	r := g.run(t)
	if r.Rollback != nil || g.ran != 1 || r.Outcome != linkproto.OutcomeNoRollback || !strings.Contains(r.Error, "is not the one that installed it here") {
		t.Fatalf("rollback %+v, outcome %q, error %q", r.Rollback, r.Outcome, r.Error)
	}
}

// Review B1's scenario: whoever sits on the box's way out (a TLS-inspecting proxy, a compromised relay with a swapped
// asset) serves a setup of its own with a SHA256SUMS that lists it. The signed digest refuses it: it is never run.
func TestASubstitutedReleaseIsNeverRun(t *testing.T) {
	g := newHealthRig(t, true, func(j *UpgradeJob) { j.FromSHA256 = "" }) // installed by hand: only the signed pin knows it
	evil := []byte("MZ not the 1.0.31 setup")
	g.old.mu.Lock()
	g.old.files["v1.0.31/"+setupFile("1.0.31")] = evil
	g.old.files["v1.0.31/SHA256SUMS"] = []byte(sumsFor(setupFile("1.0.31"), evil))
	g.old.mu.Unlock()
	g.linkUp = func(string, time.Time) bool { return false }
	r := g.run(t)
	if g.ran != 1 || r.Rollback == nil || r.Rollback.OK || r.Outcome != linkproto.OutcomeUnhealthy || !strings.Contains(r.Rollback.Error, "the signed digest: not run") {
		t.Fatalf("ran %d, rollback %+v, outcome %q", g.ran, r.Rollback, r.Outcome)
	}
	for _, s := range g.setups {
		if strings.Contains(s, string(evil)) {
			t.Fatal("the substituted setup ran")
		}
	}
}

// At most one rollback: a box still unhealthy after it is left as the rollback's setup left it, and the result says so.
func TestOneRollbackOnly(t *testing.T) {
	g := newHealthRig(t, true, nil)
	g.linkUp = func(string, time.Time) bool { return false }
	r := g.run(t)
	if g.ran != 2 || r.Rollback == nil || r.Rollback.OK || !strings.Contains(r.Rollback.Error, "not healthy after it either") ||
		r.Rollback.Health == nil || r.Rollback.Health.OK || resultOutcome(r) != linkproto.OutcomeUnhealthy {
		t.Fatalf("ran %d rollback %+v", g.ran, r.Rollback)
	}
	if len(r.RollbackSetupLogTail) == 0 || r.RollbackSetupLogTail[0] != "Log opened." {
		t.Fatalf("the rollback's setup log: %q", r.RollbackSetupLogTail)
	}
}

func TestTheDaemonMustProveItself(t *testing.T) {
	g := newHealthRig(t, true, nil)
	g.probe = func(installed string) (stateProbe, error) {
		if installed == "1.0.32" {
			var p stateProbe
			p.OK, p.Result.Daemon.Asked, p.Result.Daemon.Error = true, true, "POST /auth/token answered 401"
			return p, nil
		}
		return healthyProbe(installed)
	}
	r := g.run(t)
	if r.Rollback == nil || !r.Rollback.OK || !strings.Contains(r.Error, "daemon: not proved: POST /auth/token answered 401") {
		t.Fatalf("result %+v", r)
	}
	// a daemon answering as another build is not this upgrade's
	g = newHealthRig(t, true, nil)
	g.probe = func(installed string) (stateProbe, error) { return healthyProbe("1.0.31") }
	if r := g.run(t); r.Rollback == nil || !strings.Contains(r.Error, `it answers as "v1.0.31", not the installed 1.0.32`) {
		t.Fatalf("result %+v", r)
	}
}

func TestAStoppedDaemonIsNotAsked(t *testing.T) {
	g := newHealthRig(t, false, nil)
	g.services[DaemonServiceName] = "stopped"
	r := g.run(t)
	if !r.Health.OK || g.probes != 0 || r.Services[DaemonServiceName] != "stopped" {
		t.Fatalf("health %+v, %d probes", r.Health, g.probes)
	}
}

// A setup still running when the runner stops waiting is never followed by a health check, let alone a second setup.
func TestNoHealthCheckWhileTheSetupRuns(t *testing.T) {
	g := newHealthRig(t, true, nil)
	g.r.RunSetup = func(string, string, time.Duration) (int, error) {
		g.ran++
		return -1, errors.New("still running after 45m0s; left running")
	}
	r := g.run(t)
	if r.Health != nil || r.Rollback != nil || g.ran != 1 || g.probes != 0 {
		t.Fatalf("result %+v", r)
	}
}

// DIG-1297: never below 1.0.25 on a box with a pickup folder, rollback or not. The format floor (1.0.30 on) is above it
// already, so this guard is reached only by the rollback itself.
func TestNoRollbackBelowThePickupRelease(t *testing.T) {
	g := newHealthRig(t, true, nil)
	g.r.PickupDir = func() string { return `C:\herp\pickup` }
	job := UpgradeJob{Ticket: g.ticket, From: "1.0.24", To: "1.0.32", InstallDir: "install", DaemonWasRunning: true, HealthMinutes: 10,
		Rollback: &linkproto.RollbackPin{Version: "1.0.24", SHA256: sha(g.oldSetup)}}
	rb := g.r.rollback(job, g.tdir, &UpgradeResult{})
	if rb.OK || !strings.Contains(rb.Error, "DIG-1297") || g.ran != 0 || g.old.hit("v1.0.24/SHA256SUMS") != 0 {
		t.Fatalf("rollback %+v ran %d", rb, g.ran)
	}
}

// D11 (review M3): never a rollback below the format floor — DataDir\format-floor as the new build raised it, or the
// runner's own compiled floor — nor while the floor cannot be read. The new build stays.
func TestNoRollbackBelowTheFormatFloor(t *testing.T) {
	for _, c := range []struct {
		name  string
		floor func() (string, error)
		edit  func(*UpgradeJob)
		want  string
	}{
		{"the new build raised the floor to its own", func() (string, error) { return "1.0.32", nil }, nil, "below the format floor 1.0.32"},
		{"the floor cannot be read", func() (string, error) { return "", errors.New("format floor unreadable") }, nil, "format-floor cannot be read"},
		{"the runner's compiled floor, none recorded", func() (string, error) { return "", nil }, func(j *UpgradeJob) {
			j.From, j.FromSHA256, j.Rollback.Version = "1.0.29", "", "1.0.29"
		}, "1.0.29, the build it replaced, is below the format floor 1.0.30"},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newHealthRig(t, true, c.edit)
			g.r.FormatFloor = c.floor
			g.linkUp = func(string, time.Time) bool { return false }
			r := g.run(t)
			if r.Rollback != nil || g.ran != 1 || g.old.hit("v1.0.31/SHA256SUMS") != 0 || r.Outcome != linkproto.OutcomeBelowFloor ||
				!strings.Contains(r.Error, c.want) {
				t.Fatalf("rollback %+v, ran %d, outcome %q, error %q", r.Rollback, g.ran, r.Outcome, r.Error)
			}
		})
	}
	// at the floor is not below it: the build replaced is installed again
	g := newHealthRig(t, true, nil)
	g.r.FormatFloor = func() (string, error) { return "1.0.31", nil }
	g.linkUp = func(installed string, _ time.Time) bool { return installed == "1.0.31" }
	if r := g.run(t); r.Outcome != linkproto.OutcomeRolledBack {
		t.Fatalf("at the floor: %q %q", r.Outcome, r.Error)
	}
	// the default reads DataDir\format-floor
	g = newHealthRig(t, true, nil)
	_ = os.WriteFile(filepath.Join(g.dir, "format-floor"), []byte("1.0.32\n"), 0o600)
	g.linkUp = func(string, time.Time) bool { return false }
	if r := g.run(t); r.Outcome != linkproto.OutcomeBelowFloor {
		t.Fatalf("the recorded floor: %q %q", r.Outcome, r.Error)
	}
}

// The rollback restarts the connector: it waits a while for its order queue to empty, then goes ahead.
func TestTheRollbackWaitsForTheQueue(t *testing.T) {
	g := newHealthRig(t, true, nil)
	g.linkUp = func(installed string, _ time.Time) bool { return installed == "1.0.31" }
	g.probe = func(installed string) (stateProbe, error) {
		p, _ := healthyProbe(installed)
		p.Result.Daemon.Queue = &struct {
			Queued  int `json:"queued"`
			Running int `json:"running"`
		}{Queued: 2}
		return p, nil
	}
	r := g.run(t)
	if r.Rollback == nil || !r.Rollback.OK || !g.log.has("2 order(s) still queued after 5m0s") {
		t.Fatalf("rollback %+v", r.Rollback)
	}
}

// The marker the old link left before the setup stopped it proves nothing about the new build: the build it names and
// a liveness answer after the setup started are both required.
func TestAStaleSessionMarkerIsNoProof(t *testing.T) {
	for _, c := range []struct {
		name   string
		marker func(g *healthRig, setupStart time.Time) SessionMarker
		detail string
	}{
		{"the old build's marker", func(g *healthRig, _ time.Time) SessionMarker {
			return SessionMarker{Build: "v1.0.31", Connected: true, LiveAt: g.r.Now()}
		}, `the link that last connected is build "v1.0.31", not the installed 1.0.32`},
		{"no liveness answer since the setup", func(g *healthRig, setupStart time.Time) SessionMarker {
			g.mu.Lock()
			defer g.mu.Unlock()
			return SessionMarker{Build: "v" + g.installed, Connected: true, LiveAt: setupStart.Add(-time.Minute)}
		}, "no word from the relay since the setup"},
		// the new link answered once and went quiet while the daemon was still starting: when the daemon proves
		// itself four minutes on, the link's last word is too old
		{"a liveness answer too long ago", func(g *healthRig, setupStart time.Time) SessionMarker {
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.installed == "1.0.31" {
				return SessionMarker{Build: "v1.0.31", Connected: true, LiveAt: g.r.Now()}
			}
			return SessionMarker{Build: "v1.0.32", Connected: true, LiveAt: setupStart.Add(time.Second)}
		}, "the relay last confirmed the connection"},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newHealthRig(t, true, nil)
			var setupStart time.Time
			run := g.r.RunSetup
			g.r.RunSetup = func(s, l string, d time.Duration) (int, error) {
				if setupStart.IsZero() {
					setupStart = g.r.Now()
				}
				return run(s, l, d)
			}
			g.r.Session = func() (SessionMarker, error) { return c.marker(g, setupStart), nil }
			g.probe = func(installed string) (stateProbe, error) {
				if installed == "1.0.32" && g.r.Now().Sub(setupStart) < 4*time.Minute {
					var p stateProbe
					p.OK, p.Result.Daemon.Error = true, "the connector did not answer on 127.0.0.1:7076"
					return p, nil
				}
				return healthyProbe(installed)
			}
			r := g.run(t)
			if r.Health.OK || !strings.Contains(r.Error, c.detail) || r.Rollback == nil {
				t.Fatalf("health %+v error %q", r.Health, r.Error)
			}
		})
	}
}
