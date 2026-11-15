package provision

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/db"
)

// VerifyInput is what the caller expects beyond a working connector.
type VerifyInput struct {
	// ExpectVersion, when set, is the version /api/ping must report.
	ExpectVersion string `json:"expectVersion,omitempty"`
	// FirewallRule names the rule that must allow apiListen's port. Empty:
	// DefaultFirewallRule is checked if it exists, and its absence is no
	// failure (the spec may not have asked for one).
	FirewallRule string `json:"firewallRule,omitempty"`
	// DB {"mode":"none"} says the box has no database (plan 086 C6): the
	// database check then passes when config.yaml names none (its
	// /api/health 503 is by design) and fails when it names one.
	DB *VerifyDB `json:"db,omitempty"`
}

// VerifyDB is what verify expects of the database: only {"mode":"none"}.
type VerifyDB struct {
	Mode string `json:"mode"`
}

// VerifyResult is every check and whether all passed.
type VerifyResult struct {
	OK     bool    `json:"ok"`
	Checks []Check `json:"checks"`
}

// Check is one thing Verify looked at.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Verify checks the running connector against config.yaml: the service, a
// login with the configured credential, /api/ping, /api/health, the order
// queue, the service's recovery and SQL dependency, the firewall rule. It
// writes nothing.
func Verify(ctx context.Context, env Env, in VerifyInput) (*VerifyResult, error) {
	res := &VerifyResult{Checks: []Check{}}
	if in.DB != nil && in.DB.Mode != DBModeNone {
		return res, refuse(CodeSpecInvalid, `send "db": {"mode": "none"} for a box without a database, or leave db out`, "db.mode %q is not known", in.DB.Mode)
	}
	dbless := in.DB != nil
	add := func(name string, ok bool, format string, args ...any) {
		res.Checks = append(res.Checks, Check{Name: name, OK: ok, Detail: fmt.Sprintf(format, args...)})
	}
	b, err := os.ReadFile(filepath.Join(env.Sys.DataDir(), configFile))
	if err != nil {
		return res, refuse(CodeConfigRefused, "apply a spec first", "config.yaml cannot be read: %v", err)
	}
	cfg, err := config.Parse(b)
	if err != nil {
		return res, refuse(CodeConfigRefused, "fix config.yaml", "config.yaml does not parse: %v", err)
	}

	svc, err := env.Sys.Service(ServiceName)
	switch {
	case err != nil:
		add("service", false, "%v", err)
	case !svc.Installed:
		add("service", false, "not installed")
	default:
		add("service", svc.State == "running", "%s, %s, as %s", svc.State, svc.StartType, svc.Account)
		add("recovery", svc.RecoveryPlanned, "restart after 1, 5 and 30 min, also on non-crash failures")
		instances, _ := env.Sys.SQLInstances() // unreadable: the dependency is named from db.host alone
		if dep := sqlDependencyFor(cfg, instances); dep != "" {
			add("dependency", slices.ContainsFunc(svc.Dependencies, func(d string) bool { return strings.EqualFold(d, dep) }),
				"depends on %s", dep)
		}
		// A named instance on this machine reached without a port: the
		// Browser answers for it, after every reboot too.
		if server, instance := db.SplitInstance(cfg.DB.Host); isLocalHost(server) && instance != "" && cfg.DB.Port == 0 {
			b, err := env.Sys.Service("SQLBrowser")
			add("browser", err == nil && b.Installed && b.State == "running" && b.StartType == "auto",
				"SQL Server Browser: %s, starts %s", orUnknown(b.State), orUnknown(b.StartType))
		}
	}

	// Behind the link loopback is the rule (linkmode.go), elsewhere a defect:
	// the Hub cannot reach it.
	host, _, _ := net.SplitHostPort(cfg.APIListen)
	ip := net.ParseIP(host)
	add("listen", ip == nil || !ip.IsLoopback() || LinkMode(env.Sys.DataDir()), "%s, tls %v", cfg.APIListen, cfg.TLS.Enabled())

	d := env.daemonFor(cfg)
	tok, err := d.login(ctx, cfg.Auth.Username, cfg.Auth.Password)
	if err != nil {
		add("login", false, "%v", err)
	} else {
		add("login", true, "")
		p, err := d.ping(ctx, tok)
		switch {
		case err != nil:
			add("ping", false, "%v", err)
		case in.ExpectVersion != "" && p.Version != in.ExpectVersion:
			add("ping", false, "version %q, expected %q", p.Version, in.ExpectVersion)
		default:
			add("ping", true, "version %s", p.Version)
		}
		switch {
		case dbless && db.IsConfigured(cfg):
			add("database", false, "config.yaml names a database (%s), and none was expected", cfg.DB.Host)
		case dbless:
			add("database", true, "no database, as expected: /api/health not asked")
		case !db.IsConfigured(cfg):
			add("database", false, `config.yaml names no database (send "db": {"mode": "none"} to verify a box without one)`)
		default:
			status, err := d.get(ctx, "/api/health", tok, nil)
			add("database", err == nil && status == http.StatusOK, "/api/health %d", status)
		}
		queued, running, err := d.queue(ctx, tok)
		if errors.Is(err, errNoQueueRoute) {
			add("orders", true, "this connector cannot report its queue")
		} else {
			add("orders", err == nil, "%d queued, %d being written", queued, running)
		}
	}

	name := in.FirewallRule
	if name == "" {
		name = DefaultFirewallRule
	}
	rule, err := env.Sys.FirewallRule(name)
	switch {
	case err != nil:
		add("firewall", false, "%v", err)
	case rule == nil:
		add("firewall", in.FirewallRule == "", "no rule named %q", name)
	default:
		port := listenPort(cfg.APIListen)
		add("firewall", rule.Enabled && rule.Port == port,
			"%q: TCP %d from %s", name, rule.Port, strings.Join(rule.RemoteAddress, ", "))
	}

	res.OK = true
	for _, c := range res.Checks {
		res.OK = res.OK && c.OK
	}
	return res, nil
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// sqlDependency is the SQL Server service a connector whose database is on
// this machine should depend on ("" when the database is elsewhere), from
// db.host alone.
func sqlDependency(cfg config.Config) string { return sqlDependencyFor(cfg, nil) }

// sqlDependencyFor is sqlDependency with the machine's instances: a database
// reached by port (127.0.0.1:63501) belongs to the instance that has that
// port, which is a named one on BFL (MSSQL$WIZSOFT2017), not MSSQLSERVER
// (plan 083 §19). host\instance names its instance; a port no instance has is
// the default instance's, as before.
func sqlDependencyFor(cfg config.Config, instances []SQLInstance) string {
	server, instance := db.SplitInstance(cfg.DB.Host)
	if !isLocalHost(server) {
		return ""
	}
	if instance != "" {
		return "MSSQL$" + strings.ToUpper(instance)
	}
	if p := cfg.DB.Port; p > 0 {
		for _, in := range instances {
			if in.Service != "" && (in.StaticPort == p || in.DynamicPort == p || slices.Contains(in.ListeningPorts, p)) {
				return strings.ToUpper(in.Service)
			}
		}
	}
	return "MSSQLSERVER"
}

func isLocalHost(h string) bool {
	switch strings.ToLower(h) {
	case "localhost", ".", "(local)", "127.0.0.1", "::1":
		return true
	}
	if name, err := os.Hostname(); err == nil && strings.EqualFold(h, name) {
		return true
	}
	return false
}
