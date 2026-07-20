package api

import (
	"database/sql"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/api/handlers"
	"github.com/digitrade-e/digi-erp-connector/internal/api/middleware"
	"github.com/digitrade-e/digi-erp-connector/internal/api/utils"
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

	mux := http.NewServeMux()
	limiter := middleware.NewRateLimiter(rateLimitPerSecond, rateLimitBurst)
	withAuth := func(h http.Handler) http.Handler {
		return middleware.Auth(token, h)
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
	sendOrderHandler := handlers.NewSendOrderHandler(deps.SendOrderQueue)
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
	utils.WriteError(w, http.StatusNotFound, "Not found", "NOT_FOUND", nil)
}
