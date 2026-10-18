package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/api"
	"github.com/digitrade-e/digi-erp-connector/internal/auth"
	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/db"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
	"github.com/digitrade-e/digi-erp-connector/internal/logger"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/autostart"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/paths"
	"github.com/digitrade-e/digi-erp-connector/internal/queries"
	"github.com/digitrade-e/digi-erp-connector/internal/secrets"
)

const windowsServiceName = "digi-erp-connectord"

// orderDrainTimeout bounds how long Stop waits for queued orders to be
// written. It sits inside autostart.StopTimeout, which also covers the HTTP
// shutdown (a receipt being written can hold that up to 120 s).
const orderDrainTimeout = 120 * time.Second

type serverApp struct {
	cfg         config.Config
	logSvc      logger.LoggerService
	dbConn      *sql.DB
	srv         *http.Server
	errCh       chan error
	dbPassStr   string
	orderQueue  *hasavshevet.OrderQueue
	queueCancel context.CancelFunc
	// receiptWriter is nil when receipts are not configured. Stop closes it,
	// releasing the receipt data-directory lock for the next process.
	receiptWriter *hasavshevet.ReceiptWriter
	// startProgress, set by the service handler, reports a start checkpoint
	// (SERVICE_START_PENDING) while the data directory is walked.
	startProgress func()
}

// SetStartProgress is how the Windows service handler hands Start its
// checkpoint callback (autostart.StartProgressSetter).
func (a *serverApp) SetStartProgress(f func()) { a.startProgress = f }

// refuse ends a start whose data directory could not be created or
// protected (R3-1): no file log is opened in it — a junctioned or swapped
// root would take SYSTEM's writes elsewhere — so the reason and the early
// lines go to the Windows Event Log, and the service exits with
// autostart.ExitDataDirRefused.
func (a *serverApp) refuse(early *earlyLog, err error) error {
	reportRefusalFn(early.lines(), err)
	return &startRefused{err: err}
}

func (a *serverApp) Start() error {
	// Until the data directory is protected nothing in it is opened — not even
	// server.log: Start logs into a buffer and replays it into server.log once
	// that is safe (earlylog.go).
	early := newEarlyLog()
	early.Info(fmt.Sprintf("daemon Start() called: pid=%d goos=%s", os.Getpid(), runtime.GOOS))

	// Lock the data directory down first — before server.log is opened and
	// before the config is read, so also on a first install's start, which has
	// no config yet (Windows only; a no-op elsewhere). Fails closed.
	dataDir := paths.DataDir()
	if err := ensureDataDir(dataDir); err != nil {
		early.Error(fmt.Sprintf("could not create the data directory %s; not starting", dataDir), err)
		return a.refuse(early, err)
	}
	rep, err := protectDataDir(dataDir, a.startProgress, early)
	if err != nil {
		return a.refuse(early, err)
	}
	mark := freshServerLog(dataDir, rep, early)

	// Bootstrap logger writes directly to server.log so we capture pre-config
	// failures (config.Load errors, permissions, missing dirs) even when
	// running as a Windows service where stderr is unavailable. Its open
	// refuses a reparse point or a hard link (dataacl.OpenLog).
	bootstrapLog := newBootstrapLog()
	early.replayInto(bootstrapLog)
	if mark && !logger.IsStderr(bootstrapLog) {
		markServerLog(dataDir, bootstrapLog)
	}

	cfg, err := config.Load()
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			bootstrapLog.Error("config not found; run digi-erp-connector UI to create it", nil)
		} else {
			bootstrapLog.Error("failed to load config", err)
		}
		_ = bootstrapLog.Close()
		return err
	}
	a.cfg = cfg
	bootstrapLog.Info(fmt.Sprintf("config loaded: erp=%s apiListen=%s sendOrderDir=%q", cfg.ERP, cfg.APIListen, cfg.SendOrderDir))

	// config.yaml exists now: protect it too (logged only on failure).
	if cfgPath, pathErr := paths.ConfigFilePath(); pathErr == nil {
		protectConfigFile(cfgPath, bootstrapLog)
	}

	// Generate this installation's signing secret if it has none. Doing it here
	// rather than shipping a default is the whole point: a constant compiled into
	// the binary would let anyone with the source mint tokens accepted by every
	// deployment. Persisted so tokens survive a restart.
	if strings.TrimSpace(cfg.Auth.Secret) == "" {
		secret, secErr := auth.NewSecret()
		if secErr != nil {
			bootstrapLog.Error("could not generate an auth signing secret", secErr)
			return secErr
		}
		cfg.Auth.Secret = secret
		if saveErr := config.Save(cfg); saveErr != nil {
			bootstrapLog.Error("could not persist the generated auth signing secret", saveErr)
			return saveErr
		}
		a.cfg = cfg
		bootstrapLog.Info("generated this installation's auth signing secret and saved it to config.yaml")
	}

	bootstrapLog.Info("calling logger.New(cfg)")
	logSvc, err := logger.New(cfg)
	if err != nil {
		bootstrapLog.Error("logger init failed; using bootstrap logger", err)
		logSvc = bootstrapLog
	}
	a.logSvc = logSvc
	bootstrapLog.Info("logger.New(cfg) returned")

	logSvc.Info(fmt.Sprintf("calling secrets.Get for db password (key=%s)", dbPasswordKey(cfg.ERP)))
	dbPassword, dbPassErr := secrets.Get(dbPasswordKey(cfg.ERP))
	if dbPassErr != nil {
		logSvc.Error("failed to load db password", dbPassErr)
	}
	if dbPassErr == nil {
		a.dbPassStr = string(dbPassword)
		logSvc.Info(fmt.Sprintf("db password loaded (length=%d)", len(a.dbPassStr)))
	}

	logSvc.Info(fmt.Sprintf(
		"calling db.Open: driver=%s host=%s port=%d database=%s user=%s",
		cfg.DB.Driver, cfg.DB.Host, cfg.DB.Port, cfg.DB.Database, cfg.DB.User,
	))
	// Build the pool without contacting the server: an unreachable database at
	// startup must not stop the service. This matters most when the database is
	// on another host — the daemon would otherwise die on every network blip or
	// whenever it starts before SQL Server is ready — but it also removes a
	// whole class of "service won't stay started" incidents on a single box.
	//
	// Only a bad configuration is fatal here. Connectivity is retried by
	// database/sql on demand, and DB-dependent endpoints answer 503 meanwhile.
	var dbConn *sql.DB
	if db.IsConfigured(cfg) {
		dbConn, err = db.OpenLazy(cfg, a.dbPassStr, db.DefaultOptions())
		if err != nil {
			logSvc.Error("db configuration invalid", err)
			a.Stop(context.Background())
			return err
		}
		a.dbConn = dbConn

		if pingErr := db.Ping(context.Background(), dbConn, 0); pingErr != nil {
			logSvc.Warn(fmt.Sprintf(
				"database not reachable at startup (%v) — the API will start anyway and reconnect on demand; "+
					"database-backed endpoints return 503 until it succeeds", pingErr))
		} else {
			logSvc.Info("db connected")
		}
	} else {
		// A node that only writes order files needs no database of its own; the
		// backend supplies the customer details with the order. Saved queries,
		// price/stock and health answer 503 here.
		logSvc.Warn("no database configured (db.host/db.user/db.port unset) — " +
			"saved queries, price/stock and health will report 503. Orders require the " +
			"customer details to be supplied in the request.")
	}

	// Build the send-order queue for Hasavshevet.
	// Order number file lives next to IMOVEIN files for self-contained directory.
	numStorePath := filepath.Join(cfg.SendOrderDir, "lastOrderNumber.json")
	numStore := hasavshevet.NewOrderNumberStore(numStorePath)
	sender := hasavshevet.NewSender(dbConn, cfg, numStore, logSvc)

	queue := hasavshevet.NewOrderQueue(sender, logSvc)
	queueCtx, queueCancel := context.WithCancel(context.Background())
	queue.Start(queueCtx)
	a.orderQueue = queue
	a.queueCancel = queueCancel

	receiptWriter, err := buildReceiptWriter(cfg, paths.DataDir(), logSvc)
	if err != nil {
		logSvc.Error("receipt configuration", err)
		a.Stop(context.Background())
		return err
	}
	a.receiptWriter = receiptWriter

	queryStorePath := filepath.Join(paths.DataDir(), "queries.json")
	queryStore, err := queries.NewStore(queryStorePath)
	if err != nil {
		logSvc.Error("failed to load saved queries store", err)
		a.Stop(context.Background())
		return err
	}
	logSvc.Info(fmt.Sprintf("saved queries store loaded: path=%s count=%d", queryStorePath, queryStore.Count()))

	deps := api.ServerDeps{
		DBPassword:     a.dbPassStr,
		DB:             dbConn,
		Logger:         logSvc,
		SendOrderQueue: queue,
		QueryStore:     queryStore,
	}
	if receiptWriter != nil {
		deps.ReceiptWriter = receiptWriter // never a typed nil in the interface
	}
	srv, err := api.NewServer(cfg, deps)
	if err != nil {
		logSvc.Error("config validation error", err)
		a.Stop(context.Background())
		return err
	}
	a.srv = srv

	// NewServer has already validated the certificate pair, so reaching here with
	// TLSConfig set means the files load.
	serveTLS := srv.TLSConfig != nil
	scheme := "http"
	if serveTLS {
		scheme = "https"
	}

	a.errCh = make(chan error, 1)
	go func() {
		if serveTLS {
			// The paths are already in the server's TLS config; passing them
			// again is how net/http wants them.
			a.errCh <- srv.ListenAndServeTLS(cfg.TLS.CertFile, cfg.TLS.KeyFile)
			return
		}
		a.errCh <- srv.ListenAndServe()
	}()

	logSvc.Info(fmt.Sprintf("digi-erp-connectord listening on %s://%s", scheme, srv.Addr))
	return nil
}

// Stop shuts down in the order that loses nothing:
//
//  1. the HTTP server stops accepting and waits for requests in flight — a
//     receipt being written, an order being accepted;
//  2. the order queue is closed and the worker finishes every order already
//     queued (each was answered 202), for up to orderDrainTimeout;
//  3. only then is the worker's context cancelled;
//  4. the receipt writer is closed, releasing the receipt data-directory lock
//     for the next connector process — waiting for the import lock no longer
//     than ctx allows (a hung order import holds it; the OS then releases the
//     data lock at exit).
//
// It used to cancel the queue first, which dropped every queued order.
// ctx bounds the whole stop (autostart.StopTimeout from the service).
func (a *serverApp) Stop(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	if a.srv != nil {
		if err := a.srv.Shutdown(ctx); err != nil && a.logSvc != nil {
			a.logSvc.Error("HTTP shutdown did not finish cleanly", err)
		}
	}
	if a.orderQueue != nil {
		drainCtx, cancel := context.WithTimeout(ctx, orderDrainTimeout)
		if err := a.orderQueue.Drain(drainCtx); err != nil && a.logSvc != nil {
			a.logSvc.Error(fmt.Sprintf("stopping with %d queued order(s) not written; their numbers were already given to the caller",
				a.orderQueue.Pending()), err)
		}
		cancel()
	}
	if a.queueCancel != nil {
		a.queueCancel()
	}
	if a.receiptWriter != nil {
		if err := a.receiptWriter.CloseContext(ctx); err != nil && a.logSvc != nil {
			a.logSvc.Error("the receipt data lock was not released before the stop budget ran out; the OS releases it at exit", err)
		}
	}
	if a.dbConn != nil {
		_ = a.dbConn.Close()
	}
	if a.logSvc != nil {
		_ = a.logSvc.Close()
	}
}

// buildReceiptWriter builds the writer behind POST /api/sendReceipt, or
// returns nil when receipts are not configured (erp: hasavshevet and a
// sendReceiptDir). An invalid receipt config is an error: the daemon does not
// start on it, as the GUI does not save it.
//
// Recover finishes any receipt a crash left half-written before the route
// serves. If it fails the writer is still returned: the route retries
// recovery on each request and answers an error until it succeeds, and orders
// and queries are not held hostage to it.
func buildReceiptWriter(cfg config.Config, dataDir string, log logger.LoggerService) (*hasavshevet.ReceiptWriter, error) {
	if !cfg.ReceiptsConfigured() {
		return nil, nil
	}
	if err := hasavshevet.ValidateReceiptConfig(cfg); err != nil {
		return nil, err
	}
	w, err := hasavshevet.NewReceiptWriter(hasavshevet.ReceiptWriterConfigFrom(cfg, dataDir), log)
	if err != nil {
		return nil, err
	}
	if err := w.Recover(); err != nil {
		log.Error("receipt recovery failed; sendReceipt answers an error until it succeeds", err)
	}
	model := "a (files only)"
	switch {
	case strings.TrimSpace(cfg.HasReceiptBatFile) != "":
		model = "b (runs hasReceiptBatFile)"
	case strings.TrimSpace(cfg.HasReceiptParamFile) != "":
		model = "b (runs hasExePath with hasReceiptParamFile)"
	}
	log.Info(fmt.Sprintf("receipts configured: sendReceiptDir=%q model %s", strings.TrimSpace(cfg.SendReceiptDir), model))
	return w, nil
}

func (a *serverApp) Errors() <-chan error {
	return a.errCh
}

func (a *serverApp) Logger() autostart.Logger {
	return a.logSvc
}
