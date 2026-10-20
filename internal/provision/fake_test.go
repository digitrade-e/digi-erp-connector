package provision

import (
	"context"
	"encoding/json"
	"errors"
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
	srv          *httptest.Server
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
		_ = json.NewEncoder(w).Encode(map[string]int{"queued": d.queued, "running": d.running})
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
	daemon     *fakeDaemon
	// onStart runs when the service starts, after the daemon reloaded.
	onStart func(cfg config.Config)
	calls   []string
}

func newFakeSys(t *testing.T, d *fakeDaemon) *fakeSys {
	return &fakeSys{
		dir:    t.TempDir(),
		svc:    autostart.ServiceInfo{Installed: true, State: "running", StartType: "auto", Account: "LocalSystem", ProcessStart: time.Now().Add(-time.Hour), ProcessID: 4242},
		rules:  map[string]FirewallRule{},
		host:   HostInfo{Name: "TSDC", Domain: "groopyltd.local", PartOfDomain: true, DomainRole: 3, Elevated: true},
		daemon: d,
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
			return autostart.ServiceInfo{Installed: true, State: "running", ProcessID: 900}, nil
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

func (s *fakeSys) SetFirewallRule(r FirewallRule) error {
	s.call("rule:" + r.Name)
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
func (s *fakeSys) ImporterCandidates() ([]string, error) {
	return s.candidates, nil
}

// fakeDB records what provisioning asked SQL Server to do. A login opens
// only with the password it was last given (sa's is saPass).
type fakeDB struct {
	mu         sync.Mutex
	saPass     string
	loginPass  string // erp_connector_read's, once created
	procsMade  bool
	opened     []string // user names
	loginCalls int
	databases  []DatabaseFact
}

type fakeConn struct{ db *fakeDB }

func (f *fakeDB) open(cfg config.Config, password string) (Database, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, cfg.DB.User)
	switch {
	case strings.EqualFold(cfg.DB.User, "sa") && password == f.saPass:
	case cfg.DB.User == LeastPrivilegeLogin && f.loginPass != "" && password == f.loginPass:
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
func (c fakeConn) EnsureLeastPrivilegeLogin(_ context.Context, database, password string) error {
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	if _, err := leastPrivilegeBatches(database, password); err != nil {
		return err
	}
	c.db.loginPass = password
	c.db.loginCalls++
	return nil
}
func (c fakeConn) Databases(context.Context) ([]DatabaseFact, error) { return c.db.databases, nil }

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
	}
}

// groopyConfig is the GROOPY box as discovered on 2026-10-20 (plan 068 §2.4):
// loopback listen, WIZSOFT's dynamic port as a number, sa, orders written
// with no importer.
func groopyConfig(sendOrderDir string) config.Config {
	cfg := config.Default()
	cfg.ERP = config.ERPHasavshevet
	cfg.APIListen = "127.0.0.1:8080"
	cfg.DB.Host, cfg.DB.Port, cfg.DB.Database, cfg.DB.User = "127.0.0.1", 63501, "groopy_new", "sa"
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

// groopySpec is what digi-mcp would send for GROOPY.
func groopySpec(sendOrderDir string) Spec {
	return Spec{
		Version:   1,
		ChangeSet: "cs-groopy-0001",
		ERP:       "hasavshevet",
		APIListen: "0.0.0.0:8082",
		DB: DBSpec{
			Host: `127.0.0.1\WIZSOFT`, Database: "groopy_new",
			User: "sa", Password: testSaPass, LeastPrivilege: true,
		},
		SendOrderDir: sendOrderDir,
		Importer:     &ImporterSpec{Mode: "none"},
		Auth:         AuthSpec{Username: "digi-hub", Password: testAuthPass},
		Firewall:     &FirewallSpec{AllowFrom: []string{"34.154.127.241"}},
		Service:      &ServiceSpec{DependOn: []string{"MSSQL$WIZSOFT"}},
	}
}

// groopyBox is GROOPY before provisioning: its config, the sa password in the
// store, the daemon running on that config.
func groopyBox(t *testing.T) (*fakeSys, *fakeDB, string) {
	t.Helper()
	d := newFakeDaemon(t)
	sys := newFakeSys(t, d)
	orders := filepath.Join(t.TempDir(), "digiorders")
	if err := os.MkdirAll(orders, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := groopyConfig(orders)
	writeConfig(t, sys.dir, cfg)
	if err := sys.SecretSet(config.DBPasswordKey(cfg.ERP), []byte(testSaPass)); err != nil {
		t.Fatal(err)
	}
	sys.calls = nil
	d.set(func(d *fakeDaemon) { d.user, d.pass = cfg.Auth.Username, cfg.Auth.Password })
	return sys, &fakeDB{saPass: testSaPass}, orders
}
