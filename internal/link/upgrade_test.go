package link

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/filelock"
	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// --- a GitHub release stand-in ------------------------------------------------------------------------------------

type fakeRelease struct {
	srv   *httptest.Server
	mu    sync.Mutex
	files map[string][]byte // path under the prefix → body
	hits  map[string]int
}

func newFakeRelease(t *testing.T, version string, setup []byte, sums string) *fakeRelease {
	r := &fakeRelease{files: map[string][]byte{}, hits: map[string]int{}}
	r.files["v"+version+"/"+setupFile(version)] = setup
	r.files["v"+version+"/SHA256SUMS"] = []byte(sums)
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/download/", func(w http.ResponseWriter, req *http.Request) {
		p := strings.TrimPrefix(req.URL.Path, "/releases/download/")
		r.mu.Lock()
		r.hits[p]++
		_, ok := r.files[p]
		r.mu.Unlock()
		if !ok {
			http.NotFound(w, req)
			return
		}
		// GitHub answers a release file with a redirect to its object store
		http.Redirect(w, req, "/objects/"+p, http.StatusFound)
	})
	mux.HandleFunc("/objects/", func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		b := r.files[strings.TrimPrefix(req.URL.Path, "/objects/")]
		r.mu.Unlock()
		_, _ = w.Write(b)
	})
	r.srv = httptest.NewServer(mux)
	t.Cleanup(r.srv.Close)
	old := releasePrefix
	releasePrefix = r.srv.URL + "/releases/download/"
	t.Cleanup(func() { releasePrefix = old })
	return r
}

func (r *fakeRelease) hit(p string) int { r.mu.Lock(); defer r.mu.Unlock(); return r.hits[p] }

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func sumsFor(name string, b []byte) string {
	return "aa" + strings.Repeat("0", 62) + "  digi-erp-connector.exe\n" + sha(b) + " *" + name + "\n"
}

// stateRunner answers provision state with the given daemon state and queue.
type stateRunner struct {
	mu      sync.Mutex
	state   string // the daemon service
	queue   string // JSON of daemon.queue, or "" for none
	fail    bool
	called  int
	onState func() // called with each state run
}

func (r *stateRunner) Run(_ context.Context, argv []string, _ []byte, _ int) ([]byte, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.called++
	if r.onState != nil {
		r.onState()
	}
	if strings.Join(argv, " ") != "state --spec -" {
		return nil, -1, fmt.Errorf("unexpected %v", argv)
	}
	if r.fail {
		return []byte(`{"command":"state","ok":false}`), 1, nil
	}
	q := `"error":"GET /api/sendOrder answered 404"`
	if r.queue != "" {
		q = `"queue":` + r.queue
	}
	return []byte(`{"command":"state","ok":true,"result":{"services":{"daemon":{"state":"` + r.state + `"}},"daemon":{"asked":true,` + q + `}}}`), 0, nil
}

func (r *stateRunner) calls() int { r.mu.Lock(); defer r.mu.Unlock(); return r.called }

type spawned struct {
	mu    sync.Mutex
	calls [][]string
	err   error
}

func (s *spawned) spawn(exe string, args []string, dir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, append([]string{exe, dir}, args...))
	return s.err
}

type upgradeRig struct {
	b       *box
	rc      *relayConn
	priv    []byte
	dir     string
	rel     *fakeRelease
	setup   []byte
	runner  *stateRunner
	spawned *spawned
}

func newUpgradeRig(t *testing.T, build string, extra ...func(*Config)) *upgradeRig {
	t.Helper()
	g := &upgradeRig{dir: t.TempDir(), setup: []byte("MZ the 1.0.30 setup"), runner: &stateRunner{state: "running", queue: `{"queued":0,"running":0}`}, spawned: &spawned{}}
	g.rel = newFakeRelease(t, "1.0.30", g.setup, sumsFor(setupFile("1.0.30"), g.setup))
	self := filepath.Join(t.TempDir(), "digi-erp-connector-link.exe")
	_ = os.WriteFile(self, []byte("MZ the link"), 0o700)
	r := newRelay(t)
	priv, kr := team(t, 1)
	g.priv = priv
	g.b, g.rc = pairedBox(t, r, "bfl-read", kr, "127.0.0.1:1", func(c *Config) {
		c.Build, c.Runner, c.InstallDir = build, g.runner, `C:\Program Files\digi-erp-connector`
		c.Upgrade = UpgradeConfig{DataDir: g.dir, SelfExe: self, Spawn: g.spawned.spawn}
		for _, f := range extra {
			f(c)
		}
	})
	return g
}

func (g *upgradeRig) send(t *testing.T, command, stdin string) ([]byte, linkproto.Answer, linkproto.Envelope) {
	t.Helper()
	raw, e := envelope(g.priv, "team-1", "bfl-read", command, nil, []byte(stdin), time.Now())
	out, sa := send(t, g.rc, raw, []byte(stdin))
	return out, answerOf(sa), e
}

func (g *upgradeRig) upgrade(t *testing.T, stdin string) (UpgradeAnswer, int, string) {
	t.Helper()
	out, a, e := g.send(t, CommandUpgrade, stdin)
	if a.Refusal != "" {
		t.Fatalf("the link refused the envelope: %s", a.Refusal)
	}
	var ans UpgradeAnswer
	if err := json.Unmarshal(out, &ans); err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	return ans, a.Exit, TicketFor(e.Nonce)
}

func (g *upgradeRig) spec(version, digest string) string {
	return fmt.Sprintf(`{"version":%q,"sha256":%q}`, version, digest)
}

// --- the tests ------------------------------------------------------------------------------------------------------

func TestUpgradeIsAccepted(t *testing.T) {
	g := newUpgradeRig(t, "v1.0.29")
	ans, exit, ticket := g.upgrade(t, g.spec("1.0.30", sha(g.setup)))
	if exit != 0 || !ans.Accepted || ans.Ticket != ticket || ans.From != "1.0.29" || ans.To != "1.0.30" || ans.Refusal != nil {
		t.Fatalf("answer %+v exit %d", ans, exit)
	}
	tdir := filepath.Join(g.dir, "upgrade", ticket)
	got, _ := os.ReadFile(filepath.Join(tdir, setupFile("1.0.30")))
	if string(got) != string(g.setup) {
		t.Fatal("the setup was not downloaded into the ticket's folder")
	}
	if b, _ := os.ReadFile(filepath.Join(tdir, runnerExe)); string(b) != "MZ the link" {
		t.Fatal("the runner is not a copy of the link")
	}
	var job UpgradeJob
	b, _ := os.ReadFile(filepath.Join(tdir, jobFile))
	if err := json.Unmarshal(b, &job); err != nil || job.Ticket != ticket || job.From != "1.0.29" || job.To != "1.0.30" ||
		job.SHA256 != sha(g.setup) || job.Setup != setupFile("1.0.30") || !job.DaemonWasRunning || job.By != "ilya" ||
		job.InstallDir != `C:\Program Files\digi-erp-connector` {
		t.Fatalf("job %+v (%v)", job, err)
	}
	if len(g.spawned.calls) != 1 || strings.Join(g.spawned.calls[0], "|") != filepath.Join(tdir, runnerExe)+"|"+tdir+"|upgrade-run|"+ticket {
		t.Fatalf("spawned %v", g.spawned.calls)
	}
	if g.b.runner.count() != 0 || !g.b.log.has("upgrade 1.0.29 -> 1.0.30 accepted") {
		t.Fatal("provision ran a command, or nothing was logged")
	}
	// while it runs, a second upgrade is refused and the result is pending
	ans, exit, _ = g.upgrade(t, g.spec("1.0.30", sha(g.setup)))
	if exit != 1 || ans.Refusal == nil || ans.Refusal.Code != "upgrade_running" {
		t.Fatalf("a second upgrade: %+v", ans)
	}
	out, a, _ := g.send(t, CommandUpgradeResult, `{"ticket":"`+ticket+`"}`)
	if a.Exit != 0 || string(out) != `{"pending":true,"ticket":"`+ticket+`"}` {
		t.Fatalf("result while running: %s", out)
	}
}

func TestUpgradeRefusals(t *testing.T) {
	setup := []byte("MZ the 1.0.30 setup")
	good := sha(setup)
	other := strings.Repeat("ab", 32)
	cases := []struct {
		name  string
		build string
		stdin string
		tune  func(*upgradeRig)
		code  string
	}{
		{"not JSON", "v1.0.29", `{"version":`, nil, "spec_invalid"},
		{"an unknown field", "v1.0.29", `{"version":"1.0.30","sha256":"` + good + `","url":"https://evil.example/x.exe"}`, nil, "spec_invalid"},
		{"not a version", "v1.0.29", `{"version":"1.0","sha256":"` + good + `"}`, nil, "spec_invalid"},
		{"a path in the version", "v1.0.29", `{"version":"1.0.30/../../x","sha256":"` + good + `"}`, nil, "spec_invalid"},
		{"not a digest", "v1.0.29", `{"version":"1.0.30","sha256":"abc"}`, nil, "spec_invalid"},
		{"the same version", "v1.0.30", `{"version":"1.0.30","sha256":"` + good + `"}`, nil, "not_newer"},
		{"an older version", "v1.0.31", `{"version":"1.0.30","sha256":"` + good + `"}`, nil, "not_newer"},
		{"a dev build", "dev", `{"version":"1.0.30","sha256":"` + good + `"}`, nil, "version_unknown"},
		{"orders queued", "v1.0.29", `{"version":"1.0.30","sha256":"` + good + `"}`, func(g *upgradeRig) { g.runner.queue = `{"queued":2,"running":1}` }, "orders_pending"},
		{"the queue unreadable", "v1.0.29", `{"version":"1.0.30","sha256":"` + good + `"}`, func(g *upgradeRig) { g.runner.queue = "" }, "orders_unknown"},
		{"state refused", "v1.0.29", `{"version":"1.0.30","sha256":"` + good + `"}`, func(g *upgradeRig) { g.runner.fail = true }, "orders_unknown"},
		{"the pin is not the release's", "v1.0.29", `{"version":"1.0.30","sha256":"` + other + `"}`, nil, "sha256_mismatch"},
		{"no such release", "v1.0.29", `{"version":"1.0.31","sha256":"` + good + `"}`, nil, "download_failed"},
		{"the setup is not listed", "v1.0.29", `{"version":"1.0.30","sha256":"` + good + `"}`, func(g *upgradeRig) {
			g.rel.files["v1.0.30/SHA256SUMS"] = []byte(good + "  digi-erp-connector.exe\n")
		}, "release_unlisted"},
		{"the download is not what is listed", "v1.0.29", `{"version":"1.0.30","sha256":"` + good + `"}`, func(g *upgradeRig) {
			g.rel.files["v1.0.30/"+setupFile("1.0.30")] = []byte("MZ something else")
		}, "sha256_mismatch"},
		{"the runner does not start", "v1.0.29", `{"version":"1.0.30","sha256":"` + good + `"}`, func(g *upgradeRig) { g.spawned.err = errors.New("access denied") }, "runner_failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := newUpgradeRig(t, c.build)
			if c.tune != nil {
				c.tune(g)
			}
			ans, exit, ticket := g.upgrade(t, c.stdin)
			if exit != 1 || ans.Accepted || ans.Refusal == nil || ans.Refusal.Code != c.code {
				t.Fatalf("answer %+v exit %d, want %s", ans, exit, c.code)
			}
			if _, err := os.Stat(filepath.Join(g.dir, "upgrade", ticket)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("a refused upgrade left its ticket's folder")
			}
			if c.code != "runner_failed" && len(g.spawned.calls) != 0 {
				t.Fatal("the runner started")
			}
			if c.code == "sha256_mismatch" && strings.Contains(c.name, "pin") && g.rel.hit("v1.0.30/"+setupFile("1.0.30")) != 0 {
				t.Fatal("the setup was downloaded although its pin did not match the release")
			}
		})
	}
}

func TestUpgradeAssumeIdleAndAStoppedDaemon(t *testing.T) {
	g := newUpgradeRig(t, "v1.0.29")
	g.runner.queue = `{"queued":3,"running":0}`
	ans, exit, ticket := g.upgrade(t, `{"version":"1.0.30","sha256":"`+sha(g.setup)+`","assumeIdle":true}`)
	if exit != 0 || !ans.Accepted {
		t.Fatalf("assumeIdle: %+v", ans)
	}
	if _, err := os.Stat(filepath.Join(g.dir, "upgrade", ticket, jobFile)); err != nil {
		t.Fatal(err)
	}
	g2 := newUpgradeRig(t, "v1.0.29")
	g2.runner.state, g2.runner.queue = "stopped", ""
	ans, exit, ticket = g2.upgrade(t, g2.spec("1.0.30", sha(g2.setup)))
	if exit != 0 || !ans.Accepted {
		t.Fatalf("a stopped daemon holds no orders: %+v", ans)
	}
	var job UpgradeJob
	b, _ := os.ReadFile(filepath.Join(g2.dir, "upgrade", ticket, jobFile))
	_ = json.Unmarshal(b, &job)
	if job.DaemonWasRunning {
		t.Fatal("a stopped daemon would be started after the upgrade")
	}
}

// Review M1: an upgrade never runs beside a provisioning run — the setup would stop the daemon in the middle of a db
// rotation, or fail on a provision.exe in use.
func TestUpgradeWaitsForProvisionRuns(t *testing.T) {
	// a provision run (apply, db, auth, orders…) holds DataDir\provision\.lock
	g := newUpgradeRig(t, "v1.0.29")
	_ = os.MkdirAll(filepath.Join(g.dir, "provision"), 0o700)
	h, err := filelock.Lock(ProvisionLockPath(g.dir))
	if err != nil {
		t.Fatal(err)
	}
	ans, exit, ticket := g.upgrade(t, g.spec("1.0.30", sha(g.setup)))
	_ = h.Unlock()
	if exit != 1 || ans.Refusal == nil || ans.Refusal.Code != "busy" || len(g.spawned.calls) != 0 {
		t.Fatalf("answer %+v, spawned %v", ans, g.spawned.calls)
	}
	if _, err := os.Stat(filepath.Join(g.dir, "upgrade", ticket)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused upgrade left its ticket")
	}
	// another command is running in the second slot (a provision.exe the setup could not replace)
	g.b.link.cmdSlots <- struct{}{}
	ans, exit, _ = g.upgrade(t, g.spec("1.0.30", sha(g.setup)))
	<-g.b.link.cmdSlots
	if exit != 1 || ans.Refusal == nil || ans.Refusal.Code != "busy" {
		t.Fatalf("with another command running: %+v", ans)
	}
	// while the link checks and downloads, the lock is held: no provision run can start
	var heldDuring atomic.Bool
	g.runner.mu.Lock()
	g.runner.onState = func() {
		h, err := filelock.Lock(ProvisionLockPath(g.dir))
		if err == nil {
			_ = h.Unlock()
		}
		heldDuring.Store(errors.Is(err, filelock.ErrLocked))
	}
	g.runner.mu.Unlock()
	if ans, _, _ := g.upgrade(t, g.spec("1.0.30", sha(g.setup))); !ans.Accepted {
		t.Fatalf("%+v", ans)
	}
	if !heldDuring.Load() {
		t.Fatal("the provision lock was not held while the upgrade was accepted")
	}
	// accepted, the ticket pending: the link runs no provision command, and provision runs refuse (UpgradePending)
	if pending, _ := UpgradePending(g.dir, time.Now()); !pending {
		t.Fatal("no upgrade pending")
	}
	before := g.runner.calls()
	_, a, _ := g.send(t, "version", "")
	if a.Refusal != CodeUpgradeRunning || g.runner.calls() != before {
		t.Fatalf("a provision command ran during the upgrade: %+v", a)
	}
	if _, a, _ := g.send(t, CommandHashes, ""); a.Refusal != "" {
		t.Fatalf("the link's own commands still answer: %+v", a)
	}
}

// A redirect may not leave HTTPS for HTTP.
func TestUpgradeRefusesARedirectToPlainHTTP(t *testing.T) {
	var plain string
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, plain+req.URL.Path, http.StatusFound)
	}))
	t.Cleanup(tlsSrv.Close)
	g := newUpgradeRig(t, "v1.0.29", func(c *Config) { c.HTTPClient = tlsSrv.Client() })
	plain = g.rel.srv.URL
	releasePrefix = tlsSrv.URL + "/releases/download/"
	ans, _, _ := g.upgrade(t, g.spec("1.0.30", sha(g.setup)))
	if ans.Refusal == nil || ans.Refusal.Code != "download_failed" || !strings.Contains(ans.Refusal.Message, "refused redirect") {
		t.Fatalf("%+v", ans)
	}
}

func TestUpgradeResult(t *testing.T) {
	g := newUpgradeRig(t, "v1.0.29")
	ticket := strings.Repeat("0123456789abcdef", 2)
	for _, in := range []string{`{}`, `{"ticket":"../../config"}`, `{"ticket":"` + strings.ToUpper(ticket) + `"}`} {
		out, a, _ := g.send(t, CommandUpgradeResult, in)
		if a.Exit != 1 || !strings.Contains(string(out), "spec_invalid") {
			t.Fatalf("%s: %s", in, out)
		}
	}
	out, a, _ := g.send(t, CommandUpgradeResult, `{"ticket":"`+ticket+`"}`)
	if a.Exit != 1 || !strings.Contains(string(out), "ticket_unknown") {
		t.Fatalf("unknown: %s", out)
	}
	// a job whose runner never took the lock and is long gone: lost
	tdir := filepath.Join(g.dir, "upgrade", ticket)
	_ = os.MkdirAll(tdir, 0o700)
	_ = os.WriteFile(filepath.Join(tdir, jobFile), []byte(`{}`), 0o600)
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(filepath.Join(tdir, jobFile), old, old)
	out, _, _ = g.send(t, CommandUpgradeResult, `{"ticket":"`+ticket+`"}`)
	if !strings.Contains(string(out), `"lost":true`) {
		t.Fatalf("lost: %s", out)
	}
	// its runner holds the lock: pending
	h, err := filelock.Lock(lockPath(g.dir))
	if err != nil {
		t.Fatal(err)
	}
	out, _, _ = g.send(t, CommandUpgradeResult, `{"ticket":"`+ticket+`"}`)
	_ = h.Unlock()
	if !strings.Contains(string(out), `"pending":true`) {
		t.Fatalf("pending: %s", out)
	}
	res := UpgradeResult{Ticket: ticket, From: "1.0.29", To: "1.0.30", ExitCode: 0, InstalledVersion: "v1.0.30"}
	b, _ := json.Marshal(res)
	_ = os.MkdirAll(filepath.Join(g.dir, "results"), 0o700)
	_ = os.WriteFile(resultPath(g.dir, ticket), b, 0o600)
	out, a, _ = g.send(t, CommandUpgradeResult, `{"ticket":"`+ticket+`"}`)
	if a.Exit != 0 || string(out) != string(b) {
		t.Fatalf("done: %s", out)
	}
}

// --- the runner -----------------------------------------------------------------------------------------------------

type runnerRig struct {
	r        UpgradeRunner
	dir      string
	ticket   string
	tdir     string
	setup    []byte
	slept    []time.Duration
	started  []string
	stopped  []string
	services map[string]string
	log      *testLog
	ran      int
}

func newRunnerRig(t *testing.T, daemonWasRunning bool) *runnerRig {
	t.Helper()
	g := &runnerRig{dir: t.TempDir(), ticket: strings.Repeat("ab12", 8), setup: []byte("MZ setup"), log: &testLog{},
		services: map[string]string{ServiceName: "running", DaemonServiceName: "running"}}
	g.tdir = filepath.Join(g.dir, "upgrade", g.ticket)
	_ = os.MkdirAll(g.tdir, 0o700)
	_ = os.WriteFile(filepath.Join(g.tdir, setupFile("1.0.30")), g.setup, 0o600)
	job := UpgradeJob{Ticket: g.ticket, From: "1.0.29", To: "1.0.30", SHA256: sha(g.setup), Setup: setupFile("1.0.30"),
		InstallDir: "install", DaemonWasRunning: daemonWasRunning}
	b, _ := json.Marshal(job)
	_ = os.WriteFile(filepath.Join(g.tdir, jobFile), b, 0o600)
	now := time.Date(2026, 11, 16, 3, 0, 0, 0, time.UTC)
	g.r = UpgradeRunner{
		DataDir: g.dir, Ticket: g.ticket, Log: g.log,
		Now:   func() time.Time { return now },
		Sleep: func(d time.Duration) { g.slept = append(g.slept, d); now = now.Add(d) },
		RunSetup: func(setup, logPath string, _ time.Duration) (int, error) {
			g.ran++
			_ = os.WriteFile(logPath, []byte("Log opened.\nSetup version: Inno Setup 6\nPrepareToInstall failed: The digi-erp-connectord service did not stop\n"), 0o600)
			return 0, nil
		},
		ServiceState: func(name string) (string, error) { return g.services[name], nil },
		StartService: func(name string) error { g.started = append(g.started, name); g.services[name] = "running"; return nil },
		StopService:  func(name string) error { g.stopped = append(g.stopped, name); g.services[name] = "stopped"; return nil },
		Version:      func(string) string { return "v1.0.30" },
	}
	return g
}

func (g *runnerRig) result(t *testing.T) UpgradeResult {
	t.Helper()
	b, err := os.ReadFile(resultPath(g.dir, g.ticket))
	if err != nil {
		t.Fatal(err)
	}
	var r UpgradeResult
	_ = json.Unmarshal(b, &r)
	return r
}

func TestTheRunnerUpgrades(t *testing.T) {
	g := newRunnerRig(t, true)
	if _, err := g.r.Run(); err != nil {
		t.Fatal(err)
	}
	r := g.result(t)
	if r.ExitCode != 0 || r.Error != "" || r.From != "1.0.29" || r.To != "1.0.30" || r.InstalledVersion != "v1.0.30" ||
		r.StartedAt.IsZero() || !r.FinishedAt.After(r.StartedAt) || r.Services[ServiceName] != "running" || len(r.SetupLogTail) != 0 {
		t.Fatalf("result %+v", r)
	}
	if len(g.slept) == 0 || g.slept[0] != answerGrace || g.ran != 1 || len(g.started) != 0 {
		t.Fatalf("slept %v ran %d started %v", g.slept, g.ran, g.started)
	}
	if _, err := os.Stat(filepath.Join(g.tdir, setupFile("1.0.30"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the setup was kept")
	}
	if !g.log.has("1.0.29 -> 1.0.30 done") {
		t.Fatal("not logged")
	}
}

// A setup that stopped the services and failed leaves the box unreachable unless the runner starts them again.
func TestTheRunnerStartsWhatAFailedSetupLeftStopped(t *testing.T) {
	g := newRunnerRig(t, true)
	g.r.RunSetup = func(_, logPath string, _ time.Duration) (int, error) {
		_ = os.WriteFile(logPath, []byte("line 1\nthe digi-erp-connectord service did not stop within 180 seconds\n"), 0o600)
		g.services[ServiceName], g.services[DaemonServiceName] = "stopped", "stopped"
		return 7, nil
	}
	g.r.Version = func(string) string { return "v1.0.29" }
	_, _ = g.r.Run()
	r := g.result(t)
	if r.ExitCode != 7 || !strings.Contains(r.Error, "180 s") || len(r.SetupLogTail) != 2 || r.InstalledVersion != "v1.0.29" {
		t.Fatalf("result %+v", r)
	}
	if strings.Join(g.started, ",") != ServiceName+","+DaemonServiceName || r.Services[ServiceName] != "running" {
		t.Fatalf("started %v, services %v", g.started, r.Services)
	}
	// a daemon that was stopped before stays stopped
	g = newRunnerRig(t, false)
	g.services[DaemonServiceName] = "stopped"
	_, _ = g.r.Run()
	if len(g.started) != 0 || g.result(t).Services[DaemonServiceName] != "stopped" {
		t.Fatalf("started %v", g.started)
	}
}

// Review m3: the setup's [Run] starts the daemon whatever it was; one stopped on purpose before the upgrade (a cutover,
// maintenance) is stopped again, and the result says so.
func TestTheRunnerKeepsADeliberatelyStoppedDaemonStopped(t *testing.T) {
	g := newRunnerRig(t, false)
	g.services[DaemonServiceName] = "stopped"
	g.r.RunSetup = func(_, logPath string, _ time.Duration) (int, error) {
		_ = os.WriteFile(logPath, []byte("ok\n"), 0o600)
		g.services[DaemonServiceName] = "running" // sc.exe start in [Run]
		return 0, nil
	}
	if _, err := g.r.Run(); err != nil {
		t.Fatal(err)
	}
	r := g.result(t)
	if strings.Join(g.stopped, ",") != DaemonServiceName || r.Services[DaemonServiceName] != "stopped" || r.Error != "" {
		t.Fatalf("stopped %v, result %+v", g.stopped, r)
	}
	if !g.log.has("stopped again") {
		t.Fatal("not logged")
	}
}

// Review m4: a setup that exits 0 while a service that should run does not is a failed upgrade, not a success.
func TestTheRunnerReportsAServiceThatDoesNotRun(t *testing.T) {
	for _, c := range []struct {
		name    string
		service string
		was     bool
	}{
		{"the daemon that ran before", DaemonServiceName, true},
		{"the link", ServiceName, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newRunnerRig(t, c.was)
			if !c.was {
				g.services[DaemonServiceName] = "stopped"
			}
			g.r.RunSetup = func(_, logPath string, _ time.Duration) (int, error) {
				_ = os.WriteFile(logPath, []byte("ok\n"), 0o600)
				g.services[c.service] = "stopped" // the new build refuses to start
				return 0, nil
			}
			g.r.StartService = func(name string) error {
				g.started = append(g.started, name)
				return errors.New("the service did not start")
			}
			_, _ = g.r.Run()
			r := g.result(t)
			if r.ExitCode != 0 || !strings.Contains(r.Error, c.service) || r.Services[c.service] != "stopped" || len(r.SetupLogTail) == 0 {
				t.Fatalf("result %+v", r)
			}
		})
	}
}

func TestTheRunnerRefusesAChangedSetup(t *testing.T) {
	g := newRunnerRig(t, true)
	_ = os.WriteFile(filepath.Join(g.tdir, setupFile("1.0.30")), []byte("MZ swapped"), 0o600)
	_, _ = g.r.Run()
	if r := g.result(t); g.ran != 0 || r.ExitCode != -1 || !strings.Contains(r.Error, "not run") {
		t.Fatalf("ran %d, result %+v", g.ran, r)
	}
}

func TestTheRunnerChecksTheInstalledBuild(t *testing.T) {
	g := newRunnerRig(t, true)
	g.r.Version = func(string) string { return "v1.0.29" }
	_, _ = g.r.Run()
	if r := g.result(t); !strings.Contains(r.Error, `"v1.0.29", not 1.0.30`) || len(r.SetupLogTail) == 0 {
		t.Fatalf("result %+v", r)
	}
}

func TestOneRunnerAtATime(t *testing.T) {
	g := newRunnerRig(t, true)
	h, err := filelock.Lock(lockPath(g.dir))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Unlock() }()
	_, _ = g.r.Run()
	if r := g.result(t); g.ran != 0 || !strings.Contains(r.Error, "another upgrade runner") {
		t.Fatalf("ran %d result %+v", g.ran, r)
	}
}

// Review M1: the runner holds the provision lock from before the setup until the services run again and the result is
// written; a provision run holding it means no setup.
func TestTheRunnerHoldsTheProvisionLock(t *testing.T) {
	g := newRunnerRig(t, true)
	var heldInSetup, heldAtStart bool
	setup := g.r.RunSetup
	g.r.RunSetup = func(s, logPath string, d time.Duration) (int, error) {
		_, err := filelock.Lock(ProvisionLockPath(g.dir))
		heldInSetup = errors.Is(err, filelock.ErrLocked)
		return setup(s, logPath, d)
	}
	start := g.r.StartService
	g.services[DaemonServiceName] = "stopped"
	g.r.StartService = func(name string) error {
		_, err := filelock.Lock(ProvisionLockPath(g.dir))
		heldAtStart = errors.Is(err, filelock.ErrLocked)
		return start(name)
	}
	if _, err := g.r.Run(); err != nil {
		t.Fatal(err)
	}
	if !heldInSetup || !heldAtStart {
		t.Fatalf("the lock was not held: in the setup %v, at the service start %v", heldInSetup, heldAtStart)
	}
	h, err := filelock.Lock(ProvisionLockPath(g.dir))
	if err != nil {
		t.Fatalf("the lock was kept after the run: %v", err)
	}
	_ = h.Unlock()

	g = newRunnerRig(t, true)
	_ = os.MkdirAll(filepath.Join(g.dir, "provision"), 0o700)
	h, err = filelock.Lock(ProvisionLockPath(g.dir))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Unlock() }()
	_, _ = g.r.Run()
	if r := g.result(t); g.ran != 0 || !strings.Contains(r.Error, "provisioning run") {
		t.Fatalf("ran %d, result %+v", g.ran, r)
	}
}

func TestPruneUpgradesKeepsTheNewestFinished(t *testing.T) {
	dir := t.TempDir()
	var n atomic.Int64
	mk := func(finished bool) string {
		ticket := fmt.Sprintf("%032x", n.Add(1))
		_ = os.MkdirAll(filepath.Join(dir, "upgrade", ticket), 0o700)
		if finished {
			_ = os.MkdirAll(filepath.Join(dir, "results"), 0o700)
			_ = os.WriteFile(resultPath(dir, ticket), []byte(`{}`), 0o600)
		}
		at := time.Now().Add(time.Duration(n.Load()) * time.Minute)
		_ = os.Chtimes(filepath.Join(dir, "upgrade", ticket), at, at)
		return ticket
	}
	var finished []string
	for range 12 {
		finished = append(finished, mk(true))
	}
	running := mk(false)
	pruneUpgrades(dir)
	for i, ticket := range finished {
		_, err := os.Stat(filepath.Join(dir, "upgrade", ticket))
		if (i < 2) != errors.Is(err, os.ErrNotExist) {
			t.Errorf("ticket %d: %v", i, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "upgrade", running)); err != nil {
		t.Fatal("an unfinished ticket was removed")
	}
}

func TestVersionsAndTickets(t *testing.T) {
	for in, want := range map[string]string{"v1.0.29": "1.0.29", "1.0.30": "1.0.30", "v0.0.0-ci.abc1234": "0.0.0"} {
		if v, ok := parseVersion(buildRe, in); !ok || v.String() != want {
			t.Errorf("%s: %v %v", in, v, ok)
		}
	}
	for _, in := range []string{"dev", "", "1.0", "v1.0.29.1"} {
		if _, ok := parseVersion(buildRe, in); ok {
			t.Errorf("%q parsed", in)
		}
	}
	if _, ok := parseVersion(versionRe, "1.0.30-rc1"); ok {
		t.Error("a pre-release is not a release")
	}
	a, b := TicketFor("bm9uY2U="), TicketFor("bm9uY2U=")
	if a != b || !ticketRe.MatchString(a) || a == TicketFor("other") {
		t.Fatalf("%s %s", a, b)
	}
}
