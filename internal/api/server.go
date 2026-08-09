package api

import (
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/api/handlers"
	"github.com/digitrade-e/digi-erp-connector/internal/api/middleware"
	"github.com/digitrade-e/digi-erp-connector/internal/api/respond"
	"github.com/digitrade-e/digi-erp-connector/internal/auth"
	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
	"github.com/digitrade-e/digi-erp-connector/internal/logger"
	"github.com/digitrade-e/digi-erp-connector/internal/queries"
)

const (
	// Rate limiting protects the SQL and file endpoints from runaway callers.
	// Generous limits: the API serves a single trusted backend on localhost.
	rateLimitPerSecond = 25
	rateLimitBurst     = 50
)

type ServerDeps struct {
	DBPassword     string
	DB             *sql.DB
	Logger         logger.LoggerService
	SendOrderQueue *hasavshevet.OrderQueue
	QueryStore     *queries.Store
}

func NewServer(cfg config.Config, deps ServerDeps) (*http.Server, error) {
	addr := strings.TrimSpace(cfg.APIListen)
	if err := validateListenAddr(addr); err != nil {
		return nil, err
	}

	token := strings.TrimSpace(cfg.BearerToken)
	if token == "" {
		return nil, errors.New("bearerToken is required")
	}

	if deps.QueryStore == nil {
		return nil, errors.New("query store is required")
	}

	// The credential exchange (POST /auth/token). Optional, and when enabled the
	// tokens it issues are accepted alongside the static bearer token.
	//
	// Refuse to start on a half-configured block rather than exposing an exchange
	// that accepts blank credentials — the same reasoning as the TLS pair below.
	var signer *auth.Signer
	if cfg.Auth.Enabled {
		if err := cfg.Auth.Validate(); err != nil {
			return nil, err
		}
		if strings.TrimSpace(cfg.Auth.Secret) == "" {
			return nil, errors.New("auth.enabled requires auth.secret (the daemon generates one on first run; " +
				"if you see this, generation failed or the value was cleared by hand)")
		}
		s, err := auth.NewSigner(cfg.Auth.Secret)
		if err != nil {
			return nil, err
		}
		signer = s
	}

	mux := http.NewServeMux()
	limiter := middleware.NewRateLimiter(rateLimitPerSecond, rateLimitBurst)
	var verifyIssued middleware.TokenVerifier
	if signer != nil {
		verifyIssued = func(credential string) error {
			_, err := signer.Verify(credential)
			return err
		}
	}
	withAuth := func(h http.Handler) http.Handler {
		return middleware.AuthWithExchange(token, verifyIssued, h)
	}
	withLog := func(h http.Handler) http.Handler {
		return middleware.Logging(deps.Logger, cfg.Debug, h)
	}
	wrap := func(h http.Handler) http.Handler {
		return withLog(limiter.Middleware(withAuth(h)))
	}

	queryTimeout := time.Duration(cfg.Queries.TimeoutSeconds) * time.Second
	queryRunner := queries.NewRunner(deps.DB, queryTimeout, cfg.Queries.MaxRows)

	healthHandler := handlers.NewHealthHandler(cfg, deps.DBPassword)
	priceStockHandler := handlers.NewPriceAndStockHandler(cfg, deps.DB)
	folderFilesHandler := handlers.NewListFolderFilesHandler(cfg.ImageFolders)
	fileHandler := handlers.NewFileHandler(cfg.ImageFolders)
	// Whether this installation can process orders is derived from the config
	// rather than being a separate switch, so the two can never disagree.
	ordersConfigured := cfg.ERP == config.ERPHasavshevet && strings.TrimSpace(cfg.SendOrderDir) != ""
	sendOrderHandler := handlers.NewSendOrderHandler(deps.SendOrderQueue, ordersConfigured)
	sendOrderStatusHandler := handlers.NewSendOrderStatusHandler(deps.SendOrderQueue)
	createQueryHandler := handlers.NewCreateCustomSQLHandler(deps.QueryStore)
	listQueryHandler := handlers.NewListCustomSQLHandler(deps.QueryStore)
	getQueryHandler := handlers.NewGetCustomSQLHandler(deps.QueryStore)
	updateQueryHandler := handlers.NewUpdateCustomSQLHandler(deps.QueryStore)
	deleteQueryHandler := handlers.NewDeleteCustomSQLHandler(deps.QueryStore)
	runQueryHandler := handlers.NewRunSavedQueryHandler(deps.QueryStore, queryRunner)

	mux.Handle("GET /api/health", wrap(healthHandler))

	// "Is the service up and is my credential good" — deliberately no database
	// touch, unlike /api/health. Always registered: an operator checking a
	// connection needs it whichever credential they use.
	mux.Handle("GET /api/ping", wrap(handlers.NewPingHandler()))

	if signer != nil {
		// The credential exchange cannot itself require a credential. It is
		// still logged and rate-limited.
		mux.Handle("POST /auth/token", withLog(limiter.Middleware(
			handlers.NewTokenHandler(cfg.Auth, signer, deps.Logger))))
	}

	// Saved queries: the only SQL entry point. The backend registers queries
	// via CRUD and executes them by name — raw SQL is never accepted.
	mux.Handle("POST /api/custom_sql", wrap(createQueryHandler))
	mux.Handle("POST /api/create_custom_sql", wrap(createQueryHandler)) // legacy electron-mssql-app route
	mux.Handle("GET /api/custom_sql", wrap(listQueryHandler))
	mux.Handle("GET /api/custom_sql/{name}", wrap(getQueryHandler))
	mux.Handle("PATCH /api/custom_sql/{name}", wrap(updateQueryHandler))
	mux.Handle("DELETE /api/custom_sql/{name}", wrap(deleteQueryHandler))
	mux.Handle("GET /api/sqlqueries/{name}", wrap(runQueryHandler))

	mux.Handle("GET /api/folders/list", wrap(folderFilesHandler))
	mux.Handle("POST /api/file", wrap(fileHandler))
	mux.Handle("POST /api/sendOrder", wrap(sendOrderHandler))
	mux.Handle("GET /api/sendOrder/{jobId}", wrap(sendOrderStatusHandler))
	mux.Handle("POST /api/priceAndStockHandler", wrap(priceStockHandler))

	mux.Handle("/api/", wrap(http.HandlerFunc(NotFound)))

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	tlsCfg, err := buildTLSConfig(cfg.TLS)
	if err != nil {
		return nil, err
	}
	srv.TLSConfig = tlsCfg

	if deps.Logger != nil && tlsCfg == nil && !isLoopback(addr) {
		deps.Logger.Warn("serving plaintext HTTP on a non-loopback address (" + addr +
			"): the bearer token crosses the network in the clear. Configure tls.certFile/tls.keyFile.")
	}

	return srv, nil
}

// buildTLSConfig validates the certificate pair and returns the server's TLS
// settings, or nil when TLS is not configured.
//
// A half-configured or unloadable pair is a startup error rather than a warning:
// falling back to plaintext when the operator believes they configured HTTPS is
// exactly the failure that leaks a credential.
func buildTLSConfig(cfg config.TLSConfig) (*tls.Config, error) {
	if !cfg.Enabled() {
		return nil, nil
	}

	certFile := strings.TrimSpace(cfg.CertFile)
	keyFile := strings.TrimSpace(cfg.KeyFile)
	if certFile == "" || keyFile == "" {
		return nil, errors.New("tls requires both certFile and keyFile")
	}

	// Load it now so a bad path or mismatched pair fails at startup, not on the
	// first request.
	if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		return nil, fmt.Errorf("tls: load certificate: %w", err)
	}

	return &tls.Config{MinVersion: tls.VersionTLS12}, nil
}

// isLoopback reports whether the listen address is limited to this machine, in
// which case plaintext is not exposed to the network.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(host, "localhost")
}

func validateListenAddr(addr string) error {
	if addr == "" {
		return errors.New("apiListen is required")
	}

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("apiListen must be in host:port format")
	}
	if host == "" {
		return errors.New("apiListen host is required")
	}

	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("apiListen port is invalid")
	}

	return nil
}

func NotFound(w http.ResponseWriter, r *http.Request) {
	respond.Error(w, http.StatusNotFound, "Not found", "NOT_FOUND", nil)
}
