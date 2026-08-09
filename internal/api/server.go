package api

import (
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
	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
	"github.com/digitrade-e/digi-erp-connector/internal/legacyauth"
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

	// Legacy electron-mssql-app compatibility: when enabled, the old JWT
	// exchange is accepted alongside the static bearer token and the old routes
	// are registered. Refuse to start on a half-configured compat block rather
	// than silently exposing /auth/token with empty credentials.
	var legacySigner *legacyauth.Signer
	if cfg.LegacyCompat.Enabled {
		if cfg.LegacyCompat.JWTSecret == "" || cfg.LegacyCompat.JWTUser == "" || cfg.LegacyCompat.JWTPassword == "" {
			return nil, errors.New("legacyCompat.enabled requires jwtSecret, jwtUser and jwtPassword")
		}
		signer, err := legacyauth.NewSigner(cfg.LegacyCompat.JWTSecret)
		if err != nil {
			return nil, err
		}
		legacySigner = signer
	}

	mux := http.NewServeMux()
	limiter := middleware.NewRateLimiter(rateLimitPerSecond, rateLimitBurst)
	var verifyLegacy middleware.LegacyTokenVerifier
	if legacySigner != nil {
		verifyLegacy = func(credential string) error {
			_, err := legacySigner.Verify(credential)
			return err
		}
	}
	withAuth := func(h http.Handler) http.Handler {
		return middleware.AuthWithLegacy(token, verifyLegacy, h)
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

	// electron-mssql-app compatibility surface (cutover only, config-gated).
	if legacySigner != nil {
		// The credential exchange itself cannot require a credential; it is
		// still logged and rate-limited.
		mux.Handle("POST /auth/token", withLog(limiter.Middleware(
			handlers.NewLegacyTokenHandler(cfg.LegacyCompat, legacySigner, deps.Logger))))

		mux.Handle("GET /api/ping", wrap(handlers.NewLegacyPingHandler(deps.Logger)))
		mux.Handle("POST /api/test-connection", wrap(
			handlers.NewLegacyTestConnectionHandler(cfg, deps.DBPassword, deps.Logger)))
		mux.Handle("GET /api/customers", wrap(handlers.NewLegacyCustomersHandler(queryRunner, deps.Logger)))
		mux.Handle("GET /api/orders/{id}", wrap(handlers.NewLegacyOrderHandler(queryRunner, deps.Logger)))

		if cfg.LegacyCompat.AllowRawSQL {
			mux.Handle("POST /api/query", wrap(handlers.NewLegacyQueryHandler(queryRunner, deps.Logger)))
		}

		if deps.Logger != nil {
			deps.Logger.Warn(fmt.Sprintf(
				"legacy electron-mssql-app compatibility is ENABLED (rawSQL=%v): /auth/token JWT exchange, /api/ping, "+
					"/api/test-connection, /api/customers, /api/orders/{id} are exposed. This is a cutover aid — "+
					"migrate the backend to the static bearer token + saved queries and set legacyCompat.enabled=false.",
				cfg.LegacyCompat.AllowRawSQL))
		}
	}

	mux.Handle("/api/", wrap(http.HandlerFunc(NotFound)))

	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
	}, nil
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
