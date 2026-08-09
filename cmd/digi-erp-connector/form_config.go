//go:build windows

package main

// Mapping between the form widgets and config.Config, plus the persistence step.
// readFormConfig runs on the UI thread; persistConfig does I/O and must not.

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/db"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
	"github.com/digitrade-e/digi-erp-connector/internal/logger"
	"github.com/digitrade-e/digi-erp-connector/internal/secrets"
)

// procedureSetupTimeout bounds the Hasavshevet stored-procedure creation done on
// save; it runs DDL against the customer database.
const procedureSetupTimeout = 12 * time.Second

// readFormConfig reads all widget values and returns a Config plus the entered
// password. Must be called from the UI goroutine.
//
// It starts from f.cfg rather than a zero Config, so settings with no widget —
// queries limits, db.encrypt, hasExePath — survive a save from the GUI. Dropping
// that would silently reset them on a production box.
func (f *mainForm) readFormConfig() (config.Config, string, error) {
	port, ok := f.parsePort()
	if !ok {
		return config.Config{}, "", fmt.Errorf("DB Port must be a number between 1 and 65535, or left empty if this connector uses no database")
	}

	cfg := f.cfg
	cfg.ERP = config.ERPType(comboValue(f.erpCombo, config.ErpOption()))
	cfg.APIListen = f.apiListenEdit.Text()
	cfg.Debug = f.debugCheck.Checked()
	cfg.DB.Driver = config.DBDriver(comboValue(f.driverCombo, config.DBDriverOptions()))
	cfg.DB.Host = f.hostEdit.Text()
	cfg.DB.Port = port
	cfg.DB.User = f.userEdit.Text()
	cfg.DB.Database = f.dbNameEdit.Text()
	cfg.ERPUser = strings.TrimSpace(f.erpUserEdit.Text())

	cfg.Auth = config.AuthConfig{
		Username: strings.TrimSpace(f.authUserEdit.Text()),
		Password: strings.TrimSpace(f.authPassEdit.Text()),
		Secret:   strings.TrimSpace(f.authSecretEdit.Text()),
		TokenTTL: strings.TrimSpace(f.authTTLEdit.Text()),
	}
	// The daemon refuses to start on a half-configured exchange, so the GUI must
	// refuse to save one — otherwise the box is left holding a config its own
	// service will not start on.
	if err := cfg.Auth.Validate(); err != nil {
		return config.Config{}, "", fmt.Errorf(
			"authentication: %w — the connector cannot serve without them", err)
	}
	if !cfg.Auth.TokenTTLValid() {
		return config.Config{}, "", fmt.Errorf(
			"token lifetime %q is not a duration — use a value like 30m or 1h, or leave it blank",
			cfg.Auth.TokenTTL)
	}

	// The database is optional. A connector deployed only to write order files
	// may have no database of its own, and refusing to save left such a node
	// impossible to configure at all. Whether orders can actually be built is
	// decided at request time, where the missing piece can be named precisely.

	folders := make([]string, 0, len(f.folderEdits))
	for _, edit := range f.folderEdits {
		if p := strings.TrimSpace(edit.Text()); p != "" {
			folders = append(folders, p)
		}
	}
	cfg.ImageFolders = folders

	if cfg.ERP == config.ERPHasavshevet {
		cfg.SendOrderDir = strings.TrimSpace(f.sendOrderEdit.Text())
		cfg.HasBatFile = strings.TrimSpace(f.hasBatEdit.Text())
	} else {
		cfg.SendOrderDir = ""
		cfg.HasBatFile = ""
	}

	return cfg, f.passEdit.Text(), nil
}

// parsePort reads the DB port field. UI thread only.
//
// An empty field is valid and means "no database configured" — it yields 0, and
// db.IsConfigured treats that as unset. Only a non-numeric or out-of-range value
// is an error. Before this, leaving the DB blank produced "invalid DB Port",
// which named the wrong problem and blocked the save entirely.
func (f *mainForm) parsePort() (int, bool) {
	text := strings.TrimSpace(f.portEdit.Text())
	if text == "" {
		return 0, true
	}
	p, err := strconv.Atoi(text)
	if err != nil || p <= 0 || p > 65535 {
		return 0, false
	}
	return p, true
}

// dbConfigFromForm builds a Config carrying only the DB settings currently in
// the form, for the Test connection / Test user actions. Returns ok=false when
// the port is not a valid number.
func (f *mainForm) dbConfigFromForm() (cfg config.Config, password string, ok bool) {
	port, ok := f.parsePort()
	if !ok {
		return config.Config{}, "", false
	}

	cfg = f.cfg
	cfg.DB.Driver = config.DBDriver(comboValue(f.driverCombo, config.DBDriverOptions()))
	cfg.DB.Host = f.hostEdit.Text()
	cfg.DB.Port = port
	cfg.DB.User = f.userEdit.Text()
	cfg.DB.Database = f.dbNameEdit.Text()
	return cfg, f.passEdit.Text(), true
}

// persistConfig performs all I/O for a save: Hasavshevet procedure setup, the
// password secret, then config.yaml. Safe to call from a background goroutine.
//
// It returns a non-empty warning when the configuration was saved but something
// optional did not succeed. Only a failure to store the password or write
// config.yaml is an error.
//
// Installing the stored procedures is deliberately best-effort. It needs a
// reachable database and DDL rights, and a connector that only sends orders has
// neither — its database may be on another host, and the procedures are used
// solely by price/stock. Failing the whole save there would make such a node
// impossible to configure at all.
func persistConfig(cfg config.Config, password string, logSvc logger.LoggerService) (warning string, err error) {
	if cfg.ERP == config.ERPHasavshevet {
		warning = ensureProceduresBestEffort(cfg, password, logSvc)
	}

	// An empty field means "keep the stored password", so only overwrite when
	// something was actually typed.
	if password != "" {
		if err := secrets.Set(dbPasswordKey(cfg.ERP), []byte(password)); err != nil {
			return "", fmt.Errorf("failed to save password: %w", err)
		}
	}

	if err := config.Save(cfg); err != nil {
		return "", fmt.Errorf("error saving config: %w", err)
	}
	return warning, nil
}

// ensureProceduresBestEffort tries to install the Hasavshevet stored procedures
// and returns a human-readable warning instead of an error when it cannot.
func ensureProceduresBestEffort(cfg config.Config, enteredPassword string, logSvc logger.LoggerService) string {
	pw, err := resolveDBPassword(cfg.ERP, enteredPassword, true)
	if err != nil {
		logSvc.Warn("skipping Hasavshevet procedure setup: no DB password available")
		return "Saved. Note: no DB password, so GPRICE_Bulk/GetOnHandStockForSkus were not installed (needed only for price & stock)."
	}

	if err := ensureHasavshevetProcedures(cfg, pw, logSvc); err != nil {
		logSvc.Warn("Hasavshevet procedure setup failed: " + err.Error())
		return "Saved. Note: could not install GPRICE_Bulk/GetOnHandStockForSkus (" + err.Error() +
			"). Needed only for price & stock — sending orders is unaffected."
	}
	return ""
}

// ensureHasavshevetProcedures installs (CREATE OR ALTER) the stored procedures
// the price/stock endpoint depends on.
func ensureHasavshevetProcedures(cfg config.Config, password string, logSvc logger.LoggerService) error {
	ctx, cancel := context.WithTimeout(context.Background(), procedureSetupTimeout)
	defer cancel()

	dbConn, err := db.Open(cfg, password, db.DefaultOptions())
	if err != nil {
		return fmt.Errorf("failed to connect for Hasavshevet procedure setup: %w", err)
	}
	defer dbConn.Close()

	procedures := []struct {
		name   string
		ensure func(context.Context, *sql.DB) (bool, error)
	}{
		{"GPRICE_Bulk", hasavshevet.EnsureGPriceBulkProcedure},
		{"GetOnHandStockForSkus", hasavshevet.EnsureOnHandStockForSkusProcedure},
	}

	for _, p := range procedures {
		created, err := p.ensure(ctx, dbConn)
		if err != nil {
			logSvc.Error("failed to initialize "+p.name, err)
			return fmt.Errorf("failed to initialize %s: %w", p.name, err)
		}
		if created {
			logSvc.Success(p.name + " created")
		} else {
			logSvc.Info(p.name + " already exists")
		}
	}
	return nil
}
