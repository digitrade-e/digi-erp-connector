package link

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/atomicfile"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/filelock"
	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// Upgrade over the link (plan 086 C8, decision D2). The box accepts `upgrade` only for an official release of the
// compiled-in repository: it downloads that release's SHA256SUMS and setup itself, from releasePrefix only, and runs
// the setup only when its SHA-256 equals both the signed envelope's pin and the release's SHA256SUMS line, and the
// version is newer than its own. A stolen team key can at most install an official newer release — the trust a person
// has today downloading it by hand. Authenticode stays the later hardening (plan 083 D4).
//
// The setup stops the link service itself (the installer's PrepareToInstall), so the link cannot wait for it: it
// checks everything that can fail first, answers {"accepted":true,"ticket":...} at once, and leaves the work to a
// detached runner — a copy of this executable in the ticket's folder (upgrade-run), outside the install folder the
// setup replaces — which runs the setup silently, starts again whatever the setup left stopped, and writes
// DataDir\results\upgrade-<ticket>.json. `upgrade-result` reads it back (from the new link, after the upgrade).

const (
	CommandUpgrade       = "upgrade"
	CommandUpgradeResult = "upgrade-result"
)

// releasePrefix is where a release's files are: "<prefix>v<version>/<file>". Compiled in; tests (only) point it at a
// stand-in.
var releasePrefix = "https://github.com/digitrade-e/digi-erp-connector/releases/download/"

// Files a release carries (release-windows.yml).
const (
	sumsFile  = "SHA256SUMS"
	runnerExe = "digi-erp-connector-upgrade.exe"
	jobFile   = "job.json"
)

func setupFile(version string) string { return "digi-erp-connector-setup-" + version + ".exe" }

const (
	maxSums  = 64 << 10
	maxSetup = 512 << 20
	// downloadDeadline bounds both downloads, within the command's own deadline.
	downloadDeadline = 15 * time.Minute
	// runnerGrace is how long a ticket counts as running before its runner holds the lock.
	runnerGrace = 2 * time.Minute
	// keepTickets bounds the finished tickets and results kept.
	keepTickets = 10
)

// pickupMinVersion is the first release that knows sendOrderPickupDir (DIG-1297, v1.0.25). An older build ignores it
// and writes orders nobody imports (docs/provisioning.md: never downgrade such a box). Only newer releases are
// accepted at all, so this guard is the rule written down, for the day that changes.
var pickupMinVersion = version{1, 0, 25}

// UpgradeConfig is what the upgrade needs from the box; empty fields take the real one's.
type UpgradeConfig struct {
	DataDir string       // tickets (upgrade\) and results (results\); "" = the Store's directory
	SelfExe string       // this executable, copied as the runner; "" = os.Executable
	Protect func() error // the data directory's DACL before anything is written there (Machine.Protect)
	// Spawn starts the runner detached from the link (nil = spawnDetached: Windows only).
	Spawn func(exe string, args []string, dir string) error
}

var upgradeMu sync.Mutex // one upgrade command at a time in this process

func init() {
	selfCommands[CommandUpgrade] = (*Link).upgrade
	selfCommands[CommandUpgradeResult] = (*Link).upgradeResult
}

type upgradeInput struct {
	Version    string `json:"version"`
	SHA256     string `json:"sha256"`
	AssumeIdle bool   `json:"assumeIdle,omitempty"`
	// HealthMinutes is how long the runner waits for the box to be healthy after the setup before it rolls back (plan
	// 088 D5): 5 to 120, 0 = defaultHealthMinutes.
	HealthMinutes int `json:"healthMinutes,omitempty"`
}

// defaultHealthMinutes is a command's health check when it names none (plan 088 D5); a policy names its own.
const defaultHealthMinutes = 15

// UpgradeAnswer is upgrade's answer: accepted with its ticket, or a refusal (exit 1).
type UpgradeAnswer struct {
	Accepted bool            `json:"accepted"`
	Ticket   string          `json:"ticket,omitempty"`
	From     string          `json:"from,omitempty"`
	To       string          `json:"to,omitempty"`
	Refusal  *UpgradeRefusal `json:"refusal,omitempty"`
}

// UpgradeRefusal is why an upgrade (or a result) was refused, with the fix.
type UpgradeRefusal struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Fix     string `json:"fix,omitempty"`
}

func (r *UpgradeRefusal) Error() string { return r.Code + ": " + r.Message }

func upgradeRefuse(code, fix, format string, args ...any) *UpgradeRefusal {
	return &UpgradeRefusal{Code: code, Message: fmt.Sprintf(format, args...), Fix: fix}
}

// UpgradeJob is the ticket's job.json: what the runner does.
type UpgradeJob struct {
	Ticket           string    `json:"ticket"`
	From             string    `json:"from"`
	To               string    `json:"to"`
	SHA256           string    `json:"sha256"`
	Setup            string    `json:"setup"` // the file in the ticket's folder
	InstallDir       string    `json:"installDir"`
	DaemonWasRunning bool      `json:"daemonWasRunning"`
	By               string    `json:"by,omitempty"`
	AcceptedAt       time.Time `json:"acceptedAt"`
	// Plan 088. FromSHA256 is the setup of the build replaced, when this box knows it (a ticket or a policy installed
	// it): the rollback requires it. HealthMinutes > 0 turns the health check and the one rollback on (D5). Auto and
	// Policy: the scheduler started it, following that policy.
	FromSHA256    string     `json:"fromSha256,omitempty"`
	HealthMinutes int        `json:"healthMinutes,omitempty"`
	Auto          bool       `json:"auto,omitempty"`
	Policy        *PolicyRef `json:"policy,omitempty"`
}

// UpgradeResult is DataDir\results\upgrade-<ticket>.json.
type UpgradeResult struct {
	Ticket           string            `json:"ticket"`
	From             string            `json:"from"`
	To               string            `json:"to"`
	ExitCode         int               `json:"exitCode"` // the setup's; -1 when it did not run or did not end
	StartedAt        time.Time         `json:"startedAt"`
	FinishedAt       time.Time         `json:"finishedAt"`
	InstalledVersion string            `json:"installedVersion,omitempty"` // provision.exe version, after
	Services         map[string]string `json:"services,omitempty"`         // after: service → state
	Error            string            `json:"error,omitempty"`
	SetupLogTail     []string          `json:"setupLogTail,omitempty"` // the setup's log's end when it failed
	// Plan 088: the setup's digest as checked, whether the scheduler started it (and under which policy), the health
	// check after the setup and the one rollback after an unhealthy one (D5).
	SHA256   string                     `json:"sha256,omitempty"`
	Auto     bool                       `json:"auto,omitempty"`
	Policy   *PolicyRef                 `json:"policy,omitempty"`
	Health   *linkproto.UpgradeHealth   `json:"health,omitempty"`
	Rollback *linkproto.UpgradeRollback `json:"rollback,omitempty"`
}

var (
	versionRe   = regexp.MustCompile(`^v?(\d{1,4})\.(\d{1,4})\.(\d{1,6})$`)
	buildRe     = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:[-+][0-9A-Za-z.-]*)?$`) // a CI build: v0.0.0-ci.abc1234
	sha256Re    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	ticketRe    = regexp.MustCompile(`^[0-9a-f]{32}$`)
	sumsLineRe  = regexp.MustCompile(`^([0-9a-fA-F]{64}) [ *](\S.*)$`)
	upgradeDirs = struct{ tickets, results string }{"upgrade", "results"}
)

type version [3]int

func parseVersion(re *regexp.Regexp, s string) (version, bool) {
	m := re.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return version{}, false
	}
	var v version
	for i := range v {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return version{}, false
		}
		v[i] = n
	}
	return v, true
}

func (v version) String() string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

func (v version) less(o version) bool { return slices.Compare(v[:], o[:]) < 0 }

// TicketFor is an upgrade's ticket: the first 32 hex digits of the SHA-256 of the envelope's nonce. digi-mcp can
// compute it, so it can ask for the result even when the answer to `upgrade` was lost.
func TicketFor(nonce string) string {
	sum := sha256.Sum256([]byte(nonce))
	return hex.EncodeToString(sum[:])[:32]
}

func (l *Link) upgradeDataDir() string {
	if l.cfg.Upgrade.DataDir != "" {
		return l.cfg.Upgrade.DataDir
	}
	return l.cfg.Store.Dir
}

func answer(v any, exit int) ([]byte, int) {
	b, _ := json.Marshal(v)
	return b, exit
}

func (l *Link) upgrade(env linkproto.Envelope, stdin []byte) ([]byte, int) {
	upgradeMu.Lock()
	defer upgradeMu.Unlock()
	ans, err := l.acceptUpgrade(env, stdin)
	if err != nil {
		var r *UpgradeRefusal
		if !errors.As(err, &r) {
			r = upgradeRefuse("internal", "read link.log", "%v", err)
		}
		r.Message = withoutCredentials(r.Message)
		l.cfg.Log.Warn(fmt.Sprintf("link: upgrade refused: %s", r.Error()))
		return answer(UpgradeAnswer{Refusal: r}, 1)
	}
	l.cfg.Log.Info(fmt.Sprintf("link: upgrade %s -> %s accepted, ticket %s; the runner takes it from here", ans.From, ans.To, ans.Ticket))
	return answer(ans, 0)
}

// acceptUpgrade checks a command's input and starts the upgrade (startUpgrade).
func (l *Link) acceptUpgrade(env linkproto.Envelope, stdin []byte) (UpgradeAnswer, error) {
	var in upgradeInput
	if err := decodeStrictJSON(stdin, &in); err != nil {
		return UpgradeAnswer{}, upgradeRefuse("spec_invalid", `send {"version":"1.0.30","sha256":"<64 hex>","assumeIdle"?:true,"healthMinutes"?:5-120}`, "the input does not decode: %v", err)
	}
	to, ok := parseVersion(versionRe, in.Version)
	if !ok {
		return UpgradeAnswer{}, upgradeRefuse("spec_invalid", "version is a release's number, like 1.0.30", "version %q is not a release number", in.Version)
	}
	sha := strings.ToLower(strings.TrimSpace(in.SHA256))
	if !sha256Re.MatchString(sha) {
		return UpgradeAnswer{}, upgradeRefuse("spec_invalid", "sha256 is the setup's SHA-256, 64 hex digits", "sha256 is not 64 hex digits")
	}
	if in.HealthMinutes != 0 && (in.HealthMinutes < linkproto.MinHealthMinutes || in.HealthMinutes > linkproto.MaxHealthMinutes) {
		return UpgradeAnswer{}, upgradeRefuse("spec_invalid", "healthMinutes is 5 to 120, or left out (15)", "healthMinutes %d is out of range", in.HealthMinutes)
	}
	return l.startUpgrade(upgradeRequest{To: to, SHA256: sha, AssumeIdle: in.AssumeIdle, Ticket: TicketFor(env.Nonce), By: env.By,
		HealthMinutes: in.HealthMinutes, command: true})
}

// upgradeRequest is one upgrade to start: a signed command's, or the scheduler's under a policy (plan 088).
type upgradeRequest struct {
	To            version
	SHA256        string // the setup's, lowercase hex
	AssumeIdle    bool
	Ticket        string
	By            string
	HealthMinutes int        // 0 = defaultHealthMinutes
	Policy        *PolicyRef // the scheduler's: the policy it follows
	command       bool       // a command holds one of the two command slots itself
}

// startUpgrade runs every check that can fail before the runner starts, in order, and starts it. The command and the
// scheduler share it: the scheduler decides only when (plan 088 D2).
func (l *Link) startUpgrade(req upgradeRequest) (UpgradeAnswer, error) {
	to, sha, ticket := req.To, req.SHA256, req.Ticket
	from, ok := parseVersion(buildRe, l.cfg.Build)
	if !ok {
		return UpgradeAnswer{}, upgradeRefuse("version_unknown", "install a release by hand once", "this link's build %q is not a release, so newer cannot be judged", l.cfg.Build)
	}
	if !from.less(to) {
		return UpgradeAnswer{}, upgradeRefuse("not_newer", "upgrade only to a newer release; a downgrade is a person's job at the server",
			"%s is not newer than the running %s", to, from)
	}
	if l.cfg.Daemon != nil {
		if cfg, err := l.cfg.Daemon.Load(); err == nil && strings.TrimSpace(cfg.SendOrderPickupDir) != "" && to.less(pickupMinVersion) {
			return UpgradeAnswer{}, upgradeRefuse("downgrade_pickup", "remove sendOrderPickupDir first, or choose a release from 1.0.25 on",
				"this box writes orders to a pickup folder (DIG-1297), which %s ignores: its orders would never be imported", to)
		}
	}
	dir := l.upgradeDataDir()
	if busy, other := runningTicket(dir, l.cfg.Now()); busy {
		return UpgradeAnswer{}, upgradeRefuse("upgrade_running", "wait for it: upgrade-result with its ticket", "the upgrade of ticket %s is still running", other)
	}
	// Review M1: never beside another command — a provision.exe still running would make the setup fail on a file in
	// use — nor beside a provisioning run (DataDir\provision\.lock, WinRM's included), whose service stop and rollback
	// would cross the setup's. A command holds one of the two slots; the scheduler none.
	own := 0
	if req.command {
		own = 1
	}
	if len(l.cmdSlots) > own {
		return UpgradeAnswer{}, upgradeRefuse(busyCode, "wait for the other command to end and ask again", "another command is running on this box")
	}
	if l.cfg.Upgrade.Protect != nil {
		if err := l.cfg.Upgrade.Protect(); err != nil {
			return UpgradeAnswer{}, upgradeRefuse("data_dir_unsafe", "fix the data directory's owner or links (docs/troubleshooting.md)", "%v", err)
		}
	}
	// Held from here until the job is written and the runner started: from then on UpgradePending refuses every
	// provisioning run, and the runner takes this lock for the setup.
	held, err := lockProvision(dir, 0, l.cfg.Now, time.Sleep)
	if errors.Is(err, filelock.ErrLocked) {
		return UpgradeAnswer{}, upgradeRefuse(busyCode, "wait for the provisioning run to end and ask again", "a provisioning run is in progress on this box")
	}
	if err != nil {
		return UpgradeAnswer{}, upgradeRefuse("runner_failed", "check the disk (state: disk)", "the provision lock: %v", err)
	}
	defer func() { _ = held.Unlock() }()
	daemonRunning, err := l.orderQueueIdle(req.AssumeIdle)
	if err != nil {
		return UpgradeAnswer{}, err
	}
	pruneUpgrades(dir, l.cfg.Now())
	tdir, err := makeTicketDir(dir, ticket)
	if err != nil {
		return UpgradeAnswer{}, err
	}
	accepted := false
	defer func() {
		if !accepted {
			_ = os.RemoveAll(tdir) // nothing ran: leave nothing behind
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), downloadDeadline)
	defer cancel()
	base := releasePrefix + "v" + to.String() + "/"
	setup := setupFile(to.String())
	listed, err := releaseSum(ctx, l.cfg.HTTPClient, base+sumsFile, setup)
	if err != nil {
		return UpgradeAnswer{}, err
	}
	if listed != sha {
		return UpgradeAnswer{}, upgradeRefuse("sha256_mismatch", "pin the setup's digest from the release's SHA256SUMS",
			"the envelope pins %s but release v%s's SHA256SUMS lists %s for %s", sha, to, listed, setup)
	}
	got, err := download(ctx, l.cfg.HTTPClient, base+setup, filepath.Join(tdir, setup))
	if err != nil {
		return UpgradeAnswer{}, err
	}
	if got != sha {
		return UpgradeAnswer{}, upgradeRefuse("sha256_mismatch", "nothing was run; try again, and if it repeats report it",
			"the downloaded %s has SHA-256 %s, not %s", setup, got, sha)
	}
	self := l.cfg.Upgrade.SelfExe
	if self == "" {
		if self, err = os.Executable(); err != nil {
			return UpgradeAnswer{}, upgradeRefuse("runner_failed", "", "this executable's path: %v", err)
		}
	}
	runner := filepath.Join(tdir, runnerExe)
	if err := copyFile(self, runner); err != nil {
		return UpgradeAnswer{}, upgradeRefuse("runner_failed", "check the disk (state: disk)", "copying the runner: %v", err)
	}
	health := req.HealthMinutes
	if health == 0 {
		health = defaultHealthMinutes
	}
	job := UpgradeJob{Ticket: ticket, From: from.String(), To: to.String(), SHA256: sha, Setup: setup,
		InstallDir: l.cfg.InstallDir, DaemonWasRunning: daemonRunning, By: req.By, AcceptedAt: l.cfg.Now().UTC(),
		FromSHA256: l.knownSetupSHA(from.String()), HealthMinutes: health, Auto: req.Policy != nil, Policy: req.Policy}
	b, _ := json.MarshalIndent(job, "", "  ")
	if err := atomicfile.Write(filepath.Join(tdir, jobFile), b, 0o600); err != nil {
		return UpgradeAnswer{}, upgradeRefuse("runner_failed", "check the disk (state: disk)", "writing the job: %v", err)
	}
	spawn := l.cfg.Upgrade.Spawn
	if spawn == nil {
		spawn = spawnDetached
	}
	if err := spawn(runner, []string{"upgrade-run", ticket}, tdir); err != nil {
		var r *UpgradeRefusal
		if errors.As(err, &r) {
			return UpgradeAnswer{}, r
		}
		return UpgradeAnswer{}, upgradeRefuse("runner_failed", "", "starting the runner: %v", err)
	}
	accepted = true
	return UpgradeAnswer{Accepted: true, Ticket: ticket, From: from.String(), To: to.String()}, nil
}

// orderQueueIdle asks provision's state whether the daemon holds accepted orders (a restart loses them). daemonRunning
// says whether the runner should start the daemon again if the setup leaves it stopped.
func (l *Link) orderQueueIdle(assumeIdle bool) (daemonRunning bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	out, _, runErr := l.cfg.Runner.Run(ctx, []string{"state", "--spec", "-"}, []byte("{}"), linkproto.MaxPayload)
	var st struct {
		OK     bool `json:"ok"`
		Result struct {
			Services struct {
				Daemon struct {
					State string `json:"state"`
				} `json:"daemon"`
			} `json:"services"`
			Daemon struct {
				Queue *struct {
					Queued  int `json:"queued"`
					Running int `json:"running"`
				} `json:"queue"`
				Error string `json:"error"`
			} `json:"daemon"`
		} `json:"result"`
	}
	if runErr == nil {
		runErr = json.Unmarshal(out, &st)
	}
	if runErr == nil && !st.OK {
		runErr = errors.New("provision state refused")
	}
	if runErr != nil {
		if assumeIdle {
			return true, nil
		}
		return false, upgradeRefuse("orders_unknown", "wait for a quiet moment and set assumeIdle", "the order queue cannot be read: %v", runErr)
	}
	state := st.Result.Services.Daemon.State
	running := state != "stopped" && state != ""
	q := st.Result.Daemon.Queue
	switch {
	case assumeIdle || !running:
		return running, nil
	case q == nil:
		return true, upgradeRefuse("orders_unknown", "wait for a quiet moment and set assumeIdle",
			"the running connector's order queue cannot be read: %s", st.Result.Daemon.Error)
	case q.Queued+q.Running > 0:
		return true, upgradeRefuse("orders_pending", "wait until they are written and ask again",
			"%d order(s) queued and %d being written: the upgrade restarts the connector, which would lose them", q.Queued, q.Running)
	}
	return true, nil
}

// releaseSum fetches the release's SHA256SUMS and returns the digest it lists for file.
func releaseSum(ctx context.Context, client *http.Client, url, file string) (string, error) {
	var buf bytes.Buffer
	if _, err := fetch(ctx, client, url, &buf, maxSums); err != nil {
		return "", err
	}
	var found []string
	sc := bufio.NewScanner(&buf)
	for sc.Scan() {
		if m := sumsLineRe.FindStringSubmatch(strings.TrimRight(sc.Text(), "\r")); m != nil && m[2] == file {
			found = append(found, strings.ToLower(m[1]))
		}
	}
	switch len(found) {
	case 0:
		return "", upgradeRefuse("release_unlisted", "upgrade to a release whose SHA256SUMS lists its setup (from v1.0.29 on)",
			"the release's SHA256SUMS does not list %s", file)
	case 1:
		return found[0], nil
	}
	return "", upgradeRefuse("release_unlisted", "", "the release's SHA256SUMS lists %s %d times", file, len(found))
}

// download fetches url into path and returns the SHA-256 of what was written.
func download(ctx context.Context, client *http.Client, url, path string) (string, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", upgradeRefuse("runner_failed", "check the disk (state: disk)", "creating %s: %v", filepath.Base(path), err)
	}
	h := sha256.New()
	_, err = fetch(ctx, client, url, io.MultiWriter(f, h), maxSetup)
	if cerr := f.Close(); err == nil && cerr != nil {
		err = upgradeRefuse("runner_failed", "check the disk (state: disk)", "writing %s: %v", filepath.Base(path), cerr)
	}
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fetch GETs url through the link's own HTTP client (its proxy, plan 083 O5) into w, at most max bytes. A redirect
// may not leave the scheme (GitHub sends release files on to its object store over HTTPS).
func fetch(ctx context.Context, client *http.Client, url string, w io.Writer, max int64) (int64, error) {
	if client == nil {
		client = http.DefaultClient
	}
	c := *client
	first := c.CheckRedirect
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || req.URL.Scheme != via[0].URL.Scheme {
			return errors.New("refused redirect")
		}
		if first != nil {
			return first(req, via)
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, upgradeRefuse("download_failed", "", "%v", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, upgradeRefuse("download_failed", "check the box's way out (link-status: proxy; logs: link)", "GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, upgradeRefuse("download_failed", "check that the release exists and has this file", "GET %s answered %d", url, resp.StatusCode)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, max+1))
	if err != nil {
		return n, upgradeRefuse("download_failed", "try again", "GET %s: %v", url, err)
	}
	if n > max {
		return n, upgradeRefuse("download_failed", "", "GET %s is larger than %d bytes", url, max)
	}
	return n, nil
}

func decodeStrictJSON(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the JSON object")
	}
	return nil
}

// makeTicketDir creates DataDir\upgrade\<ticket>, refusing a link in the way.
func makeTicketDir(dataDir, ticket string) (string, error) {
	root := filepath.Join(dataDir, upgradeDirs.tickets)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", upgradeRefuse("runner_failed", "check the disk (state: disk)", "%v", err)
	}
	if err := dataacl.PlainDir(root); err != nil {
		return "", upgradeRefuse("data_dir_unsafe", "", "%v", err)
	}
	tdir := filepath.Join(root, ticket)
	if err := os.Mkdir(tdir, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", upgradeRefuse("ticket_exists", "send a new envelope", "ticket %s exists already", ticket)
		}
		return "", upgradeRefuse("runner_failed", "check the disk (state: disk)", "%v", err)
	}
	return tdir, nil
}

func lockPath(dataDir string) string {
	return filepath.Join(dataDir, upgradeDirs.tickets, "runner.lock")
}

// ProvisionLockPath is provision's run lock, DataDir\provision\.lock (provision.lockRuns; a test pins that both agree).
// The upgrade takes it too (review M1): from the link's checks until the runner has started the services again and
// written the result, no provisioning run — over the link or WinRM — runs beside the setup.
func ProvisionLockPath(dataDir string) string {
	return filepath.Join(dataDir, "provision", ".lock")
}

// CodeUpgradeRunning refuses a provision command while an upgrade is pending: the setup is about to stop the services
// and replace provision.exe.
const CodeUpgradeRunning = "upgrade_running"

// busyCode is the upgrade's refusal while another command or a provisioning run is in progress (provision's "busy").
const busyCode = "busy"

// UpgradePending reports an upgrade whose runner is alive or may not have started yet (its job written, no result),
// and its ticket. provision refuses to start a run then.
func UpgradePending(dataDir string, now time.Time) (bool, string) {
	return runningTicket(dataDir, now)
}

// lockProvision takes provision's run lock, waiting up to wait for a holder that is about to let it go; a holder past
// that is ErrLocked.
func lockProvision(dataDir string, wait time.Duration, now func() time.Time, sleep func(time.Duration)) (*filelock.Held, error) {
	if err := os.MkdirAll(filepath.Dir(ProvisionLockPath(dataDir)), 0o700); err != nil {
		return nil, err
	}
	deadline := now().Add(wait)
	for {
		h, err := filelock.Lock(ProvisionLockPath(dataDir))
		if err == nil || !errors.Is(err, filelock.ErrLocked) || !now().Before(deadline) {
			return h, err
		}
		sleep(250 * time.Millisecond)
	}
}

func resultPath(dataDir, ticket string) string {
	return filepath.Join(dataDir, upgradeDirs.results, "upgrade-"+ticket+".json")
}

// runnerHoldsLock reports whether a runner holds the lock now (taking it for a moment when nobody does).
func runnerHoldsLock(dataDir string) bool {
	h, err := filelock.Lock(lockPath(dataDir))
	if errors.Is(err, filelock.ErrLocked) {
		return true
	}
	if err == nil {
		_ = h.Unlock()
	}
	return false
}

// ticketPending: the ticket has a job and no result, and its runner is alive (holds the lock) or may not have
// started yet. lost: the runner ended without a result.
func ticketPending(dataDir, ticket string, now time.Time) (pending, lost bool) {
	if _, err := os.Stat(resultPath(dataDir, ticket)); err == nil {
		return false, false
	}
	fi, err := os.Stat(filepath.Join(dataDir, upgradeDirs.tickets, ticket, jobFile))
	if err != nil {
		return false, false
	}
	if runnerHoldsLock(dataDir) || now.Sub(fi.ModTime()) < runnerGrace {
		return true, false
	}
	return false, true
}

// runningTicket finds a ticket still running.
func runningTicket(dataDir string, now time.Time) (bool, string) {
	entries, _ := os.ReadDir(filepath.Join(dataDir, upgradeDirs.tickets))
	for _, e := range entries {
		if e.IsDir() && ticketRe.MatchString(e.Name()) {
			if pending, _ := ticketPending(dataDir, e.Name(), now); pending {
				return true, e.Name()
			}
		}
	}
	return false, ""
}

// pruneUpgrades removes the folders of finished tickets beyond the newest keepTickets (the setup and the runner copy
// are tens of MB), and results beyond the newest keepTickets*2. Errors are ignored: a file still in use stays.
func pruneUpgrades(dataDir string, now time.Time) {
	type entry struct {
		path string
		at   time.Time
	}
	newestFirst := func(dir string, keep func(os.DirEntry) bool) []entry {
		entries, _ := os.ReadDir(dir)
		var out []entry
		for _, e := range entries {
			if !keep(e) {
				continue
			}
			if fi, err := e.Info(); err == nil {
				out = append(out, entry{filepath.Join(dir, e.Name()), fi.ModTime()})
			}
		}
		slices.SortFunc(out, func(a, b entry) int { return b.at.Compare(a.at) })
		return out
	}
	tickets := newestFirst(filepath.Join(dataDir, upgradeDirs.tickets), func(e os.DirEntry) bool {
		if !e.IsDir() || !ticketRe.MatchString(e.Name()) {
			return false
		}
		_, err := os.Stat(resultPath(dataDir, e.Name()))
		return err == nil // finished
	})
	for i, t := range tickets {
		if i >= keepTickets {
			_ = os.RemoveAll(t.path)
		}
	}
	// Review m12: a lost ticket (its runner ended without a result) never finishes, so its setup and runner copy stayed
	// for good; they go, its job and logs stay (upgrade-result answers lost from them).
	entries, _ := os.ReadDir(filepath.Join(dataDir, upgradeDirs.tickets))
	for _, e := range entries {
		if !e.IsDir() || !ticketRe.MatchString(e.Name()) {
			continue
		}
		if _, lost := ticketPending(dataDir, e.Name(), now); lost {
			dropTicketExes(filepath.Join(dataDir, upgradeDirs.tickets, e.Name()))
		}
	}
	results := newestFirst(filepath.Join(dataDir, upgradeDirs.results), func(e os.DirEntry) bool {
		return !e.IsDir() && strings.HasPrefix(e.Name(), "upgrade-") && strings.HasSuffix(e.Name(), ".json")
	})
	for i, r := range results {
		if i >= 2*keepTickets {
			_ = os.Remove(r.path)
		}
	}
}

// dropTicketExes removes a lost ticket's setup and runner copy (a file still in use stays).
func dropTicketExes(tdir string) {
	files, _ := os.ReadDir(tdir)
	for _, f := range files {
		if !f.IsDir() && strings.EqualFold(filepath.Ext(f.Name()), ".exe") {
			_ = os.Remove(filepath.Join(tdir, f.Name()))
		}
	}
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// upgradeResult answers upgrade-result: the result the runner wrote, {"ticket","pending":true} while it runs, or
// {"ticket","pending":false,"lost":true} when the runner ended without one.
func (l *Link) upgradeResult(_ linkproto.Envelope, stdin []byte) ([]byte, int) {
	var in struct {
		Ticket string `json:"ticket"`
	}
	if err := decodeStrictJSON(stdin, &in); err != nil || !ticketRe.MatchString(in.Ticket) {
		return answer(map[string]any{"refusal": upgradeRefuse("spec_invalid", `send {"ticket":"<32 hex digits>"}`, "the input is not a ticket")}, 1)
	}
	dir := l.upgradeDataDir()
	f, err := dataacl.OpenRead(dir, resultPath(dir, in.Ticket))
	if err == nil {
		defer func() { _ = f.Close() }()
		b, err := io.ReadAll(io.LimitReader(f, 256<<10))
		if err == nil && json.Valid(b) {
			return b, 0
		}
		return answer(map[string]any{"ticket": in.Ticket, "refusal": upgradeRefuse("result_unreadable", "read it on the box", "the result file cannot be read")}, 1)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return answer(map[string]any{"ticket": in.Ticket, "refusal": upgradeRefuse("result_unreadable", "read it on the box", "%v", err)}, 1)
	}
	pending, lost := ticketPending(dir, in.Ticket, l.cfg.Now())
	switch {
	case pending:
		return answer(map[string]any{"ticket": in.Ticket, "pending": true}, 0)
	case lost:
		dropTicketExes(filepath.Join(dir, upgradeDirs.tickets, in.Ticket)) // review m12: its setup is never run now
		return answer(map[string]any{"ticket": in.Ticket, "pending": false, "lost": true,
			"error": "the upgrade runner ended without writing a result: read upgrade\\" + in.Ticket + "\\runner.log and setup.log on the box, and state for what is installed"}, 0)
	}
	return answer(map[string]any{"ticket": in.Ticket, "refusal": upgradeRefuse("ticket_unknown", "", "no upgrade with ticket %s on this box", in.Ticket)}, 1)
}
