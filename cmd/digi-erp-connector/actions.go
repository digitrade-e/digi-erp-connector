//go:build windows

package main

// Configuration button handlers. All of these run on the UI thread; anything
// that blocks (a DB round-trip, a file write) is moved to a goroutine and hands
// its result back through Synchronize.

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/lxn/walk"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/db"
)

// dbTestTimeout bounds the Test connection / Test user probes.
const dbTestTimeout = 8 * time.Second

const documentationURL = "https://drive.google.com/file/d/1wWlVuB6Gyab6_SGN11e3TIl-14JVQ2Zz/view?usp=sharing"

func (f *mainForm) onERPChanged() {
	f.updateSendOrderVisibility(config.ERPType(comboValue(f.erpCombo, config.ErpOption())))
}

func (f *mainForm) onGenerateToken() {
	token, err := newBearerToken()
	if err != nil {
		f.setStatus("Failed to generate key: " + err.Error())
		return
	}
	f.bearerTokenEdit.SetText(token)
}

func (f *mainForm) onAddFolder() {
	f.addFolderRow("")
}

func (f *mainForm) onBrowseSendOrder() {
	f.browseFolderInto(f.sendOrderEdit, "Select send order folder")
}

func (f *mainForm) onBrowseHasBat() {
	dlg := &walk.FileDialog{
		Title:  "Select Hasavshevet BAT file",
		Filter: "BAT Files (*.bat)|*.bat|All Files (*.*)|*.*",
	}
	if ok, err := dlg.ShowOpen(f.MainWindow); err != nil {
		f.setStatus("File selection error: " + err.Error())
	} else if ok {
		f.hasBatEdit.SetText(dlg.FilePath)
	}
}

func (f *mainForm) onDocumentation() {
	if err := exec.Command("cmd", "/c", "start", documentationURL).Start(); err != nil {
		f.setStatus("Failed to open documentation: " + err.Error())
	}
}

// onTestUser checks that the configured ERP user exists in the customer's USERS
// table, which is the usual cause of a working DB connection but failing orders.
func (f *mainForm) onTestUser() {
	loginName := strings.TrimSpace(f.erpUserEdit.Text())
	if loginName == "" {
		f.setStatus("ERP user is required")
		return
	}
	cfg, password, ok := f.dbConfigFromForm()
	if !ok {
		f.setStatus("Invalid DB Port")
		return
	}

	f.setStatus("Testing user...")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), dbTestTimeout)
		defer cancel()

		dbConn, err := db.Open(cfg, password, db.DefaultOptions())
		if err != nil {
			f.Synchronize(func() { f.setStatus("Connection failed: " + err.Error()) })
			return
		}
		defer dbConn.Close()

		var found string
		err = dbConn.QueryRowContext(ctx,
			"SELECT LoginName FROM USERS WHERE LoginName = @p1", loginName).Scan(&found)
		f.Synchronize(func() {
			if err != nil {
				f.setStatus("User not found: " + loginName)
				return
			}
			f.setStatus("User OK: " + found)
		})
	}()
}

func (f *mainForm) onTestConnection() {
	cfg, password, ok := f.dbConfigFromForm()
	if !ok {
		f.setStatus("Invalid DB Port")
		return
	}
	cfg.ERP = config.ERPType(comboValue(f.erpCombo, config.ErpOption()))
	cfg.APIListen = f.apiListenEdit.Text()

	f.setStatus("Testing connection...")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), dbTestTimeout)
		defer cancel()

		err := db.TestConnection(ctx, cfg, password)
		f.Synchronize(func() {
			if err != nil {
				f.setStatus("Connection failed: " + err.Error())
				return
			}
			f.setStatus("Connection OK")
		})
	}()
}

// confirmLegacyDisable asks before the compatibility layer is switched off, and
// reports whether the save should go ahead.
//
// Turning it off is a single click, and on a migrated box it is the click that
// cuts the backend off: erp-manager authenticates by trading its ClientConnection
// authLogin/authPassword for a JWT at POST /auth/token, and that route stops
// existing along with the tokens it issued. Nothing else in the GUI can break
// production this quietly, which is why this is the only confirmation prompt.
func (f *mainForm) confirmLegacyDisable(next config.Config) bool {
	if !f.cfg.LegacyCompat.Enabled || next.LegacyCompat.Enabled {
		return true
	}

	answer := walk.MsgBox(
		f.MainWindow,
		"Disable legacy compatibility?",
		"POST /auth/token will stop existing and every token it issued will be rejected.\r\n\r\n"+
			"A backend that authenticates with a login and password loses access as soon as the "+
			"service restarts — erp-manager does exactly that, through the authLogin/authPassword "+
			"on its ClientConnection. It has to be switched to the static bearer token first.\r\n\r\n"+
			"Disable it anyway?",
		walk.MsgBoxYesNo|walk.MsgBoxIconWarning,
	)
	return answer == walk.DlgCmdYes
}

func (f *mainForm) onSave() {
	if f.busy {
		return
	}
	cfg, password, err := f.readFormConfig()
	if err != nil {
		f.setStatus(err.Error())
		return
	}
	if !f.confirmLegacyDisable(cfg) {
		f.setStatus("Save cancelled — legacy compatibility is still enabled.")
		return
	}

	f.busy = true
	f.setStatus("Saving...")
	go func() {
		warning, err := persistConfig(cfg, password, f.logSvc)
		f.Synchronize(func() {
			if err != nil {
				f.finish(err.Error())
				return
			}
			f.cfg = cfg
			if warning != "" {
				// Saved, but something optional did not succeed — say so rather
				// than reporting plain success.
				f.finish(warning)
				return
			}
			f.finish("נשמר בהצלחה.")
		})
	}()
}
