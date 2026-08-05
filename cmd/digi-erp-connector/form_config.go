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
// legacyCompat, queries limits, db.encrypt, hasExePath — survive a save from the
// GUI. Dropping that would silently disable the compatibility layer on a
// production box.
func (f *mainForm) readFormConfig() (config.Config, string, error) {
	port, ok := f.parsePort()
	if !ok {
		return config.Config{}, "", fmt.Errorf("invalid DB Port")
	}

	cfg := f.cfg
	cfg.ERP = config.ERPType(comboValue(f.erpCombo, config.ErpOption()))
	cfg.APIListen = f.apiListenEdit.Text()
	cfg.Debug = f.debugCheck.Checked()
	cfg.BearerToken = strings.TrimSpace(f.bearerTokenEdit.Text())
	cfg.DB.Driver = config.DBDriver(comboValue(f.driverCombo, config.DBDriverOptions()))
	cfg.DB.Host = f.hostEdit.Text()
	cfg.DB.Port = port
	cfg.DB.User = f.userEdit.Text()
	cfg.DB.Database = f.dbNameEdit.Text()
	cfg.ERPUser = strings.TrimSpace(f.erpUserEdit.Text())

	if cfg.ERP == config.ERPHasavshevet && strings.TrimSpace(cfg.DB.Database) == "" {
		return config.Config{}, "", fmt.Errorf("DB database is required for Hasavshevet")
	}

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

// parsePort parses and validates the port field. UI thread only.
func (f *mainForm) parsePort() (int, bool) {
	p, err := strconv.Atoi(f.portEdit.Text())
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
// Order matters: the procedures are created first because they need a working
// connection, so a bad password or host fails before anything is written.
func persistConfig(cfg config.Config, password string, logSvc logger.LoggerService) error {
	pw, err := resolveDBPassword(cfg.ERP, password, cfg.ERP == config.ERPHasavshevet)
	if err != nil {
		return err
	}

	if cfg.ERP == config.ERPHasavshevet {
		if err := ensureHasavshevetProcedures(cfg, pw, logSvc); err != nil {
			return err
		}
	}

	// An empty field means "keep the stored password", so only overwrite when
	// something was actually typed.
	if password != "" {
		if err := secrets.Set(dbPasswordKey(cfg.ERP), []byte(password)); err != nil {
			return fmt.Errorf("failed to save password: %w", err)
		}
	}

	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("error saving config: %w", err)
	}
	return nil
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
