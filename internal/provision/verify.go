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
		if dep := sqlDependency(cfg); dep != "" {
			add("dependency", slices.ContainsFunc(svc.Dependencies, func(d string) bool { return strings.EqualFold(d, dep) }),
				"depends on %s", dep)
		}
	}

	host, _, _ := net.SplitHostPort(cfg.APIListen)
	ip := net.ParseIP(host)
	add("listen", ip == nil || !ip.IsLoopback(), "%s, tls %v", cfg.APIListen, cfg.TLS.Enabled())

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
		status, err := d.get(ctx, "/api/health", tok, nil)
		add("database", err == nil && status == http.StatusOK, "/api/health %d", status)
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

// sqlDependency is the SQL Server service a connector whose database is on
// this machine should depend on ("" when the database is elsewhere).
func sqlDependency(cfg config.Config) string {
	server, instance := db.SplitInstance(cfg.DB.Host)
	if !isLocalHost(server) {
		return ""
	}
	if instance == "" {
		return "MSSQLSERVER"
	}
	return "MSSQL$" + strings.ToUpper(instance)
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
