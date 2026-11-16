package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
	"hash/fnv"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/autostart"
	"github.com/digitrade-e/digi-erp-connector/internal/secrets"
)

// fakeDaemon is the connector's API as provisioning sees it: a login, ping,
// health and the order queue. On a service start it loads config.yaml again,
// as the real daemon does.
type fakeDaemon struct {
	mu           sync.Mutex
	user, pass   string
	queued       int
	running      int
	health       int
	version      string
	noQueueRoute bool
	down         bool // refuses every login with 503: it never comes up
	// queuedFrom: from the queuedFrom-th question about the queue on (1-based), it answers queuedLater orders — an
	// order that arrives in the middle of a run. 0: never.
	queuedFrom, queuedLater, queueAsks int
	srv                                *httptest.Server
}

func newFakeDaemon(t *testing.T) *fakeDaemon {
	d := &fakeDaemon{health: http.StatusOK, version: "v1.0.99"}
	d.srv = httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *fakeDaemon) serve(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r.Method == http.MethodPost && r.URL.Path == "/auth/token" {
		var body struct{ Username, Password string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch {
		case d.down:
			w.WriteHeader(http.StatusServiceUnavailable)
		case body.Username == d.user && body.Password == d.pass && d.pass != "":
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "issued"})
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
		return
	}
	if r.Header.Get("Authorization") != "Bearer issued" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/ping":
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": 1, "version": d.version})
	case r.Method == http.MethodGet && r.URL.Path == "/api/health":
		w.WriteHeader(d.health)
	case r.Method == http.MethodGet && r.URL.Path == "/api/sendOrder" && !d.noQueueRoute:
		d.queueAsks++
		queued := d.queued
		if d.queuedFrom > 0 && d.queueAsks >= d.queuedFrom {
			queued = d.queuedLater
		}
		_ = json.NewEncoder(w).Encode(map[string]int{"queued": queued, "running": d.running})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (d *fakeDaemon) set(f func(*fakeDaemon)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f(d)
}

// fakeSys is a Windows box in a temp directory. Secrets are files under
// secrets\, in clear (the unix build of the secrets store does the same).
type fakeSys struct {
	dir        string
	svc        autostart.ServiceInfo
	sqlBrowser autostart.ServiceInfo
	rules      map[string]FirewallRule
	profiles   FirewallProfiles
	listeners  []Listener
	instances  []SQLInstance
	host       HostInfo
	candidates []string
	writers    map[string][]string        // who besides the administrators can change a file (or a folder: FolderWriters)
	accounts   map[string]dataacl.Account // LookupAccount, by lower-case name
	// folder-acl: the folders' security (one SDDL each), plan errors, parent writers
	folderSD      map[string]string
	lockdownErr   map[string]error
	parentWriters map[string][]dataacl.Writer
	executables   map[string][]string // the plan's executable entries, by folder
	finalPath     map[string]string   // the plan's final path, by folder (default \\?\<dir>)
	lastExecExts  map[string]bool     // what the last plan was asked to flag
	lastTrusted   []string            // and whom it trusted
	folderRules   FolderRules         // FolderRules (review T3); none by default
	reparse       map[string]string   // ReparsePointOn by path; otherwise only the path itself is looked at
	daemon        *fakeDaemon
	// onStart runs when the service starts, after the daemon reloaded.
	onStart func(cfg config.Config)
	calls   []string
	// daemonDBPass is the DB password the running daemon read at its start (its pool's, until the next start).
	daemonDBPass string
	// failSecretOnce makes the next SecretSet of that key fail.
	failSecretOnce string
	// depState overrides an instance's service (its start type, its state).
	depState map[string]autostart.ServiceInfo
}

func newFakeSys(t *testing.T, d *fakeDaemon) *fakeSys {
	return &fakeSys{
		dir:   t.TempDir(),
		svc:   autostart.ServiceInfo{Installed: true, State: "running", StartType: "auto", Account: "LocalSystem", ProcessStart: time.Now().Add(-time.Hour), ProcessID: 4242},
		rules: map[string]FirewallRule{},
		// The pilot's Browser runs; its start type is the healthy one.
		sqlBrowser: autostart.ServiceInfo{Installed: true, State: "running", StartType: "auto"},
		host:       HostInfo{Name: "ERP01", Domain: "example.local", PartOfDomain: true, DomainRole: 3, Elevated: true},
		daemon:     d,
	}
}

func (s *fakeSys) call(c string) { s.calls = append(s.calls, c) }

func (s *fakeSys) DataDir() string       { return s.dir }
func (s *fakeSys) ProtectDataDir() error { s.call("protect"); return nil }

func (s *fakeSys) secretPath(key string) string {
	return filepath.Join(s.dir, "secrets", key+".bin")
}

func (s *fakeSys) SecretGet(key string) ([]byte, error) {
	b, err := os.ReadFile(s.secretPath(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil, secrets.ErrNotFound
	}
	return b, err
}

func (s *fakeSys) SecretSet(key string, v []byte) error {
	s.call("secret:" + key)
	if s.failSecretOnce == key {
		s.failSecretOnce = ""
		return errors.New("the secret store refused")
	}
	if err := os.MkdirAll(filepath.Dir(s.secretPath(key)), 0o700); err != nil {
		return err
	}
	return os.WriteFile(s.secretPath(key), v, 0o600)
}

func (s *fakeSys) Service(name string) (autostart.ServiceInfo, error) {
	switch name {
	case ServiceName:
		return s.svc, nil
	case "SQLBrowser":
		return s.sqlBrowser, nil
	}
	for _, in := range s.instances {
		if in.Service == name {
			if st, ok := s.depState[name]; ok {
				return st, nil
			}
			return autostart.ServiceInfo{Installed: true, State: "running", StartType: "auto", ProcessID: 900}, nil
		}
	}
	return autostart.ServiceInfo{}, nil
}

func (s *fakeSys) SetServiceDependencies(_ string, deps []string) error {
	s.call("deps:" + strings.Join(deps, ","))
	s.svc.Dependencies = deps
	return nil
}

func (s *fakeSys) EnsureServiceRecovery(string) (bool, error) {
	if s.svc.RecoveryPlanned {
		return false, nil
	}
	s.call("recovery")
	s.svc.RecoveryPlanned = true
	return true, nil
}

func (s *fakeSys) StopService(string) error {
	s.call("stop")
	s.svc.State = "stopped"
	s.svc.ProcessID = 0
	return nil
}

func (s *fakeSys) StartService(string) error {
	s.call("start")
	s.svc.State = "running"
	s.svc.ProcessID = 4243
	s.svc.ProcessStart = time.Now()
	b, err := os.ReadFile(filepath.Join(s.dir, configFile))
	if err != nil {
		return err
	}
	cfg, err := config.Parse(b)
	if err != nil {
		return err
	}
	s.daemon.set(func(d *fakeDaemon) { d.user, d.pass = cfg.Auth.Username, cfg.Auth.Password })
	pw, _ := s.SecretGet(config.DBPasswordKey(cfg.ERP))
	s.daemonDBPass = string(pw)
	if s.onStart != nil {
		s.onStart(cfg)
	}
	return nil
}

func (s *fakeSys) FirewallProfiles() (FirewallProfiles, error) { return s.profiles, nil }

func (s *fakeSys) FirewallRule(name string) (*FirewallRule, error) {
	if r, ok := s.rules[name]; ok {
		return &r, nil
	}
	return nil, nil
}

// SetFirewallRule tags the rule as the real one does (its group).
func (s *fakeSys) SetFirewallRule(r FirewallRule) error {
	s.call("rule:" + r.Name)
	r.Ours = true
	s.rules[r.Name] = r
	return nil
}

func (s *fakeSys) RemoveFirewallRule(name string) error {
	s.call("unrule:" + name)
	delete(s.rules, name)
	return nil
}

func (s *fakeSys) Listeners() ([]Listener, error)       { return s.listeners, nil }
func (s *fakeSys) SQLInstances() ([]SQLInstance, error) { return s.instances, nil }
func (s *fakeSys) Host() (HostInfo, error)              { return s.host, nil }
func (s *fakeSys) Writers(path string) ([]string, error) {
	if w := s.writers[path]; w != nil {
		return w, nil
	}
	return []string{}, nil
}

// FolderWriters answers from the same map, keyed by the folder, each name
// with its SID (accounts, else a fake one derived from the name).
func (s *fakeSys) FolderWriters(dir string) ([]dataacl.Writer, error) {
	out := []dataacl.Writer{}
	for _, n := range s.writers[dir] {
		out = append(out, dataacl.Writer{Name: n, SID: s.sidOf(n)})
	}
	return out, nil
}

func (s *fakeSys) sidOf(name string) string {
	if a, ok := s.accounts[strings.ToLower(name)]; ok {
		return a.SID
	}
	h := fnv.New32a()
	h.Write([]byte(strings.ToLower(name)))
	return fmt.Sprintf("S-1-5-21-0-0-0-%d", 1000+h.Sum32()%100000)
}

// The folder-acl fakes: a folder's security is one SDDL string; a lock-down
// replaces it (and clears its writers), a restore puts the recorded one back.
func (s *fakeSys) PlanFolderLockdown(dir string, trusted []string, execExts map[string]bool) (dataacl.LockdownPlan, error) {
	s.call("plan-lockdown:" + filepath.Base(dir))
	s.lastExecExts, s.lastTrusted = execExts, trusted
	if err := s.lockdownErr[dir]; err != nil {
		return dataacl.LockdownPlan{}, err
	}
	cur := s.folderSD[dir]
	if cur == "" {
		cur = "O:S-1-5-21-0-0-0-1001D:AI(A;OICIID;0x1301bf;;;BU)"
	}
	final := s.finalPath[dir]
	if final == "" {
		final = `\\?\` + dir
	}
	plan := dataacl.LockdownPlan{Entries: []dataacl.FolderEntry{{Rel: "", FileID: "id-" + dir, SDDL: cur}, {Rel: "history", FileID: "id-h", SDDL: cur}},
		Principals:     []dataacl.PrincipalRights{{SID: "S-1-5-32-545", Name: `BUILTIN\Users`, Before: "modify", After: "read", Entries: 2}},
		Owners:         []dataacl.OwnerChange{{SID: "S-1-5-21-0-0-0-1001", Name: `BFL\olduser`, Entries: 1}},
		EntriesChanged: 2, Executables: s.executables[dir], FinalPath: final, ParentWriters: s.parentWriters[dir]}
	return plan, nil
}

func (s *fakeSys) ApplyFolderLockdown(dir string, _ []string, plan dataacl.LockdownPlan) (int, error) {
	s.call("lockdown:" + filepath.Base(dir))
	if s.folderSD == nil {
		s.folderSD = map[string]string{}
	}
	s.folderSD[dir] = "O:BAD:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	delete(s.writers, dir)
	return len(plan.Entries), nil
}

func (s *fakeSys) RestoreFolderACL(dir string, entries []dataacl.FolderEntry) (int, int, error) {
	s.call("restore-acl:" + filepath.Base(dir))
	if s.folderSD == nil {
		s.folderSD = map[string]string{}
	}
	s.folderSD[dir] = entries[0].SDDL
	return len(entries), 0, nil
}

// LookupAccount answers from accounts (keyed by the lower-case name).
func (s *fakeSys) LookupAccount(name string) (dataacl.Account, error) {
	if a, ok := s.accounts[strings.ToLower(name)]; ok {
		return a, nil
	}
	return dataacl.Account{}, fmt.Errorf("%w: %s", dataacl.ErrAccountUnknown, name)
}

func (s *fakeSys) ImporterCandidates() ([]string, error) {
	return s.candidates, nil
}

// fakeDB records what provisioning asked SQL Server to do. A login opens
// only with the password it was last given (sa's is saPass).
type fakeDB struct {
	mu         sync.Mutex
	saPass     string
	logins     map[string]string // least-privilege login → its password on the server
	procsMade  bool
	opened     []string // user names
	loginCalls int
	databases  []DatabaseFact
}

// login is the server's password for a least-privilege login, and whether
// the login exists.
func (f *fakeDB) login(name string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pw, ok := f.logins[name]
	return pw, ok
}

// setLogin puts a login on the server with password, as the hand script,
// another connector or a DBA would.
func (f *fakeDB) setLogin(name, password string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.logins == nil {
		f.logins = map[string]string{}
	}
	f.logins[name] = password
}

type fakeConn struct{ db *fakeDB }

func (f *fakeDB) open(cfg config.Config, password string) (Database, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, cfg.DB.User)
	switch {
	case strings.EqualFold(cfg.DB.User, "sa") && password == f.saPass:
	case f.logins[cfg.DB.User] != "" && password == f.logins[cfg.DB.User]:
	default:
		// As a driver would: the DSN, password included, in the error.
		return nil, errors.New("login failed for sqlserver://" + cfg.DB.User + ":" + password + "@" + cfg.DB.Host)
	}
	return fakeConn{f}, nil
}

func (c fakeConn) Ping(context.Context) error { return nil }
func (c fakeConn) Close() error               { return nil }
func (c fakeConn) EnsureProcedures(context.Context) ([]hasavshevet.ProcedureResult, error) {
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	made := !c.db.procsMade
	c.db.procsMade = true
	return []hasavshevet.ProcedureResult{{Name: "GPRICE_Bulk", Created: made}, {Name: "GetOnHandStockForSkus", Created: made}}, nil
}
func (c fakeConn) LoginExists(_ context.Context, login string) (bool, error) {
	_, ok := c.db.login(login)
	return ok, nil
}
func (c fakeConn) EnsureLeastPrivilegeLogin(_ context.Context, login, database, password string) error {
	if _, err := leastPrivilegeBatches(login, database, password); err != nil {
		return err
	}
	c.db.setLogin(login, password)
	c.db.mu.Lock()
	c.db.loginCalls++
	c.db.mu.Unlock()
	return nil
}
func (c fakeConn) CountDatabases(context.Context) (int, error) { return len(c.db.databases), nil }
func (c fakeConn) Fingerprint(_ context.Context, name string) (DatabaseFact, error) {
	for _, d := range c.db.databases {
		if d.Name == name {
			return d, nil
		}
	}
	return DatabaseFact{Name: name}, errors.New(name + " is not a database this login can open")
}

// testEnv wires a fake box. The clock advances by every Sleep, so a port
// proof that never succeeds ends at once.
func testEnv(sys *fakeSys, fdb *fakeDB) Env {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	return Env{
		Sys:    sys,
		OpenDB: fdb.open,
		HTTP:   sys.daemon.srv.Client(),
		LocalAddr: func(string) string {
			return strings.TrimPrefix(sys.daemon.srv.URL, "http://")
		},
		Now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		},
		Sleep: func(d time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			now = now.Add(d)
		},
		ProofTimeout: 5 * time.Second,
		LookupIP: func(_ context.Context, host string) ([]net.IP, error) {
			switch host {
			case "erp01.example.local":
				return []net.IP{net.ParseIP("10.0.0.5")}, nil
			case "sql.example.com":
				return []net.IP{net.ParseIP("10.0.0.6"), net.ParseIP("203.0.113.7")}, nil
			}
			return nil, errors.New("no such host")
		},
	}
}

// handInstallConfig is the pilot box as discovered (plan 068 §2.4):
// loopback listen, WIZSOFT's dynamic port as a number, sa, orders written
// with no importer.
func handInstallConfig(sendOrderDir string) config.Config {
	cfg := config.Default()
	cfg.ERP = config.ERPHasavshevet
	cfg.APIListen = "127.0.0.1:8080"
	cfg.DB.Host, cfg.DB.Port, cfg.DB.Database, cfg.DB.User = "127.0.0.1", 63501, "erp_db", "sa"
	cfg.SendOrderDir = sendOrderDir
	cfg.Auth = config.AuthConfig{Username: "digitrade", Password: "typed-in-the-gui-on-10-19", Secret: strings.Repeat("ab", 32)}
	return cfg
}

func writeConfig(t *testing.T, dir string, cfg config.Config) []byte {
	t.Helper()
	b, err := config.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, configFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return b
}

func readConfig(t *testing.T, dir string) (config.Config, []byte) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, configFile))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, b
}

const (
	testSaPass   = "sa-Pass-from-the-client"
	testAuthPass = "generated-by-digi-mcp-43-characters-long-xyz"
)

// handInstallSpec is what digi-mcp would send for the pilot box.
func handInstallSpec(sendOrderDir string) Spec {
	return Spec{
		Version:   1,
		ChangeSet: "cs-erp01-0001",
		ERP:       "hasavshevet",
		APIListen: "0.0.0.0:8082",
		DB: DBSpec{
			Host: `127.0.0.1\WIZSOFT`, Database: "erp_db",
			User: "sa", Password: testSaPass, LeastPrivilege: true,
		},
		SendOrderDir: &sendOrderDir,
		Importer:     &ImporterSpec{Mode: "none"},
		Auth:         AuthSpec{Username: "digi-hub", Password: testAuthPass},
		Firewall:     &FirewallSpec{AllowFrom: []string{"198.51.100.241"}},
		Service:      &ServiceSpec{DependOn: []string{"MSSQL$WIZSOFT"}},
	}
}

// handInstallBox is the pilot box before provisioning: its config, the sa password in the
// store, the daemon running on that config.
func handInstallBox(t *testing.T) (*fakeSys, *fakeDB, string) {
	t.Helper()
	d := newFakeDaemon(t)
	sys := newFakeSys(t, d)
	orders := filepath.Join(t.TempDir(), "digiorders")
	if err := os.MkdirAll(orders, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := handInstallConfig(orders)
	writeConfig(t, sys.dir, cfg)
	if err := sys.SecretSet(config.DBPasswordKey(cfg.ERP), []byte(testSaPass)); err != nil {
		t.Fatal(err)
	}
	sys.calls = nil
	d.set(func(d *fakeDaemon) { d.user, d.pass = cfg.Auth.Username, cfg.Auth.Password })
	return sys, &fakeDB{saPass: testSaPass}, orders
}

func (s *fakeSys) FolderRules() FolderRules { return s.folderRules }

// FinalPath is the test's final path for dir (finalPath), else \\?\<dir> for a folder that exists.
func (s *fakeSys) FinalPath(dir string) (string, error) {
	if f, ok := s.finalPath[dir]; ok {
		return f, nil
	}
	if _, err := os.Stat(dir); err != nil {
		return "", err
	}
	return `\\?\` + dir, nil
}

// ReparsePointOn is the test's answer for path, else whether path itself is
// a link: the test's temp folder above it is the machine's (on macOS /var is
// a symbolic link).
func (s *fakeSys) ReparsePointOn(path string) (string, error) {
	if at, ok := s.reparse[path]; ok {
		return at, nil
	}
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return path, nil
	}
	return "", nil
}
