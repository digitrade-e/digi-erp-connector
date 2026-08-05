package db

import (
	"context"
	"database/sql"
	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"errors"
	"fmt"
	"net/url"
	"time"
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

func Open(cfg config.Config, password string, opt Options) (*sql.DB, error) {
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

	if opt.PingTimeout <= 0 {
		opt.PingTimeout = 5 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), opt.PingTimeout)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}

	return db, nil
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
