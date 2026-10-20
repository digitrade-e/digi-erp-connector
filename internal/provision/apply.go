package provision

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/api"
	"github.com/digitrade-e/digi-erp-connector/internal/auth"
	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/atomicfile"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/autostart"
)

// ApplyResult is what Apply did. It never carries a secret.
type ApplyResult struct {
	ChangeSet string `json:"changeSet"`
	// PreimageCaptured is false when this change set already had a pre-image
	// (a re-run): the one from before its first run is kept.
	PreimageCaptured bool     `json:"preimageCaptured"`
	Steps            []Step   `json:"steps"`
	Restarted        bool     `json:"restarted"`
	Proof            *Proof   `json:"proof,omitempty"`
	RolledBack       bool     `json:"rolledBack,omitempty"`
	Warnings         []string `json:"warnings,omitempty"`

	generated []string // passwords born here, for Redact; never marshalled
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

// Apply makes the connector match s (see the package doc). On a refusal the
// result says what had been done; nothing is written before every check has
// passed, and a failed port proof puts the pre-image back.
func Apply(ctx context.Context, env Env, s Spec) (*ApplyResult, error) {
	res := &ApplyResult{ChangeSet: s.ChangeSet, Steps: []Step{}}
	if r := s.Validate(); r != nil {
		return res, r
	}
	a := &applier{env: env, s: s, dir: env.Sys.DataDir(), res: res}
	return res, a.run(ctx)
}

type applier struct {
	env Env
	s   Spec
	dir string
	res *ApplyResult

	cur      config.Config
	curBytes []byte // nil: there is no config.yaml
	curMod   time.Time
	svc      autostart.ServiceInfo
	tgt      config.Config

	dbPassword string // what the connector will log in to SQL with
	restart    bool   // the config, the secret or the TLS pair changed
	captured   bool   // a pre-image exists: a failure from here on can be rolled back
	stopped    bool   // this run stopped the service: a rollback starts it again
}

func (a *applier) step(name string, changed bool, detail string) {
	a.res.Steps = append(a.res.Steps, Step{Name: name, Changed: changed, Detail: detail})
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
		dbUser = LeastPrivilegeLogin
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
	if err := a.checkFilesExist(); err != nil {
		return err
	}
	if err := a.idle(ctx, "before anything is written"); err != nil {
		return err
	}
	admin, err := a.openAdmin()
	if err != nil {
		return err
	}
	defer admin.Close()

	// The DACL first: the pre-image copies config.yaml — credential in clear —
	// into the data directory, so it lands only in a tree the walk accepted.
	if err := a.env.Sys.ProtectDataDir(); err != nil {
		return refuse(CodeWriteFailed, `see "The service will not start" in docs/troubleshooting.md`,
			"the data directory could not be protected, so nothing was written: %v", err)
	}
	if err := a.capture(); err != nil {
		return err
	}

	for _, f := range []func(context.Context, Database) error{a.procedures, a.login} {
		if err := f(ctx, admin); err != nil {
			return err
		}
	}
	for _, f := range []func() error{a.writeTLS, a.writeSecret, a.writeConfig, a.firewall, a.service} {
		if err := f(); err != nil {
			return a.fail(ctx, err)
		}
	}
	if err := a.env.Sys.ProtectDataDir(); err != nil {
		a.warn("the data directory could not be protected after the writes (%v); the service sets it at its start, and refuses to start if it cannot", err)
	}
	if err := a.restartAndProve(ctx); err != nil {
		return err
	}
	if err := prunePreimages(a.dir, a.s.ChangeSet); err != nil {
		a.warn("old pre-images were not pruned: %v", err)
	}
	return nil
}

// readCurrent reads config.yaml (absent is fine: a fresh install), guards
// against drift, and requires the service.
func (a *applier) readCurrent() error {
	p := filepath.Join(a.dir, configFile)
	b, err := os.ReadFile(p)
	switch {
	case errors.Is(err, os.ErrNotExist):
		a.cur = config.Default()
	case err != nil:
		return refuse(CodeWriteFailed, "run it elevated (as an administrator)", "read config.yaml: %v", err)
	default:
		a.cur, err = config.Parse(b)
		if err != nil {
			return refuse(CodeConfigRefused, "fix config.yaml by hand or restore it", "config.yaml does not parse: %v", err)
		}
		a.curBytes = b
		if info, err := os.Stat(p); err == nil {
			a.curMod = info.ModTime()
		}
	}
	if e := a.s.Expect; e != nil && e.ConfigSHA256 != nil {
		if got := fileSHA(a.curBytes); got != *e.ConfigSHA256 {
			return refuse(CodeDrift, "discover again and make a new plan", "config.yaml changed after the plan read it")
		}
	}
	a.svc, err = a.env.Sys.Service(ServiceName)
	if err != nil {
		return refuse(CodeServiceFailed, "run it elevated (as an administrator)", "read the %s service: %v", ServiceName, err)
	}
	if !a.svc.Installed {
		return refuse(CodeServiceMissing, "install the connector first (its installer creates the service)", "the %s service is not installed", ServiceName)
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

func (a *applier) capture() error {
	files := []string{configFile, secretFile(a.cur.ERP), secretFile(a.tgt.ERP), tlsCertFile, tlsKeyFile}
	var svc *preService
	if a.s.Service != nil {
		svc = &preService{Dependencies: slices.Clone(a.svc.Dependencies)}
	}
	var fw *preFirewall
	if a.s.Firewall != nil {
		name := ruleName(a.s.Firewall)
		rule, err := a.env.Sys.FirewallRule(name)
		if err != nil {
			return refuse(CodeFirewallFailed, "run it elevated (as an administrator)", "read the firewall rule %q: %v", name, err)
		}
		fw = &preFirewall{Name: name, Rule: rule}
	}
	captured, err := capturePreimage(a.dir, a.s.ChangeSet, files, svc, fw, a.env.Now())
	if err != nil {
		return refuse(CodeWriteFailed, "run it elevated (as an administrator)", "the pre-image could not be written, so nothing was changed: %v", err)
	}
	a.res.PreimageCaptured = captured
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

// login settles the credential the connector logs in to SQL with. With least
// privilege, a login that already works with the stored password is kept —
// a second run must not rotate it.
func (a *applier) login(ctx context.Context, admin Database) error {
	if !a.s.DB.LeastPrivilege {
		a.dbPassword = a.s.DB.Password
		a.step("login", false, "the given login")
		return nil
	}
	if strings.EqualFold(a.cur.DB.User, LeastPrivilegeLogin) {
		if stored, err := a.env.Sys.SecretGet(config.DBPasswordKey(a.tgt.ERP)); err == nil {
			if dbc, err := a.env.OpenDB(a.tgt, string(stored)); err == nil {
				_ = dbc.Close()
				a.dbPassword = string(stored)
				a.step("login", false, LeastPrivilegeLogin+" works with the stored password")
				return nil
			}
		}
	}
	pw, err := NewSQLPassword()
	if err != nil {
		return refuse(CodeLeastPrivilege, "run it again", "%v", err)
	}
	a.res.generated = append(a.res.generated, pw)
	if err := admin.EnsureLeastPrivilegeLogin(ctx, a.tgt.DB.Database, pw); err != nil {
		return refuse(CodeLeastPrivilege, "the given login needs ALTER ANY LOGIN and rights in the database",
			"create %s: %v", LeastPrivilegeLogin, err)
	}
	dbc, err := a.env.OpenDB(a.tgt, pw)
	if err != nil {
		return refuse(CodeLeastPrivilege, "check the server allows SQL logins (mixed mode)",
			"%s was created but does not log in: %v", LeastPrivilegeLogin, err)
	}
	_ = dbc.Close()
	a.dbPassword = pw
	a.step("login", true, LeastPrivilegeLogin+" created or given a new password")
	return nil
}

func (a *applier) writeTLS() error {
	if a.s.TLS == nil || a.s.TLS.Mode != "self-signed" {
		return nil
	}
	cert, _ := os.ReadFile(filepath.Join(a.dir, tlsCertFile))
	_, keyErr := os.Stat(filepath.Join(a.dir, tlsKeyFile))
	if keyErr == nil && certCovers(cert, a.s.TLS.Hosts, a.env.Now()) {
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
	if err := atomicfile.Write(filepath.Join(a.dir, tlsCertFile), certPEM, 0o600); err != nil {
		return refuse(CodeWriteFailed, "run it elevated (as an administrator)", "write the TLS certificate: %v", err)
	}
	a.restart = true
	a.step("tls", true, "a self-signed pair for "+strings.Join(a.s.TLS.Hosts, ", "))
	return nil
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
	want := FirewallRule{
		Name:          ruleName(a.s.Firewall),
		Enabled:       true,
		Port:          listenPort(a.tgt.APIListen),
		RemoteAddress: canonicalSources(a.s.Firewall.AllowFrom),
	}
	if profiles, err := a.env.Sys.FirewallProfiles(); err == nil && profiles.AllOff() {
		a.warn("firewall_inert: every Windows firewall profile is off, so the rule filters nothing — the client's perimeter is the only gate")
	}
	have, err := a.env.Sys.FirewallRule(want.Name)
	if err != nil {
		return refuse(CodeFirewallFailed, "run it elevated (as an administrator)", "read the firewall rule: %v", err)
	}
	if have != nil && have.Same(want) {
		a.step("firewall", false, "")
		return nil
	}
	if err := a.env.Sys.SetFirewallRule(want); err != nil {
		return refuse(CodeFirewallFailed, "run it elevated (as an administrator)", "set the firewall rule: %v", err)
	}
	a.step("firewall", true, fmt.Sprintf("TCP %d from %s", want.Port, strings.Join(want.RemoteAddress, ", ")))
	return nil
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
// the queue again, then proves the port with the new credential. A failure
// puts the pre-image back.
func (a *applier) restartAndProve(ctx context.Context) error {
	stale := configStale(a.curMod, a.svc.ProcessStart)
	if !a.restart && !stale && a.svc.State == "running" {
		a.step("restart", false, "nothing the service reads changed")
		return nil
	}
	if err := a.idle(ctx, "before the restart"); err != nil {
		return a.fail(ctx, err)
	}
	a.stopped = true
	if err := a.env.Sys.StopService(ServiceName); err != nil {
		return a.fail(ctx, refuse(CodeServiceFailed, "stop it in services.msc and run it again", "stop the service: %v", err))
	}
	if err := a.env.Sys.StartService(ServiceName); err != nil {
		return a.fail(ctx, refuse(CodePortProofFailed, "read server.log for why it stopped", "the service did not start: %v", err))
	}
	a.res.Restarted = true
	a.step("restart", true, "")
	proof, err := a.prove(ctx, a.tgt)
	if err != nil {
		return a.fail(ctx, refuse(CodePortProofFailed, "read server.log; the pre-image was put back", "%v", err))
	}
	a.res.Proof = proof
	if proof.Health != "ok" {
		a.warn("the connector answers but its database does not (/api/health: %s)", proof.Health)
	}
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

// fail puts the pre-image back when there is one — restarting the service on
// it only if this run had stopped it — and returns cause.
func (a *applier) fail(ctx context.Context, cause error) error {
	if !a.captured {
		return cause
	}
	if err := a.rollback(ctx); err != nil {
		a.warn("the rollback did not finish: %v — run restore %s", err, a.s.ChangeSet)
	}
	return cause
}

func (a *applier) rollback(ctx context.Context) error {
	a.res.RolledBack = true
	m, err := loadPreimage(a.dir, a.s.ChangeSet)
	if err != nil {
		return fmt.Errorf("read the pre-image: %w", err)
	}
	if _, err := putBack(a.env, m); err != nil {
		return err
	}
	if err := a.env.Sys.ProtectDataDir(); err != nil {
		a.warn("the data directory could not be protected after the rollback: %v", err)
	}
	if !a.stopped {
		return nil // it still runs the old config, from memory
	}
	_ = a.env.Sys.StopService(ServiceName)
	if err := a.env.Sys.StartService(ServiceName); err != nil {
		return fmt.Errorf("the service did not start on the restored config: %w", err)
	}
	return nil
}

// putBack restores a pre-image's files, firewall rule and dependencies.
func putBack(env Env, m manifest) (bool, error) {
	changed, err := restoreFiles(env.Sys.DataDir(), m)
	if err != nil {
		return changed, err
	}
	if fw := m.Firewall; fw != nil {
		if fw.Rule == nil {
			if err := env.Sys.RemoveFirewallRule(fw.Name); err != nil {
				return changed, fmt.Errorf("remove the firewall rule: %w", err)
			}
		} else if err := env.Sys.SetFirewallRule(*fw.Rule); err != nil {
			return changed, fmt.Errorf("restore the firewall rule: %w", err)
		}
		changed = true
	}
	if svc := m.Service; svc != nil {
		if err := env.Sys.SetServiceDependencies(ServiceName, svc.Dependencies); err != nil {
			return changed, fmt.Errorf("restore the service's dependencies: %w", err)
		}
		changed = true
	}
	return changed, nil
}
