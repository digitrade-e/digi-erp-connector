package db

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/microsoft/go-mssqldb/msdsn"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

func baseConfig() config.Config {
	cfg := config.Default()
	cfg.DB.Driver = config.DBDriverMSSQL
	cfg.DB.Host = "localhost"
	cfg.DB.Port = 1433
	cfg.DB.User = "sa"
	cfg.DB.Database = "BFL"
	return cfg
}

func TestBuildDSN(t *testing.T) {
	driver, dsn, err := buildDSN(baseConfig(), "p@ss word")
	if err != nil {
		t.Fatalf("buildDSN: %v", err)
	}
	if driver != "sqlserver" {
		t.Errorf("driver = %q, want sqlserver", driver)
	}

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("DSN is not a valid URL: %v", err)
	}
	if u.Scheme != "sqlserver" {
		t.Errorf("scheme = %q", u.Scheme)
	}
	if u.Host != "localhost:1433" {
		t.Errorf("host = %q, want localhost:1433", u.Host)
	}
	if user := u.User.Username(); user != "sa" {
		t.Errorf("user = %q, want sa", user)
	}
	// A password with a space and an @ must survive URL encoding intact,
	// otherwise the login silently fails with a confusing driver error.
	if pass, _ := u.User.Password(); pass != "p@ss word" {
		t.Errorf("password round-trip = %q, want %q", pass, "p@ss word")
	}
	if got := u.Query().Get("database"); got != "BFL" {
		t.Errorf("database = %q, want BFL", got)
	}
}

// The TLS options exist because electron-mssql-app connected with
// encrypt:true + trustServerCertificate:true against a local self-signed
// certificate. They must be omitted entirely when off, so existing
// installations keep whatever driver default they were commissioned with.
func TestBuildDSNTLSOptions(t *testing.T) {
	tests := []struct {
		name        string
		encrypt     bool
		trust       bool
		wantEncrypt string
		wantTrust   string
		wantAbsent  []string
	}{
		{
			name:       "both off omits both options",
			wantAbsent: []string{"encrypt", "TrustServerCertificate"},
		},
		{
			name:        "encrypt only",
			encrypt:     true,
			wantEncrypt: "true",
			wantAbsent:  []string{"TrustServerCertificate"},
		},
		{
			name:        "both on (the production setting)",
			encrypt:     true,
			trust:       true,
			wantEncrypt: "true",
			wantTrust:   "true",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.DB.Encrypt = tc.encrypt
			cfg.DB.TrustServerCertificate = tc.trust

			_, dsn, err := buildDSN(cfg, "pw")
			if err != nil {
				t.Fatalf("buildDSN: %v", err)
			}
			q, err := url.Parse(dsn)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			params := q.Query()

			if got := params.Get("encrypt"); got != tc.wantEncrypt {
				t.Errorf("encrypt = %q, want %q", got, tc.wantEncrypt)
			}
			if got := params.Get("TrustServerCertificate"); got != tc.wantTrust {
				t.Errorf("TrustServerCertificate = %q, want %q", got, tc.wantTrust)
			}
			for _, key := range tc.wantAbsent {
				if _, present := params[key]; present {
					t.Errorf("%s should be absent from the DSN, got %q", key, params.Get(key))
				}
			}
		})
	}
}

func TestBuildDSNValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*config.Config)
		wantErr string
	}{
		{"no host", func(c *config.Config) { c.DB.Host = "" }, "db.host is required"},
		{"no user", func(c *config.Config) { c.DB.User = "" }, "db.user is required"},
		{"port zero", func(c *config.Config) { c.DB.Port = 0 }, "db.port is invalid"},
		{"port negative", func(c *config.Config) { c.DB.Port = -1 }, "db.port is invalid"},
		{"port too large", func(c *config.Config) { c.DB.Port = 70000 }, "db.port is invalid"},
		{"unknown driver", func(c *config.Config) { c.DB.Driver = "postgres" }, "unsupported driver"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			tc.mutate(&cfg)

			_, _, err := buildDSN(cfg, "pw")
			if err == nil {
				t.Fatalf("expected an error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// db.host "host\instance" names a SQL Server instance (the docs have offered
// it since the start). The DSN used to carry it in the host part, escaped to
// %5C, and the driver refused it — so this reads every DSN back with the
// driver's own parser, not a string comparison.
func TestBuildDSNNamedInstance(t *testing.T) {
	tests := []struct {
		name         string
		host         string
		port         int
		wantHost     string
		wantInstance string
		wantPort     uint64
	}{
		{"instance, no port: the Browser resolves it", `127.0.0.1\WIZSOFT`, 0, "127.0.0.1", "WIZSOFT", 0},
		{"instance and a port: connect there directly", `127.0.0.1\WIZSOFT`, 63501, "127.0.0.1", "WIZSOFT", 63501},
		{"spaces around either part are dropped", ` sql01 \ ERP `, 0, "sql01", "ERP", 0},
		{"a default instance is unchanged", "localhost", 1433, "localhost", "", 1433},
		{"an IPv6 literal gets brackets", "::1", 1433, "::1", "", 1433},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.DB.Host = tc.host
			cfg.DB.Port = tc.port

			_, dsn, err := buildDSN(cfg, "p@ss word")
			if err != nil {
				t.Fatalf("buildDSN: %v", err)
			}
			got, err := msdsn.Parse(dsn)
			if err != nil {
				t.Fatalf("the driver refuses %q: %v", dsn, err)
			}
			if got.Host != tc.wantHost || got.Instance != tc.wantInstance || got.Port != tc.wantPort {
				t.Errorf("driver reads host=%q instance=%q port=%d, want %q %q %d (dsn %q)",
					got.Host, got.Instance, got.Port, tc.wantHost, tc.wantInstance, tc.wantPort, dsn)
			}
			if got.Password != "p@ss word" || got.Database != "BFL" {
				t.Errorf("password or database lost: %q %q", got.Password, got.Database)
			}
		})
	}
}

func TestIsConfiguredNamedInstance(t *testing.T) {
	tests := []struct {
		name string
		host string
		port int
		want bool
	}{
		{"a named instance needs no port", `127.0.0.1\WIZSOFT`, 0, true},
		{"a default instance needs one", "127.0.0.1", 0, false},
		{"an instance with no server is not a host", `\WIZSOFT`, 0, false},
		{"host and port", "127.0.0.1", 1433, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.DB.Host = tc.host
			cfg.DB.Port = tc.port
			if got := IsConfigured(cfg); got != tc.want {
				t.Errorf("IsConfigured = %v, want %v", got, tc.want)
			}
		})
	}
}

// An empty database name is legitimate (connect to the login's default).
func TestBuildDSNOmitsEmptyDatabase(t *testing.T) {
	cfg := baseConfig()
	cfg.DB.Database = ""

	_, dsn, err := buildDSN(cfg, "pw")
	if err != nil {
		t.Fatalf("buildDSN: %v", err)
	}
	if strings.Contains(dsn, "database=") {
		t.Errorf("DSN should omit an empty database, got %q", dsn)
	}
}

// OpenLazy must not contact the server. A connector whose database lives on
// another host has to survive starting before that host is reachable — the
// daemon relies on this to keep serving (and answering 503) instead of exiting.
func TestOpenLazyDoesNotRequireAReachableServer(t *testing.T) {
	cfg := baseConfig()
	// Nothing listens here; port 1 is reserved and refuses immediately.
	cfg.DB.Host = "127.0.0.1"
	cfg.DB.Port = 1

	pool, err := OpenLazy(cfg, "pw", DefaultOptions())
	if err != nil {
		t.Fatalf("OpenLazy should succeed without a reachable server: %v", err)
	}
	if pool == nil {
		t.Fatal("OpenLazy returned a nil pool")
	}
	defer pool.Close()

	// The failure surfaces on use, not on construction.
	if err := Ping(context.Background(), pool, 2*time.Second); err == nil {
		t.Error("Ping should fail against a port with no listener")
	}
}

// A broken configuration is still fatal at construction: that is an operator
// error, not a transient network condition.
func TestOpenLazyStillRejectsBadConfig(t *testing.T) {
	cfg := baseConfig()
	cfg.DB.Host = ""

	if _, err := OpenLazy(cfg, "pw", DefaultOptions()); err == nil {
		t.Error("OpenLazy accepted a config with no host")
	}
}

func TestPingRejectsNilHandle(t *testing.T) {
	if err := Ping(context.Background(), nil, time.Second); err == nil {
		t.Error("Ping(nil) should return an error rather than panicking")
	}
}

// A db.host the driver's URL parser refuses is refused here instead: the
// driver would fail on connect, quoting the DSN — password URL-escaped — in
// the error the daemon logs and the GUI shows.
func TestBuildDSNRefusesAHostTheDriverCannotParse(t *testing.T) {
	for _, host := range []string{"my host", "a/b", "sa@evil", "host%20x", `\WIZSOFT`} {
		cfg := baseConfig()
		cfg.DB.Host = host
		_, err := OpenLazy(cfg, "P@ss word!x", DefaultOptions())
		if err == nil {
			t.Errorf("%q: accepted", host)
			continue
		}
		if msg := err.Error(); strings.Contains(msg, "word") || strings.Contains(msg, "P%40ss") {
			t.Errorf("%q: the error carries the password: %s", host, msg)
		}
	}
	for _, host := range []string{"localhost", ".", "(local)", `.\SQLEXPRESS`, "TSDC", "sql-01.corp.local", "my_host", "10.0.0.5", "::1", `127.0.0.1\WIZSOFT`} {
		cfg := baseConfig()
		cfg.DB.Host = host
		_, dsn, err := buildDSN(cfg, "pw")
		if err != nil {
			t.Errorf("%q: %v", host, err)
			continue
		}
		if _, err := msdsn.Parse(dsn); err != nil {
			t.Errorf("%q: the driver refuses %q: %v", host, dsn, err)
		}
	}
}
