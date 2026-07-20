package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/api"
	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/db"
	"github.com/digitrade-e/digi-erp-connector/internal/email"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
	"github.com/digitrade-e/digi-erp-connector/internal/logger"
	"github.com/digitrade-e/digi-erp-connector/internal/pdf"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/autostart"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/paths"
	"github.com/digitrade-e/digi-erp-connector/internal/print"
	"github.com/digitrade-e/digi-erp-connector/internal/queries"
	"github.com/digitrade-e/digi-erp-connector/internal/secrets"
)

const windowsServiceName = "digi-erp-connectord"

type serverApp struct {
	cfg          config.Config
	logSvc       logger.LoggerService
	dbConn       *sql.DB
	srv          *http.Server
	errCh        chan error
	dbPassStr    string
	orderQueue   *hasavshevet.OrderQueue
	queueCancel  context.CancelFunc
}

func (a *serverApp) Start() error {
	// Bootstrap logger writes directly to server.log so we capture pre-config
	// failures (config.Load errors, permissions, missing dirs) even when
	// running as a Windows service where stderr is unavailable.
	bootstrapLog := logger.NewBootstrap()
	bootstrapLog.Info(fmt.Sprintf("daemon Start() called: pid=%d goos=%s", os.Getpid(), runtime.GOOS))

	cfg, err := config.Load()
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			bootstrapLog.Error("config not found; run digi-erp-connector UI to create it", nil)
			return err
		}
		bootstrapLog.Error("failed to load config", err)
		return err
	}
	a.cfg = cfg
	bootstrapLog.Info(fmt.Sprintf("config loaded: erp=%s apiListen=%s sendOrderDir=%q", cfg.ERP, cfg.APIListen, cfg.SendOrderDir))

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
	dbConn, err := db.Open(cfg, a.dbPassStr, db.DefaultOptions())
	if err != nil {
		logSvc.Error("db connection failed", err)
		a.Stop(context.Background())
		return err
	}
	a.dbConn = dbConn
	logSvc.Info("db.Open returned successfully")

	// Build the send-order queue for Hasavshevet.
	// Order number file lives next to IMOVEIN files for self-contained directory.
	numStorePath := filepath.Join(cfg.SendOrderDir, "lastOrderNumber.json")
	numStore := hasavshevet.NewOrderNumberStore(numStorePath)
	sender := hasavshevet.NewSender(dbConn, cfg, numStore, logSvc)

	// Set up post-order hooks (PDF generation, printing, email).
	logSvc.Info(fmt.Sprintf(
		"PDF config snapshot at startup: PrintAfterOrder=%v EmailAfterOrder=%v UseRemoteTemplate=%v RemoteTemplateBaseURL=%q tokenCount=%d ChromePath=%q SumatraPDFPath=%q PrinterName=%q",
		cfg.PDF.PrintAfterOrder, cfg.PDF.EmailAfterOrder, cfg.PDF.UseRemoteTemplate,
		cfg.PDF.RemoteTemplateBaseURL, len(cfg.PDF.RemoteTokens),
		cfg.PDF.ChromePath, cfg.PDF.SumatraPDFPath, cfg.PDF.PrinterName,
	))

	if cfg.PDF.PrintAfterOrder {
		logVisiblePrintersAndValidate(logSvc, cfg.PDF.PrinterName)
	}
	var postHooks []hasavshevet.PostOrderHook
	if cfg.PDF.PrintAfterOrder || cfg.PDF.EmailAfterOrder {
		chromePath := cfg.PDF.ChromePath
		if chromePath == "" {
			chromePath = pdf.DetectChrome()
			logSvc.Info(fmt.Sprintf("ChromePath empty in config; auto-detect resolved=%q", chromePath))
		}
		if chromePath == "" {
			logSvc.Warn("Chrome not found; PDF generation after order will be skipped (no PDF post-order hook will be registered)")
		} else {
			pdfGen := pdf.NewGenerator(chromePath)

			var emailSender *email.Sender
			if cfg.PDF.EmailAfterOrder && cfg.SMTP.Host != "" {
				smtpPass, _ := secrets.Get("smtp_password")
				emailSender = email.NewSender(cfg.SMTP, string(smtpPass))
				logSvc.Info("email after order enabled")
			}

			postHooks = append(postHooks, hasavshevet.NewPDFPostOrderHook(
				cfg, pdfGen, emailSender, logSvc,
			))
			logSvc.Info(fmt.Sprintf("PDF post-order hook enabled (print=%v, email=%v, chrome=%s)",
				cfg.PDF.PrintAfterOrder, cfg.PDF.EmailAfterOrder, chromePath))
		}
	} else {
		logSvc.Warn("no PDF post-order hook registered: both PrintAfterOrder and EmailAfterOrder are false in config — toggle them in the GUI Settings → PDF & Email Settings, click Save, then RESTART digi-erp-connectord for changes to take effect")
	}

	queue := hasavshevet.NewOrderQueue(sender, logSvc, postHooks...)
	queueCtx, queueCancel := context.WithCancel(context.Background())
	queue.Start(queueCtx)
	a.orderQueue = queue
	a.queueCancel = queueCancel

	queryStorePath := filepath.Join(paths.DataDir(), "queries.json")
	queryStore, err := queries.NewStore(queryStorePath)
	if err != nil {
		logSvc.Error("failed to load saved queries store", err)
		a.Stop(context.Background())
		return err
	}
	logSvc.Info(fmt.Sprintf("saved queries store loaded: path=%s count=%d", queryStorePath, queryStore.Count()))

	srv, err := api.NewServer(cfg, api.ServerDeps{
		DBPassword:     a.dbPassStr,
		DB:             dbConn,
		Logger:         logSvc,
		SendOrderQueue: queue,
		QueryStore:     queryStore,
	})
	if err != nil {
		logSvc.Error("config validation error", err)
		a.Stop(context.Background())
		return err
	}
	a.srv = srv

	a.errCh = make(chan error, 1)
	go func() {
		a.errCh <- srv.ListenAndServe()
	}()
	logSvc.Info(fmt.Sprintf("HTTP server goroutine launched, will listen on %s", srv.Addr))

	logSvc.Info(fmt.Sprintf("digi-erp-connectord listening on %s", srv.Addr))
	return nil
}

func (a *serverApp) Stop(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	if a.queueCancel != nil {
		a.queueCancel()
	}
	if a.srv != nil {
		_ = a.srv.Shutdown(ctx)
	}
	if a.dbConn != nil {
		_ = a.dbConn.Close()
	}
	if a.logSvc != nil {
		_ = a.logSvc.Close()
	}
}

func (a *serverApp) Errors() <-chan error {
	return a.errCh
}

func (a *serverApp) Logger() autostart.Logger {
	return a.logSvc
}

// logVisiblePrintersAndValidate enumerates the printers visible to the daemon
// process and validates the configured PrinterName against that list. The
// daemon typically runs as a Windows service under LocalSystem, which sees
// only machine-wide printers — per-user printers from the interactive user's
// session are invisible. This logs both the snapshot and a clear WARN when
// the configured printer is missing or uses a WSD port (incompatible with
// services). The data is invaluable when troubleshooting silent print failures.
func logVisiblePrintersAndValidate(logSvc logger.LoggerService, configuredName string) {
	account := "<unknown>"
	if u, err := user.Current(); err == nil {
		account = u.Username
	}

	printers, err := print.EnumeratePrinters()
	if err != nil {
		logSvc.Warn(fmt.Sprintf("EnumeratePrinters failed (account=%s): %v — print issues will be hard to diagnose", account, err))
		return
	}
	if len(printers) == 0 {
		logSvc.Warn(fmt.Sprintf("no printers visible to daemon (account=%s). If running as a service under LocalSystem, install the printer machine-wide or run the service under a user account that has the printer.", account))
		return
	}

	descriptions := make([]string, 0, len(printers))
	for _, p := range printers {
		descriptions = append(descriptions, fmt.Sprintf("%s [port=%s, driver=%s]", p.Name, p.PortName, p.DriverName))
	}
	logSvc.Info(fmt.Sprintf("printers visible to daemon (account=%s, count=%d): %s",
		account, len(printers), strings.Join(descriptions, "; "),
	))

	if configuredName == "" {
		logSvc.Info("PrinterName empty in config — system default will be used at print time")
		return
	}

	matched := print.FindPrinter(printers, configuredName)
	if matched == nil {
		logSvc.Warn(fmt.Sprintf(
			"configured PrinterName=%q is NOT in the daemon's visible printer list. "+
				"If the daemon runs as LocalSystem, per-user printers are invisible — install the printer for all users, "+
				"or pick one of: %s",
			configuredName, strings.Join(printerNames(printers), ", "),
		))
		return
	}

	if print.IsServiceUnsafePort(matched.PortName) {
		logSvc.Warn(fmt.Sprintf(
			"configured PrinterName=%q uses port=%q (WSD). WSD printers do NOT work reliably from a service "+
				"running as LocalSystem because WSD requires the user-session Function Discovery service. "+
				"Print jobs will appear to succeed (SumatraPDF returns 0) but never reach the device. "+
				"Install a Standard TCP/IP Port for the same physical printer and switch PrinterName to that.",
			configuredName, matched.PortName,
		))
		return
	}

	logSvc.Info(fmt.Sprintf(
		"configured PrinterName=%q resolved to port=%q driver=%q (service-safe)",
		matched.Name, matched.PortName, matched.DriverName,
	))
}

func printerNames(printers []print.PrinterInfo) []string {
	out := make([]string, 0, len(printers))
	for _, p := range printers {
		out = append(out, p.Name)
	}
	return out
}
