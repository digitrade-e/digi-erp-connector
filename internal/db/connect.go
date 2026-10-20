package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

type Options struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	PingTimeout     time.Duration
}

func DefaultOptions() Options {
	return Options{
		MaxOpenConns:    10,
		MaxIdleConns:    10,
		ConnMaxLifetime: 30 * time.Minute,
		PingTimeout:     5 * time.Second,
	}
}

// IsConfigured reports whether a database has been set up at all.
//
// A connector deployed only to write ERP order files may legitimately have no
// database: the backend supplies the customer details with the order, and the
// connector never queries anything. Everything that touches the database checks
// this first so an unconfigured node degrades cleanly — endpoints answer 503
// instead of failing with a confusing driver error.
//
// A named instance (db.host "host\instance") counts without a port: the driver
// then asks the SQL Server Browser where the instance listens.
func IsConfigured(cfg config.Config) bool {
	server, instance := SplitInstance(cfg.DB.Host)
	return server != "" &&
		strings.TrimSpace(cfg.DB.User) != "" &&
		(cfg.DB.Port > 0 || instance != "")
}

// Check refuses a database configuration the connector cannot use, without
// contacting anything: what OpenLazy would refuse, which stops the daemon. A
// config with no database is fine. The GUI's save and api.CheckConfig call it,
// so neither leaves a config the service will not start on.
func Check(cfg config.Config) error {
	if !IsConfigured(cfg) {
		return nil
	}
	_, _, err := buildDSN(cfg, "")
	return err
}

// SplitInstance separates db.host into the server and a named instance:
// "127.0.0.1\WIZSOFT" is ("127.0.0.1", "WIZSOFT"), "localhost" is
// ("localhost", "").
func SplitInstance(host string) (server, instance string) {
	server, instance, _ = strings.Cut(strings.TrimSpace(host), `\`)
	return strings.TrimSpace(server), strings.TrimSpace(instance)
}

// Open returns a pool that is verified reachable: it pings the server and fails
// if the database cannot be contacted. Use it for interactive checks ("Test
// connection") where an immediate answer is the point.
//
// Long-running services should prefer OpenLazy — see the note there.
func Open(cfg config.Config, password string, opt Options) (*sql.DB, error) {
	db, err := OpenLazy(cfg, password, opt)
	if err != nil {
		return nil, err
	}

	if err := Ping(context.Background(), db, opt.PingTimeout); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// OpenLazy builds the pool without contacting the server. It fails only on a
// bad configuration (missing host, invalid port, unknown driver), never on
// connectivity.
//
// This is what the daemon uses. database/sql opens connections on demand and
// re-establishes them after a failure, so a database that is unreachable at
// startup — a service starting before SQL Server, or a database on another host
// across a flaky link — is a temporary condition rather than a fatal one. The
// API keeps serving and the DB-dependent endpoints fail closed with 503 until
// the connection succeeds.
func OpenLazy(cfg config.Config, password string, opt Options) (*sql.DB, error) {
	driverName, dsn, err := buildDSN(cfg, password)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open(driverName, dsn)
	if err != nil {
		// Not err: the driver's parse error quotes the DSN, password
		// included, and the daemon logs what this returns.
		return nil, errors.New("the db settings do not make a valid connection string (check db.host, db.port and db.user)")
	}

	if opt.MaxOpenConns > 0 {
		db.SetMaxOpenConns(opt.MaxOpenConns)
	}
	if opt.MaxIdleConns > 0 {
		db.SetMaxIdleConns(opt.MaxIdleConns)
	}
	if opt.ConnMaxLifetime > 0 {
		db.SetConnMaxLifetime(opt.ConnMaxLifetime)
	}

	return db, nil
}

// Ping verifies the pool can reach the server. timeout <= 0 uses the default.
func Ping(ctx context.Context, db *sql.DB, timeout time.Duration) error {
	if db == nil {
		return errors.New("no database handle")
	}
	if timeout <= 0 {
		timeout = DefaultOptions().PingTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return db.PingContext(ctx)
}

// hostRe is what db.host's server part may be besides an IP: a host name
// (NetBIOS names may carry _), "." or "(local)". Anything else makes the
// driver refuse the DSN — not at Open but on connect, with the DSN, password
// URL-escaped, in the error the daemon logs and the GUI shows.
var hostRe = regexp.MustCompile(`^([A-Za-z0-9]([A-Za-z0-9_.-]*[A-Za-z0-9])?|\.|\(local\))$`)

func validHost(h string) bool { return net.ParseIP(h) != nil || hostRe.MatchString(h) }

// buildDSN puts a named instance in the URL path (sqlserver://host/INSTANCE),
// the form go-mssqldb reads as host\INSTANCE. It used to go into the host
// part, where the backslash was escaped to %5C and the driver refused the DSN
// outright ("invalid URL escape") — so the host\instance form the docs offer
// never connected. With an instance and no port the driver asks the SQL Server
// Browser (UDP 1434) for the instance's port, which follows a dynamic port
// across restarts; with a port it connects there directly.
func buildDSN(cfg config.Config, password string) (driverName string, dsn string, err error) {
	host, instance := SplitInstance(cfg.DB.Host)
	port := cfg.DB.Port
	user := cfg.DB.User
	dbName := cfg.DB.Database

	if host == "" {
		return "", "", errors.New("db.host is required")
	}
	if !validHost(host) {
		return "", "", errors.New("db.host is not a host name or IP address")
	}
	if port < 0 || port > 65535 || (port == 0 && instance == "") {
		return "", "", errors.New("db.port is invalid")
	}
	if user == "" {
		return "", "", errors.New("db.user is required")
	}

	switch cfg.DB.Driver {
	case config.DBDriverMSSQL:
		u := &url.URL{
			Scheme: "sqlserver",
			User:   url.UserPassword(user, password),
			Host:   host,
		}
		if port > 0 {
			u.Host = net.JoinHostPort(host, strconv.Itoa(port))
		}
		if instance != "" {
			u.Path = "/" + instance
		}
		q := url.Values{}
		if dbName != "" {
			q.Set("database", dbName)
		}
		// Only emit the TLS options when explicitly enabled so existing
		// installs keep the driver defaults they were commissioned with.
		if cfg.DB.Encrypt {
			q.Set("encrypt", "true")
		}
		if cfg.DB.TrustServerCertificate {
			q.Set("TrustServerCertificate", "true")
		}
		u.RawQuery = q.Encode()

		return "sqlserver", u.String(), nil

	default:
		return "", "", fmt.Errorf("unsupported driver: %q", cfg.DB.Driver)
	}
}

func TestConnection(ctx context.Context, cfg config.Config, password string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	opt := DefaultOptions()
	db, err := Open(cfg, password, opt)
	if err != nil {
		return err
	}
	return db.Close()
}
