//go:build windows

package main

// The configuration window: widget declaration and the small helpers that read
// or update individual widgets. Behaviour lives in the sibling files:
//
//	form_config.go     form <-> config.Config mapping and persistence
//	actions.go         button handlers for configuration
//	service_control.go start/stop/restart of the daemon service

import (
	"strconv"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/logger"
)

// mainForm holds all widget references and application state.
//
// Every field is touched on the UI goroutine only. Background work must hand
// results back through (*walk.MainWindow).Synchronize.
type mainForm struct {
	*walk.MainWindow

	cfg    config.Config
	logSvc logger.LoggerService
	busy   bool // set on UI thread only; prevents concurrent save/start

	erpCombo        *walk.ComboBox
	apiListenEdit   *walk.LineEdit
	debugCheck      *walk.CheckBox
	bearerTokenEdit *walk.LineEdit

	legacyEnabledCheck *walk.CheckBox
	legacyFields       *walk.Composite
	legacyUserEdit     *walk.LineEdit
	legacyPassEdit     *walk.LineEdit
	legacySecretEdit   *walk.LineEdit
	legacyExpiryEdit   *walk.LineEdit
	legacyShowCheck    *walk.CheckBox
	legacyRawSQLCheck  *walk.CheckBox

	driverCombo *walk.ComboBox
	hostEdit    *walk.LineEdit
	portEdit    *walk.LineEdit
	userEdit    *walk.LineEdit
	dbNameEdit  *walk.LineEdit
	passEdit    *walk.LineEdit
	erpUserEdit *walk.LineEdit

	foldersComposite *walk.Composite
	folderEdits      []*walk.LineEdit

	sendOrderSection *walk.Composite
	sendOrderEdit    *walk.LineEdit
	hasBatEdit       *walk.LineEdit

	statusLabel *walk.Label
}

func newMainForm(cfg config.Config, logSvc logger.LoggerService) (*mainForm, error) {
	f := &mainForm{cfg: cfg, logSvc: logSvc}

	err := (MainWindow{
		AssignTo: &f.MainWindow,
		Title:    "Digitrage ERP Connector",
		MinSize:  Size{Width: 520, Height: 400},
		Size:     Size{Width: 540, Height: 700},
		Layout:   VBox{MarginsZero: true},
		Children: []Widget{
			ScrollView{
				Layout: VBox{},
				Children: []Widget{
					// ── ERP ──────────────────────────────────────────────
					Label{Text: "ERP"},
					ComboBox{
						AssignTo:              &f.erpCombo,
						Model:                 config.ErpOption(),
						OnCurrentIndexChanged: f.onERPChanged,
					},

					// ── API ──────────────────────────────────────────────
					Label{Text: "API Listen (host:port)"},
					LineEdit{AssignTo: &f.apiListenEdit},
					CheckBox{
						AssignTo: &f.debugCheck,
						Text:     "Debug mode",
					},
					Label{Text: "Bearer token"},
					Composite{
						Layout: HBox{MarginsZero: true},
						Children: []Widget{
							LineEdit{AssignTo: &f.bearerTokenEdit},
							PushButton{Text: "Generate key", OnClicked: f.onGenerateToken},
						},
					},

					// ── Legacy compatibility ─────────────────────────────
					// The bearer token above is not the only accepted
					// credential, and on a migrated box it is usually not the
					// one in use: a backend written against electron-mssql-app
					// trades a login and password for a JWT at POST /auth/token
					// and sends that instead. Both live in config.yaml, but only
					// the token had a widget, so the GUI showed a credential the
					// backend never sends and hid the one it does.
					HSeparator{},
					Label{Text: "Legacy compatibility (electron-mssql-app)"},
					CheckBox{
						AssignTo:         &f.legacyEnabledCheck,
						Text:             "Enabled — also accept the POST /auth/token login/password exchange",
						OnCheckedChanged: f.onLegacyEnabledChanged,
					},
					Composite{
						AssignTo: &f.legacyFields,
						Layout:   VBox{MarginsZero: true},
						Children: []Widget{
							Label{Text: "These must match the ClientConnection row in erp-manager:"},
							Label{Text: "JWT user (= authLogin there)"},
							LineEdit{AssignTo: &f.legacyUserEdit},
							Label{Text: "JWT password (= authPassword there)"},
							LineEdit{AssignTo: &f.legacyPassEdit, PasswordMode: true},
							Label{Text: "JWT secret (signs issued tokens — changing it invalidates live ones)"},
							LineEdit{AssignTo: &f.legacySecretEdit, PasswordMode: true},
							Label{Text: "Token expiry (minutes, blank = 30)"},
							LineEdit{AssignTo: &f.legacyExpiryEdit},
							CheckBox{
								AssignTo:         &f.legacyShowCheck,
								Text:             "Show password and secret",
								OnCheckedChanged: f.onLegacyShowChanged,
							},
							CheckBox{
								AssignTo: &f.legacyRawSQLCheck,
								Text:     "Allow raw SQL (POST /api/query, SELECT only)",
							},
						},
					},

					// ── DB ───────────────────────────────────────────────
					HSeparator{},
					Label{Text: "DB Settings"},
					Label{Text: "Driver"},
					ComboBox{
						AssignTo: &f.driverCombo,
						Model:    config.DBDriverOptions(),
					},
					Label{Text: "Host"},
					LineEdit{AssignTo: &f.hostEdit},
					Label{Text: "Port"},
					LineEdit{AssignTo: &f.portEdit},
					Label{Text: "User"},
					LineEdit{AssignTo: &f.userEdit},
					Label{Text: "Database"},
					LineEdit{AssignTo: &f.dbNameEdit},
					Label{Text: "Password"},
					LineEdit{
						AssignTo:     &f.passEdit,
						PasswordMode: true,
						CueBanner:    "Leave blank to keep existing",
					},
					Label{Text: "ERP User"},
					Composite{
						Layout: HBox{MarginsZero: true},
						Children: []Widget{
							LineEdit{AssignTo: &f.erpUserEdit},
							PushButton{Text: "Test user", OnClicked: f.onTestUser},
						},
					},

					// ── Image folders ─────────────────────────────────────
					HSeparator{},
					Label{Text: "Image folders"},
					Composite{
						AssignTo: &f.foldersComposite,
						Layout:   VBox{MarginsZero: true},
					},
					PushButton{Text: "Add new folder path", OnClicked: f.onAddFolder},

					// ── Hasavshevet-only section ──────────────────────────
					Composite{
						AssignTo: &f.sendOrderSection,
						Layout:   VBox{MarginsZero: true},
						Children: []Widget{
							Label{Text: "Send order folder"},
							Composite{
								Layout: HBox{MarginsZero: true},
								Children: []Widget{
									LineEdit{AssignTo: &f.sendOrderEdit},
									PushButton{Text: "Browse...", OnClicked: f.onBrowseSendOrder},
								},
							},
							Label{Text: "Hasavshevet BAT file (digi.bat)"},
							Composite{
								Layout: HBox{MarginsZero: true},
								Children: []Widget{
									LineEdit{
										AssignTo:  &f.hasBatEdit,
										CueBanner: `e.g. C:\Hash7\digi.bat`,
									},
									PushButton{Text: "Browse...", OnClicked: f.onBrowseHasBat},
								},
							},
						},
					},

					// ── Action buttons ────────────────────────────────────
					Composite{
						Layout: HBox{MarginsZero: true},
						Children: []Widget{
							PushButton{Text: "Test connection", OnClicked: f.onTestConnection},
							PushButton{Text: "שמירה", OnClicked: f.onSave},
							PushButton{Text: "Start server", OnClicked: f.onStartServer},
							PushButton{Text: "Stop server", OnClicked: f.onStopServer},
							PushButton{Text: "Restart server", OnClicked: f.onRestartServer},
							PushButton{Text: "Documentation", OnClicked: f.onDocumentation},
						},
					},
					Label{AssignTo: &f.statusLabel},
				},
			},
		},
	}.Create())
	if err != nil {
		return nil, err
	}

	f.populateFromConfig(cfg)
	return f, nil
}

// populateFromConfig copies config values into the widgets. The inverse of
// readFormConfig in form_config.go — keep the two in step.
func (f *mainForm) populateFromConfig(cfg config.Config) {
	f.setComboByValue(f.erpCombo, config.ErpOption(), string(cfg.ERP))
	f.apiListenEdit.SetText(cfg.APIListen)
	f.debugCheck.SetChecked(cfg.Debug)
	f.bearerTokenEdit.SetText(cfg.BearerToken)
	f.populateLegacyCompat(cfg.LegacyCompat)
	f.setComboByValue(f.driverCombo, config.DBDriverOptions(), string(cfg.DB.Driver))
	f.hostEdit.SetText(cfg.DB.Host)
	f.portEdit.SetText(strconv.Itoa(cfg.DB.Port))
	f.userEdit.SetText(cfg.DB.User)
	f.dbNameEdit.SetText(cfg.DB.Database)
	f.erpUserEdit.SetText(cfg.ERPUser)
	f.sendOrderEdit.SetText(cfg.SendOrderDir)
	f.hasBatEdit.SetText(cfg.HasBatFile)

	// Always show at least one folder row so the list is discoverable.
	if len(cfg.ImageFolders) == 0 {
		f.addFolderRow("")
	} else {
		for _, p := range cfg.ImageFolders {
			f.addFolderRow(p)
		}
	}

	f.updateSendOrderVisibility(cfg.ERP)
}

// populateLegacyCompat fills the compatibility widgets. Unlike the DB password —
// which lives in DPAPI and is deliberately never shown — jwtPassword and
// jwtSecret are stored in config.yaml, so displaying them reveals nothing that
// reading the file would not. They start masked; the Show box unmasks them.
func (f *mainForm) populateLegacyCompat(legacy config.LegacyCompatConfig) {
	f.legacyEnabledCheck.SetChecked(legacy.Enabled)
	f.legacyUserEdit.SetText(legacy.JWTUser)
	f.legacyPassEdit.SetText(legacy.JWTPassword)
	f.legacySecretEdit.SetText(legacy.JWTSecret)
	if legacy.JWTExpiryMinutes > 0 {
		f.legacyExpiryEdit.SetText(strconv.Itoa(legacy.JWTExpiryMinutes))
	} else {
		f.legacyExpiryEdit.SetText("")
	}
	f.legacyShowCheck.SetChecked(false)
	f.onLegacyShowChanged()
	f.legacyRawSQLCheck.SetChecked(legacy.AllowRawSQL)
	f.updateLegacyFieldsEnabled(legacy.Enabled)
}

// onLegacyEnabledChanged greys the compat fields out when the exchange is off,
// so credentials that are configured but inactive cannot read as active ones.
func (f *mainForm) onLegacyEnabledChanged() {
	f.updateLegacyFieldsEnabled(f.legacyEnabledCheck.Checked())
}

func (f *mainForm) updateLegacyFieldsEnabled(enabled bool) {
	f.legacyFields.SetEnabled(enabled)
}

// onLegacyShowChanged unmasks the password and secret, so they can be compared
// against the ClientConnection row in erp-manager without opening config.yaml.
func (f *mainForm) onLegacyShowChanged() {
	masked := !f.legacyShowCheck.Checked()
	f.legacyPassEdit.SetPasswordMode(masked)
	f.legacySecretEdit.SetPasswordMode(masked)
}

// setComboByValue selects the combo box item matching value; falls back to index 0.
func (*mainForm) setComboByValue(combo *walk.ComboBox, options []string, value string) {
	for i, v := range options {
		if v == value {
			combo.SetCurrentIndex(i)
			return
		}
	}
	if len(options) > 0 {
		combo.SetCurrentIndex(0)
	}
}

// comboValue returns the string value of the currently selected combo box item.
func comboValue(combo *walk.ComboBox, options []string) string {
	i := combo.CurrentIndex()
	if i >= 0 && i < len(options) {
		return options[i]
	}
	return ""
}

// addFolderRow appends a folder entry row (text field + Browse button) to
// foldersComposite. Widget creation failures are silently skipped: a missing row
// is a cosmetic problem, and there is nowhere useful to report it during layout.
func (f *mainForm) addFolderRow(path string) {
	row, err := walk.NewComposite(f.foldersComposite)
	if err != nil {
		return
	}
	row.SetLayout(walk.NewHBoxLayout())

	edit, err := walk.NewLineEdit(row)
	if err != nil {
		return
	}
	edit.SetText(path)

	btn, err := walk.NewPushButton(row)
	if err != nil {
		return
	}
	btn.SetText("Browse...")
	btn.SetMinMaxSize(walk.Size{Width: 75}, walk.Size{Width: 75})
	btn.Clicked().Attach(func() {
		f.browseFolderInto(edit, "Select folder")
	})

	f.folderEdits = append(f.folderEdits, edit)
}

// browseFolderInto shows a folder picker and writes the choice into edit.
func (f *mainForm) browseFolderInto(edit *walk.LineEdit, title string) {
	dlg := &walk.FileDialog{Title: title}
	if ok, err := dlg.ShowBrowseFolder(f.MainWindow); err != nil {
		f.setStatus("Folder selection error: " + err.Error())
	} else if ok {
		edit.SetText(dlg.FilePath)
	}
}

// updateSendOrderVisibility shows the order-pipeline fields for Hasavshevet only.
func (f *mainForm) updateSendOrderVisibility(erp config.ERPType) {
	f.sendOrderSection.SetVisible(erp == config.ERPHasavshevet)
}

func (f *mainForm) setStatus(text string) {
	f.statusLabel.SetText(text)
}

// finish clears the busy flag and reports text. Call from the UI thread; this is
// the tail of every background action.
func (f *mainForm) finish(text string) {
	f.busy = false
	f.setStatus(text)
}
