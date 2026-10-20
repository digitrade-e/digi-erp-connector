package provision

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/api"
	"github.com/digitrade-e/digi-erp-connector/internal/auth"
	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/atomicfile"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/autostart"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/filelock"
)

// ApplyResult is what Apply did. It never carries a secret.
type ApplyResult struct {
	ChangeSet string `json:"changeSet"`
	// PreimageCaptured: this run took the change set's pre-image (its first
	// run that changed something). A re-run keeps the first one.
	PreimageCaptured bool     `json:"preimageCaptured"`
	Steps            []Step   `json:"steps"`
	Restarted        bool     `json:"restarted"`
	Proof            *Proof   `json:"proof,omitempty"`
	RolledBack       bool     `json:"rolledBack,omitempty"`
	Warnings         []string `json:"warnings,omitempty"`
	// Check is what the run would do, when it was apply --check.
	Check *CheckResult `json:"check,omitempty"`

	secrets []string // seen or born here, for Redact; never marshalled
}

// CheckResult is the answer of apply --check: every check of a real run, its
// refusals included, then what it would change — and nothing written. digi-mcp
// shows it in its plan, so the box's own decisions reach the plan instead of
// being re-implemented there (plan 068 §10.2). Procedures are created where
// missing at apply; the check does not look.
type CheckResult struct {
	Config       string `json:"config"`                 // "unchanged", or configDiff's "changed: …"
	Login        string `json:"login"`                  // given | keep | create | rotate
	DBSecret     bool   `json:"dbSecret"`               // the stored DB password would change
	TLS          string `json:"tls,omitempty"`          // keep | generate, when the spec asks for a pair
	Firewall     string `json:"firewall,omitempty"`     // keep | set, when the spec names a rule
	Dependencies bool   `json:"dependencies,omitempty"` // the service's dependencies would change
	Recovery     bool   `json:"recovery"`               // the service's recovery actions would be set
	OrderCounter bool   `json:"orderCounter,omitempty"` // the order counter would move with sendOrderDir
	Restart      bool   `json:"restart"`                // the service would restart
}

// Step is one part of the change: whether it changed anything.
type Step struct {
	Name    string `json:"name"`
	Changed bool   `json:"changed"`
	Detail  string `json:"detail,omitempty"`
}

// Proof is the connector answering after a restart.
type Proof struct {
	Version string `json:"version,omitempty"`
	// Health is GET /api/health: ok, db_unavailable, or the status.
	Health  string  `json:"health"`
	Seconds float64 `json:"seconds"`
}

// orderNumberFile is the order counter, kept in sendOrderDir
// (hasavshevet.OrderNumberStore). A missing one starts again at 1.
const orderNumberFile = "lastOrderNumber.json"

// Apply makes the connector match s (see the package doc). Nothing is written
// before every check has passed; a failure after the first write puts this
// run's pre-image back.
func Apply(ctx context.Context, env Env, s Spec) (*ApplyResult, error) {
	return apply(ctx, env, s, false)
}

// ApplyCheck is apply --check: Apply's checks, then what it would change, with
// nothing written (CheckResult).
func ApplyCheck(ctx context.Context, env Env, s Spec) (*ApplyResult, error) {
	return apply(ctx, env, s, true)
}

func apply(ctx context.Context, env Env, s Spec, check bool) (*ApplyResult, error) {
	res := &ApplyResult{ChangeSet: s.ChangeSet, Steps: []Step{}}
	if r := s.Validate(); r != nil {
		return res, r
	}
	a := &applier{env: env, s: s, dir: env.Sys.DataDir(), res: res, check: check}
	unlock, err := lockRuns(a.dir)
	if err != nil {
		return res, err
	}
	defer unlock()
	err = a.run(ctx)
	a.settle()
	return res, err
}

// lockRuns holds provision\.lock for one run: two at once — a WinRM retry
// over a run still restarting the service — would interleave the login's
// password and the stored secret.
func lockRuns(dataDir string) (func(), error) {
	if err := os.MkdirAll(provisionDir(dataDir), 0o700); err != nil {
		return nil, refuse(CodeWriteFailed, "run it elevated (as an administrator)", "create the provision folder: %v", err)
	}
	held, err := filelock.Lock(filepath.Join(provisionDir(dataDir), ".lock"))
	if errors.Is(err, filelock.ErrLocked) {
		return nil, refuse(CodeBusy, "wait for the other run to finish", "another provisioning run is in progress on this machine")
	}
	if err != nil {
		return nil, refuse(CodeWriteFailed, "run it elevated (as an administrator)", "lock the provision folder: %v", err)
	}
	return func() { _ = held.Unlock() }, nil
}

type applier struct {
	env   Env
	s     Spec
	dir   string
	res   *ApplyResult
	check bool // apply --check: stop before the first write

	cur      config.Config
	curBytes []byte // nil: there is no config.yaml
	curMod   time.Time
	svc      autostart.ServiceInfo
	tgt      config.Config

	running    bool          // the service ran before this run
	fwBefore   *FirewallRule // the rule of the spec's name before this run (ours, or none)
	loginPlan  string        // given | keep | create | rotate
	moveOrders bool          // carry lastOrderNumber.json to the new sendOrderDir at the restart

	dbPassword string // what the connector will log in to SQL with
	rotated    bool   // this run set the least-privilege login's password in SQL Server
	restart    bool   // the config, the secret or the TLS pair changed
	captured   bool   // this run's pre-image exists: a failure from here on is rolled back
	wrote      bool   // something on the box changed
	touched    bool   // this run stopped (or started) the service
}

func (a *applier) step(name string, changed bool, detail string) {
	a.res.Steps = append(a.res.Steps, Step{Name: name, Changed: changed, Detail: detail})
	if changed {
		a.wrote = true
	}
}

func (a *applier) warn(format string, args ...any) {
	a.res.Warnings = append(a.res.Warnings, fmt.Sprintf(format, args...))
}

func (a *applier) run(ctx context.Context) error {
	// Everything that can refuse comes before the first write.
	if err := a.readCurrent(); err != nil {
		return err
	}
	dbUser := strings.TrimSpace(a.s.DB.User)
	if a.s.DB.LeastPrivilege {
		dbUser = a.s.DB.login()
	}
	a.tgt = buildTarget(a.cur, a.s, a.dir, dbUser)
	if strings.TrimSpace(a.tgt.Auth.Secret) == "" {
		secret, err := auth.NewSecret()
		if err != nil {
			return refuse(CodeWriteFailed, "run it again", "generate the signing secret: %v", err)
		}
		a.tgt.Auth.Secret = secret
	}
	// The daemon's own startup checks; the TLS pair is checked again once it
	// exists.
	check := a.tgt
	check.TLS = config.TLSConfig{}
	if err := api.CheckConfig(check); err != nil {
		return refuse(CodeConfigRefused, "fix the spec: the connector would refuse to start on this config", "%v", err)
	}
	for _, f := range []func() error{a.checkImporterStated, a.checkFilesExist, a.checkImporterWriters, a.checkOrderNumbers, a.checkFirewallOwner} {
		if err := f(); err != nil {
			return err
		}
	}
	if err := a.idle(ctx, "before anything is written"); err != nil {
		return err
	}
	if r := checkDBHost(ctx, a.env, a.tgt.DB.Host); r != nil {
		return r
	}
	admin, err := a.openAdmin()
	if err != nil {
		return err
	}
	defer admin.Close()
	if err := a.planLogin(ctx, admin); err != nil {
		return err
	}
	if a.check {
		a.res.Check = a.wouldDo()
		return nil
	}

	// The DACL first: the pre-image copies config.yaml — credential in clear —
	// into the data directory, so it lands only in a tree the walk accepted.
	if err := a.env.Sys.ProtectDataDir(); err != nil {
		return refuse(CodeWriteFailed, `see "The service will not start" in docs/troubleshooting.md`,
			"the data directory could not be protected, so nothing was written: %v", err)
	}
	if err := a.capture(); err != nil {
		return err
	}
	for _, f := range []func(context.Context, Database) error{a.procedures, a.applyLogin} {
		if err := f(ctx, admin); err != nil {
			return a.fail(err)
		}
	}
	for _, f := range []func() error{a.writeTLS, a.writeSecret, a.writeConfig, a.firewall, a.service} {
		if err := f(); err != nil {
			return a.fail(err)
		}
	}
	if err := a.env.Sys.ProtectDataDir(); err != nil {
		a.warn("the data directory could not be protected after the writes (%v); the service sets it at its start, and refuses to start if it cannot", err)
	}
	return a.restartAndProve(ctx)
}

// wouldDo is what the write steps would change, decided as they decide it.
func (a *applier) wouldDo() *CheckResult {
	c := &CheckResult{Config: "unchanged", Login: a.loginPlan, Recovery: !a.svc.RecoveryPlanned, OrderCounter: a.moveOrders}
	if out, err := config.Marshal(a.tgt); err != nil || !bytes.Equal(out, a.curBytes) {
		c.Config = configDiff(a.cur, a.tgt)
	}
	switch a.loginPlan {
	case "create", "rotate":
		c.DBSecret = true
	default:
		cur, err := a.env.Sys.SecretGet(config.DBPasswordKey(a.tgt.ERP))
		c.DBSecret = err != nil || string(cur) != a.dbPassword
	}
	if a.s.TLS != nil && a.s.TLS.Mode == "self-signed" {
		c.TLS = "generate"
		if a.tlsCovered() {
			c.TLS = "keep"
		}
	}
	if a.s.Firewall != nil {
		c.Firewall = "set"
		if a.fwBefore != nil && a.fwBefore.Same(a.wantRule()) {
			c.Firewall = "keep"
		}
	}
	if a.s.Service != nil {
		c.Dependencies = !sameFold(dedupe(a.s.Service.DependOn), a.svc.Dependencies)
	}
	c.Restart = c.Config != "unchanged" || c.DBSecret || c.TLS == "generate" || c.OrderCounter ||
		configStale(a.curMod, a.svc.ProcessStart) || a.svc.State != "running"
	return c
}

// settle runs after every run: the run's pre-image goes (it holds config.yaml
// in clear and is only for rolling this run back); a change-set pre-image
// this run took but did not need — nothing changed — goes too, so runs that
// change nothing never push a real restore point out; and old pre-images are
// pruned.
func (a *applier) settle() {
	_ = os.RemoveAll(runDir(a.dir, a.s.ChangeSet))
	if a.res.PreimageCaptured && !a.wrote && !a.rotated {
		_ = os.RemoveAll(preimageDir(a.dir, a.s.ChangeSet))
		a.res.PreimageCaptured = false
	}
	if a.res.PreimageCaptured {
		if err := prunePreimages(a.dir, a.s.ChangeSet); err != nil {
			a.warn("old pre-images were not pruned: %v", err)
		}
	}
}

// readCurrent reads config.yaml (absent is fine: a fresh install), guards
// against drift, and requires the service.
func (a *applier) readCurrent() error {
	if err := a.readConfig(); err != nil {
		return err
	}
	if e := a.s.Expect; e != nil && e.ConfigSHA256 != nil {
		if got := fileSHA(a.curBytes); got != *e.ConfigSHA256 {
			return refuse(CodeDrift, "discover again and make a new plan", "config.yaml changed after the plan read it")
		}
	}
	var err error
	a.svc, err = a.env.Sys.Service(ServiceName)
	if err != nil {
		return refuse(CodeServiceFailed, "run it elevated (as an administrator)", "read the %s service: %v", ServiceName, err)
	}
	if !a.svc.Installed {
		return refuse(CodeServiceMissing, "install the connector first (its installer creates the service)", "the %s service is not installed", ServiceName)
	}
	a.running = a.svc.State != "stopped"
	return nil
}

func (a *applier) readConfig() error {
	p := filepath.Join(a.dir, configFile)
	b, err := os.ReadFile(p)
	switch {
	case errors.Is(err, os.ErrNotExist):
		a.cur = config.Default()
		return nil
	case err != nil:
		return refuse(CodeWriteFailed, "run it elevated (as an administrator)", "read config.yaml: %v", err)
	}
	a.cur, err = config.Parse(b)
	if err != nil {
		return refuse(CodeConfigRefused, "fix config.yaml by hand or restore it", "config.yaml does not parse: %v", err)
	}
	a.curBytes = b
	if info, err := os.Stat(p); err == nil {
		a.curMod = info.ModTime()
	}
	return nil
}

// fileSHA is the sha256 Expect compares: "" for no file.
func fileSHA(b []byte) string {
	if b == nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// checkImporterStated refuses orders whose import nobody stated: has.exe, a
// BAT, or an explicit "none".
func (a *applier) checkImporterStated() error {
	if a.tgt.ERP != config.ERPHasavshevet || a.tgt.SendOrderDir == "" ||
		a.tgt.HasExePath != "" || a.tgt.HasBatFile != "" ||
		(a.s.Importer != nil && a.s.Importer.Mode == "none") {
		return nil
	}
	return refuse(CodeImporterRequired, `name has.exe and its parameter file ("mode": "hasExe"), a BAT ("bat"), or say "none"; orders are then written and imported by hand`,
		"orders go to %s but no importer is named", a.tgt.SendOrderDir)
}

// checkFilesExist refuses an order folder or importer that is not on this
// machine: a typo would otherwise surface on the first order.
func (a *applier) checkFilesExist() error {
	if d := a.tgt.SendOrderDir; d != "" {
		if info, err := os.Stat(d); err != nil || !info.IsDir() {
			return refuse(CodeSendOrderDirMissing, "create the folder Hasavshevet's importer reads, or leave sendOrderDir empty",
				"sendOrderDir %s is not a folder on this machine", d)
		}
	}
	param := a.tgt.HasParamFile
	if param != "" && !filepath.IsAbs(param) {
		param = filepath.Join(a.tgt.SendOrderDir, param) // has.exe runs from sendOrderDir
	}
	for _, p := range []string{a.tgt.HasExePath, param, a.tgt.HasBatFile} {
		if p == "" {
			continue
		}
		if info, err := os.Stat(p); err != nil || info.IsDir() {
			return refuse(CodeImporterMissing, "name a file that exists (discover lists candidates)", "importer file %s is not on this machine", p)
		}
	}
	return nil
}

// checkImporterWriters refuses an importer that accounts besides the
// administrators can change (System.Writers): the connector runs it as
// LocalSystem for every order, so it would be their code run as SYSTEM — on a
// domain controller, the whole domain's (plan 068's panel). allowWritable
// accepts it, as a risk a human was shown.
func (a *applier) checkImporterWriters() error {
	for _, p := range []string{a.tgt.HasExePath, a.tgt.HasBatFile} {
		if p == "" {
			continue
		}
		who, err := a.env.Sys.Writers(p)
		if err != nil {
			return refuse(CodeImporterWritable, "run it elevated (as an administrator)", "read who can change %s: %v", p, err)
		}
		if len(who) == 0 {
			continue
		}
		if a.s.Importer != nil && a.s.Importer.AllowWritable {
			a.warn("importer_writable: %s can be changed by %s, and the connector runs it as LocalSystem", p, strings.Join(who, ", "))
			continue
		}
		return refuse(CodeImporterWritable, "let only the administrators change it and its folder, or set importer.allowWritable once a human accepted the risk",
			"%s can be changed by %s, and the connector would run it as LocalSystem for every order", p, strings.Join(who, ", "))
	}
	return nil
}

// checkOrderNumbers: the order counter lives in sendOrderDir. A new folder
// without it would start again at 1 and hand Hasavshevet numbers it already
// has, so the counter moves with the folder (at the restart, when no order
// can come in); two different counters are a human's call.
func (a *applier) checkOrderNumbers() error {
	from, to := strings.TrimSpace(a.cur.SendOrderDir), strings.TrimSpace(a.tgt.SendOrderDir)
	if from == "" || to == "" || sameFolder(from, to) {
		return nil
	}
	old, err := os.ReadFile(filepath.Join(from, orderNumberFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return refuse(CodeWriteFailed, "run it elevated (as an administrator)", "read %s: %v", filepath.Join(from, orderNumberFile), err)
	}
	cur, err := os.ReadFile(filepath.Join(to, orderNumberFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
		a.moveOrders = true
		return nil
	case err != nil:
		return refuse(CodeWriteFailed, "run it elevated (as an administrator)", "read %s: %v", filepath.Join(to, orderNumberFile), err)
	}
	n, m := lastOrderNumber(old), lastOrderNumber(cur)
	if n == m && n >= 0 {
		return nil
	}
	return refuse(CodeOrderNumberConflict, "keep one counter by hand: Hasavshevet must never get a number twice",
		"%s and %s each hold an order counter (%d and %d)", from, to, n, m)
}

func lastOrderNumber(b []byte) int64 {
	var f struct {
		LastOrderNumber *int64 `json:"lastOrderNumber"`
	}
	if json.Unmarshal(b, &f) != nil || f.LastOrderNumber == nil {
		return -1
	}
	return *f.LastOrderNumber
}

// sameFolder compares two paths as the file system does: cleaned, links
// resolved, and without case on Windows.
func sameFolder(a, b string) bool {
	norm := func(p string) string {
		p = filepath.Clean(p)
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		return p
	}
	x, y := norm(a), norm(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(x, y)
	}
	return x == y
}

// checkFirewallOwner refuses a rule of the spec's name that this tool did
// not make: it is the client's, and it is never changed or removed.
func (a *applier) checkFirewallOwner() error {
	if a.s.Firewall == nil {
		return nil
	}
	name := ruleName(a.s.Firewall)
	rule, err := a.env.Sys.FirewallRule(name)
	if err != nil {
		return refuse(CodeFirewallFailed, "run it elevated (as an administrator)", "read the firewall rule %q: %v", name, err)
	}
	if rule != nil && !rule.Ours {
		return refuse(CodeFirewallForeign, "name another firewall.ruleName, or remove that rule yourself",
			"a firewall rule named %q exists and this tool did not make it", name)
	}
	a.fwBefore = rule
	return nil
}

// idle refuses unless the running connector holds no accepted order: the
// queue is in memory and a restart drops it. A stopped service holds none. It
// asks with the credential in config.yaml — the config the daemon runs, unless
// config.yaml changed after its start, and then only AssumeIdle gets past.
func (a *applier) idle(ctx context.Context, when string) error {
	svc, err := a.env.Sys.Service(ServiceName)
	if err != nil {
		return refuse(CodeServiceFailed, "run it elevated (as an administrator)", "read the %s service: %v", ServiceName, err)
	}
	if svc.State == "stopped" {
		return nil
	}
	d := a.env.daemonFor(a.cur)
	tok, err := d.login(ctx, a.cur.Auth.Username, a.cur.Auth.Password)
	if err == nil {
		var queued, running int
		queued, running, err = d.queue(ctx, tok)
		if err == nil {
			if queued+running > 0 {
				return refuse(CodeOrdersPending, "wait until they are written and run it again",
					"%d order(s) queued and %d being written %s: a restart now would lose them", queued, running, when)
			}
			return nil
		}
	}
	if a.s.AssumeIdle {
		a.warn("the order queue could not be read %s (%v); assumeIdle was set", when, err)
		return nil
	}
	return refuse(CodePendingUnknown, "update the connector (GET /api/sendOrder), wait for a quiet moment and set assumeIdle, or stop the service yourself",
		"the running connector's order queue cannot be read %s: %v", when, err)
}

// openAdmin opens the target database with the credential the client gave:
// the one with DDL rights, used for the procedures and the least-privilege
// login.
func (a *applier) openAdmin() (Database, error) {
	cfg := a.tgt
	cfg.DB.User = strings.TrimSpace(a.s.DB.User)
	dbc, err := a.env.OpenDB(cfg, a.s.DB.Password)
	if err != nil {
		return nil, refuse(CodeDBUnreachable, `check db.host (host\instance needs the SQL Server Browser), the port, the database and the credential`,
			"the database does not answer with the given credential: %v", err)
	}
	return dbc, nil
}

// planLogin settles, before anything is written, which login the connector
// will use. The least-privilege login gets a new password only when it is new,
// or when the spec says rotateLogin: it may be another connector's (the hand
// script creates the same name), and a password set in SQL Server cannot be
// taken back.
func (a *applier) planLogin(ctx context.Context, admin Database) error {
	if !a.s.DB.LeastPrivilege {
		a.loginPlan = "given"
		a.dbPassword = a.s.DB.Password
		return nil
	}
	login := a.s.DB.login()
	// The passwords this box knows for the login: its own key, and the
	// daemon's secret while the running config logs in as it (a box
	// provisioned before the key existed).
	var known []string
	if b, err := a.env.Sys.SecretGet(loginKey(login)); err == nil {
		known = append(known, string(b))
	}
	if strings.EqualFold(a.cur.DB.User, login) {
		if b, err := a.env.Sys.SecretGet(config.DBPasswordKey(a.tgt.ERP)); err == nil {
			known = append(known, string(b))
		}
	}
	var last error
	for _, pw := range known {
		a.res.secrets = append(a.res.secrets, pw)
		dbc, err := a.env.OpenDB(a.tgt, pw)
		if err == nil {
			_ = dbc.Close()
			a.loginPlan, a.dbPassword = "keep", pw
			return nil
		}
		last = err
	}
	if len(known) > 0 && !a.s.DB.RotateLogin {
		return refuse(CodeLeastPrivilege, "set db.rotateLogin to give it a new password; anything else that logs in as it then needs the new one",
			"%s's stored password does not log in: %v", login, last)
	}
	exists, err := admin.LoginExists(ctx, login)
	if err != nil {
		return refuse(CodeLeastPrivilege, "the given login needs VIEW ANY DEFINITION or to be an administrator", "look for %s: %v", login, err)
	}
	if exists && !a.s.DB.RotateLogin {
		return refuse(CodeLeastPrivilege, "name another login (db.login), or set db.rotateLogin to take this one over (its password changes)",
			"%s already exists and this box does not know its password: another connector or a person may use it", login)
	}
	a.loginPlan = "create"
	if exists {
		a.loginPlan = "rotate"
	}
	return nil
}

func (a *applier) capture() error {
	files := []string{configFile, secretFile(a.cur.ERP), secretFile(a.tgt.ERP), tlsCertFile, tlsKeyFile}
	var svc *preService
	if a.s.Service != nil {
		svc = &preService{Dependencies: slices.Clone(a.svc.Dependencies)}
	}
	var fw *preFirewall
	if a.s.Firewall != nil {
		fw = &preFirewall{Name: ruleName(a.s.Firewall), Rule: a.fwBefore}
	}
	first, err := capturePreimage(a.dir, a.s.ChangeSet, files, svc, fw, a.env.Now())
	if err != nil {
		return refuse(CodeWriteFailed, "run it elevated (as an administrator)", "the pre-image could not be written, so nothing was changed: %v", err)
	}
	a.res.PreimageCaptured = first
	a.captured = true
	return nil
}

func (a *applier) procedures(ctx context.Context, admin Database) error {
	if a.tgt.ERP != config.ERPHasavshevet {
		return nil
	}
	results, err := admin.EnsureProcedures(ctx)
	var created []string
	for _, r := range results {
		if r.Created {
			created = append(created, r.Name)
		}
	}
	if err != nil {
		return refuse(CodeProceduresFailed, "the given login needs CREATE PROCEDURE in the database",
			"the Hasavshevet procedures: %v", err)
	}
	detail := "present"
	if len(created) > 0 {
		detail = "created " + strings.Join(created, ", ")
	}
	a.step("procedures", len(created) > 0, detail)
	return nil
}

// applyLogin carries out planLogin's decision.
func (a *applier) applyLogin(ctx context.Context, admin Database) error {
	switch a.loginPlan {
	case "given":
		a.step("login", false, "the given login")
		return nil
	case "keep":
		// A box provisioned before the login's own key gets it now.
		if b, err := a.env.Sys.SecretGet(loginKey(a.s.DB.login())); err != nil || string(b) != a.dbPassword {
			if err := a.env.Sys.SecretSet(loginKey(a.s.DB.login()), []byte(a.dbPassword)); err != nil {
				return refuse(CodeWriteFailed, "run it elevated (as an administrator)", "keep %s's password: %v", a.s.DB.login(), err)
			}
		}
		a.step("login", false, a.s.DB.login()+" works with the stored password")
		return nil
	}
	pw, err := NewSQLPassword()
	if err != nil {
		return refuse(CodeLeastPrivilege, "run it again", "%v", err)
	}
	a.res.secrets = append(a.res.secrets, pw)
	// From here the password may have reached SQL Server even if a step
	// fails: treat it as set, so a rollback keeps the stored secret in step.
	login := a.s.DB.login()
	if err := a.env.Sys.SecretSet(loginKey(login), []byte(pw)); err != nil {
		return refuse(CodeWriteFailed, "run it elevated (as an administrator)", "keep %s's new password: %v", login, err)
	}
	a.rotated, a.dbPassword = true, pw
	if err := admin.EnsureLeastPrivilegeLogin(ctx, login, a.tgt.DB.Database, pw); err != nil {
		return refuse(CodeLeastPrivilege, "the given login needs ALTER ANY LOGIN and rights in the database",
			"%s %s: %v", a.loginPlan, login, err)
	}
	dbc, err := a.env.OpenDB(a.tgt, pw)
	if err != nil {
		return refuse(CodeLeastPrivilege, "check the server allows SQL logins (mixed mode)",
			"%s was set up but does not log in: %v", login, err)
	}
	_ = dbc.Close()
	what := "created"
	if a.loginPlan == "rotate" {
		what = "given a new password"
	}
	a.step("login", true, login+" "+what)
	return nil
}

func (a *applier) writeTLS() error {
	if a.s.TLS == nil || a.s.TLS.Mode != "self-signed" {
		return nil
	}
	if a.tlsCovered() {
		a.step("tls", false, "the pair covers "+strings.Join(a.s.TLS.Hosts, ", "))
		return nil
	}
	certPEM, keyPEM, err := selfSignedPair(a.s.TLS.Hosts, a.env.Now())
	if err != nil {
		return refuse(CodeWriteFailed, "run it again", "the TLS pair: %v", err)
	}
	if err := atomicfile.Write(filepath.Join(a.dir, tlsKeyFile), keyPEM, 0o600); err != nil {
		return refuse(CodeWriteFailed, "run it elevated (as an administrator)", "write the TLS key: %v", err)
	}
	a.wrote = true
	if err := atomicfile.Write(filepath.Join(a.dir, tlsCertFile), certPEM, 0o600); err != nil {
		return refuse(CodeWriteFailed, "run it elevated (as an administrator)", "write the TLS certificate: %v", err)
	}
	a.restart = true
	a.step("tls", true, "a self-signed pair for "+strings.Join(a.s.TLS.Hosts, ", "))
	return nil
}

// tlsCovered: the box's pair covers the spec's hosts and has time left.
func (a *applier) tlsCovered() bool {
	cert, _ := os.ReadFile(filepath.Join(a.dir, tlsCertFile))
	_, keyErr := os.Stat(filepath.Join(a.dir, tlsKeyFile))
	return keyErr == nil && certCovers(cert, a.s.TLS.Hosts, a.env.Now())
}

func (a *applier) writeSecret() error {
	key := config.DBPasswordKey(a.tgt.ERP)
	if cur, err := a.env.Sys.SecretGet(key); err == nil && string(cur) == a.dbPassword {
		a.step("db_secret", false, "")
		return nil
	}
	if err := a.env.Sys.SecretSet(key, []byte(a.dbPassword)); err != nil {
		return refuse(CodeWriteFailed, "run it elevated (as an administrator)", "store the DB password: %v", err)
	}
	a.wrote = true
	if back, err := a.env.Sys.SecretGet(key); err != nil || string(back) != a.dbPassword {
		return refuse(CodeSecretRoundTrip, "check the data directory's secrets folder", "the stored DB password does not read back")
	}
	a.restart = true
	a.step("db_secret", true, "stored with DPAPI")
	return nil
}

func (a *applier) writeConfig() error {
	if err := api.CheckConfig(a.tgt); err != nil {
		return refuse(CodeConfigRefused, "fix the spec: the connector would refuse to start on this config", "%v", err)
	}
	out, err := config.Marshal(a.tgt)
	if err != nil {
		return refuse(CodeWriteFailed, "report it", "%v", err)
	}
	if bytes.Equal(out, a.curBytes) {
		a.step("config", false, "")
		return nil
	}
	if err := atomicfile.Write(filepath.Join(a.dir, configFile), out, 0o600); err != nil {
		return refuse(CodeWriteFailed, "run it elevated (as an administrator)", "write config.yaml: %v", err)
	}
	a.restart = true
	a.step("config", true, configDiff(a.cur, a.tgt))
	return nil
}

// configDiff names the settings that changed, never their secret values.
func configDiff(cur, tgt config.Config) string {
	var out []string
	add := func(name string, changed bool) {
		if changed {
			out = append(out, name)
		}
	}
	add("erp", cur.ERP != tgt.ERP)
	add("apiListen", cur.APIListen != tgt.APIListen)
	add("db", cur.DB != tgt.DB)
	add("sendOrderDir", cur.SendOrderDir != tgt.SendOrderDir)
	add("importer", cur.HasExePath != tgt.HasExePath || cur.HasParamFile != tgt.HasParamFile || cur.HasBatFile != tgt.HasBatFile)
	add("auth", cur.Auth != tgt.Auth)
	add("tls", cur.TLS != tgt.TLS)
	if len(out) == 0 {
		return "rewritten in the connector's format"
	}
	return "changed: " + strings.Join(out, ", ")
}

func ruleName(f *FirewallSpec) string {
	if f.RuleName != "" {
		return f.RuleName
	}
	return DefaultFirewallRule
}

func listenPort(listen string) int {
	_, p, _ := net.SplitHostPort(listen)
	port, _ := strconv.Atoi(p)
	return port
}

func (a *applier) firewall() error {
	if a.s.Firewall == nil {
		return nil
	}
	want := a.wantRule()
	if profiles, err := a.env.Sys.FirewallProfiles(); err == nil && profiles.AllOff() {
		a.warn("firewall_inert: every Windows firewall profile is off, so the rule filters nothing: the client's perimeter is the only gate")
	}
	if a.fwBefore != nil && a.fwBefore.Same(want) {
		a.step("firewall", false, "")
		return nil
	}
	if err := a.env.Sys.SetFirewallRule(want); err != nil {
		return refuse(CodeFirewallFailed, "run it elevated (as an administrator)", "set the firewall rule: %v", err)
	}
	a.step("firewall", true, fmt.Sprintf("TCP %d from %s", want.Port, strings.Join(want.RemoteAddress, ", ")))
	return nil
}

// wantRule is the firewall rule the spec asks for.
func (a *applier) wantRule() FirewallRule {
	return FirewallRule{
		Name:          ruleName(a.s.Firewall),
		Enabled:       true,
		Port:          listenPort(a.tgt.APIListen),
		RemoteAddress: canonicalSources(a.s.Firewall.AllowFrom),
		Ours:          true,
	}
}

func (a *applier) service() error {
	if a.s.Service != nil {
		want := dedupe(a.s.Service.DependOn)
		if !sameFold(want, a.svc.Dependencies) {
			if err := a.env.Sys.SetServiceDependencies(ServiceName, want); err != nil {
				return refuse(CodeServiceFailed, "run it elevated (as an administrator)", "set the service's dependencies: %v", err)
			}
			a.step("service_dependencies", true, strings.Join(want, ", "))
		} else {
			a.step("service_dependencies", false, "")
		}
	}
	changed, err := a.env.Sys.EnsureServiceRecovery(ServiceName)
	if err != nil {
		return refuse(CodeServiceFailed, "run it elevated (as an administrator)", "set the service's recovery: %v", err)
	}
	a.step("service_recovery", changed, "")
	return nil
}

func sameFold(a, b []string) bool {
	norm := func(in []string) []string {
		out := make([]string, 0, len(in))
		for _, s := range in {
			out = append(out, strings.ToUpper(s))
		}
		return dedupe(out)
	}
	return slices.Equal(norm(a), norm(b))
}

// restartAndProve restarts the service when something it reads changed (or
// it runs a config older than config.yaml, or does not run), after asking
// the queue again; moves the order counter while it is stopped; then proves
// the port with the new credential. A failure puts the run's pre-image back.
func (a *applier) restartAndProve(ctx context.Context) error {
	stale := configStale(a.curMod, a.svc.ProcessStart)
	if !a.restart && !stale && a.svc.State == "running" && !a.moveOrders {
		a.step("restart", false, "nothing the service reads changed")
		return nil
	}
	if err := a.idle(ctx, "before the restart"); err != nil {
		return a.fail(err)
	}
	a.touched = true
	if err := a.env.Sys.StopService(ServiceName); err != nil {
		return a.fail(refuse(CodeServiceFailed, "stop it in services.msc and run it again", "stop the service: %v", err))
	}
	if a.moveOrders {
		if err := a.moveOrderCounter(); err != nil {
			return a.fail(err)
		}
	}
	if err := a.env.Sys.StartService(ServiceName); err != nil {
		return a.fail(refuse(CodePortProofFailed, "read server.log for why it stopped", "the service did not start: %v", err))
	}
	a.res.Restarted = true
	a.step("restart", true, "")
	proof, err := a.prove(ctx, a.tgt)
	if err != nil {
		return a.fail(refuse(CodePortProofFailed, "read server.log; the pre-image was put back", "%v", err))
	}
	a.res.Proof = proof
	if proof.Health != "ok" {
		a.warn("the connector answers but its database does not (/api/health: %s)", proof.Health)
	}
	return nil
}

// moveOrderCounter copies lastOrderNumber.json from the old order folder to
// the new one, read now — the service is stopped, so no order can move it.
func (a *applier) moveOrderCounter() error {
	from := filepath.Join(a.cur.SendOrderDir, orderNumberFile)
	to := filepath.Join(a.tgt.SendOrderDir, orderNumberFile)
	b, err := os.ReadFile(from)
	if err != nil {
		return refuse(CodeWriteFailed, "copy it by hand before the service takes an order", "read %s: %v", from, err)
	}
	if _, err := os.Stat(to); err == nil {
		return refuse(CodeOrderNumberConflict, "keep one counter by hand", "%s appeared during the run", to)
	}
	if err := atomicfile.Write(to, b, 0o600); err != nil {
		return refuse(CodeWriteFailed, "copy it by hand before the service takes an order", "write %s: %v", to, err)
	}
	a.step("order_counter", true, fmt.Sprintf("moved to %s at %d", a.tgt.SendOrderDir, lastOrderNumber(b)))
	return nil
}

// prove waits up to ProofTimeout for the connector on cfg's address to log in
// with cfg's credential and answer /api/ping, then reads /api/health.
func (a *applier) prove(ctx context.Context, cfg config.Config) (*Proof, error) {
	start := a.env.Now()
	deadline := start.Add(a.env.ProofTimeout)
	d := a.env.daemonFor(cfg)
	var last error
	for {
		tok, err := d.login(ctx, cfg.Auth.Username, cfg.Auth.Password)
		if err == nil {
			var p pingBody
			if p, err = d.ping(ctx, tok); err == nil {
				health := "ok"
				switch status, herr := d.get(ctx, "/api/health", tok, nil); {
				case herr != nil:
					health = "error: " + herr.Error()
				case status == http.StatusServiceUnavailable:
					health = "db_unavailable"
				case status != http.StatusOK:
					health = "status " + strconv.Itoa(status)
				}
				return &Proof{Version: p.Version, Health: health, Seconds: a.env.Now().Sub(start).Seconds()}, nil
			}
		}
		last = err
		if !a.env.Now().Before(deadline) {
			return nil, fmt.Errorf("the connector did not answer on %s within %s: %v", cfg.APIListen, a.env.ProofTimeout, last)
		}
		a.env.Sleep(time.Second)
	}
}

// fail puts this run's pre-image back when there is one and returns cause.
func (a *applier) fail(cause error) error {
	if !a.captured {
		return cause
	}
	if err := a.rollback(); err != nil {
		a.warn("the rollback did not finish: %v; restore by hand from provision\\%s\\run", err, a.s.ChangeSet)
	}
	return cause
}

// rollback returns the box to where this run found it: the files, the
// firewall rule and the dependencies from the run's pre-image; the
// least-privilege login's password kept if this run set it and the restored
// config logs in as it (SQL Server has no other); the service as it was —
// running on the restored config, or stopped.
func (a *applier) rollback() error {
	a.res.RolledBack = true
	p, err := loadPreimage(runDir(a.dir, a.s.ChangeSet))
	if err != nil {
		return fmt.Errorf("read the run's pre-image: %w", err)
	}
	if _, err := putBack(a.env, p); err != nil {
		return err
	}
	var pw []byte
	if a.dbPassword != "" {
		pw = []byte(a.dbPassword)
	}
	if err := keepLoginPassword(a.env, a.tgt.DB.User, pw); err != nil {
		return err
	}
	if err := a.env.Sys.ProtectDataDir(); err != nil {
		a.warn("the data directory could not be protected after the rollback: %v", err)
	}
	if !a.touched {
		return nil // it still runs the old config, from memory
	}
	_ = a.env.Sys.StopService(ServiceName)
	// The target's order folder may have issued numbers while it ran.
	if err := a.carryOrderCounter(a.tgt.SendOrderDir, a.cur.SendOrderDir); err != nil {
		return fmt.Errorf("the service is left stopped: %w; copy %s by hand before starting it", err, orderNumberFile)
	}
	if !a.running {
		return nil // it was stopped before this run
	}
	if err := a.env.Sys.StartService(ServiceName); err != nil {
		return fmt.Errorf("the service did not start on the restored config: %w", err)
	}
	return nil
}

// keepLoginPassword makes the daemon's DB secret the current password of the
// login the config on disk uses, when this box knows it: that login's own key
// (loginKey, kept out of every pre-image), else before — the secret in use
// until now — when userBefore is the same login. A credential only moves
// forward: SQL Server knows no older password, and a restored one would lock
// the connector out.
func keepLoginPassword(env Env, userBefore string, before []byte) error {
	b, err := os.ReadFile(filepath.Join(env.Sys.DataDir(), configFile))
	if err != nil {
		return nil // no config: nothing logs in
	}
	cfg, err := config.Parse(b)
	if err != nil {
		return nil
	}
	pw := before
	if !strings.EqualFold(cfg.DB.User, userBefore) {
		pw = nil
	}
	if user := strings.ToLower(cfg.DB.User); loginNameRe.MatchString(user) {
		if k, err := env.Sys.SecretGet(loginKey(user)); err == nil {
			pw = k
		}
	}
	if pw == nil {
		return nil
	}
	if err := env.Sys.SecretSet(config.DBPasswordKey(cfg.ERP), pw); err != nil {
		return fmt.Errorf("keep %s's current password: %w", cfg.DB.User, err)
	}
	return nil
}

// carryOrderCounter gives the order folder to a counter no lower than from's.
// Run while the service is stopped, when a restore or a rollback takes the
// connector back to an older sendOrderDir: moveOrderCounter copied the counter
// and left the old file behind, so that folder's counter is stale, and
// Hasavshevet must never get a number twice.
func (a *applier) carryOrderCounter(from, to string) error {
	if from == "" || to == "" || sameFolder(from, to) {
		return nil
	}
	src := filepath.Join(from, orderNumberFile)
	b, err := os.ReadFile(src)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	n := lastOrderNumber(b)
	if n < 0 {
		return fmt.Errorf("%s holds no order counter", src)
	}
	dst := filepath.Join(to, orderNumberFile)
	cur, err := os.ReadFile(dst)
	switch {
	case err == nil && lastOrderNumber(cur) >= n:
		return nil
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("read %s: %w", dst, err)
	}
	if err := atomicfile.Write(dst, b, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	a.step("order_counter", true, fmt.Sprintf("carried to %s at %d", to, n))
	return nil
}

// putBack restores a pre-image's files, firewall rule and dependencies.
func putBack(env Env, p preimage) (bool, error) {
	changed, err := restoreFiles(env.Sys.DataDir(), p)
	if err != nil {
		return changed, err
	}
	if fw := p.m.Firewall; fw != nil {
		cur, err := env.Sys.FirewallRule(fw.Name)
		if err != nil {
			return changed, fmt.Errorf("read the firewall rule: %w", err)
		}
		switch {
		case cur != nil && !cur.Ours:
			// Not ours now: leave it alone, whatever the pre-image says.
		case fw.Rule == nil && cur != nil:
			if err := env.Sys.RemoveFirewallRule(fw.Name); err != nil {
				return changed, fmt.Errorf("remove the firewall rule: %w", err)
			}
			changed = true
		case fw.Rule != nil && (cur == nil || !cur.Same(*fw.Rule)):
			if err := env.Sys.SetFirewallRule(*fw.Rule); err != nil {
				return changed, fmt.Errorf("restore the firewall rule: %w", err)
			}
			changed = true
		}
	}
	if svc := p.m.Service; svc != nil {
		cur, err := env.Sys.Service(ServiceName)
		if err != nil {
			return changed, fmt.Errorf("read the service: %w", err)
		}
		if !sameFold(cur.Dependencies, svc.Dependencies) {
			if err := env.Sys.SetServiceDependencies(ServiceName, svc.Dependencies); err != nil {
				return changed, fmt.Errorf("restore the service's dependencies: %w", err)
			}
			changed = true
		}
	}
	return changed, nil
}
