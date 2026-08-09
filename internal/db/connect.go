package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
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
func IsConfigured(cfg config.Config) bool {
	return strings.TrimSpace(cfg.DB.Host) != "" &&
		strings.TrimSpace(cfg.DB.User) != "" &&
		cfg.DB.Port > 0
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
		return nil, err
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

func buildDSN(cfg config.Config, password string) (driverName string, dsn string, err error) {
	host := cfg.DB.Host
	port := cfg.DB.Port
	user := cfg.DB.User
	dbName := cfg.DB.Database

	if host == "" {
		return "", "", errors.New("db.host is required")
	}
	if port <= 0 || port > 65535 {
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
			Host:   fmt.Sprintf("%s:%d", host, port),
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
