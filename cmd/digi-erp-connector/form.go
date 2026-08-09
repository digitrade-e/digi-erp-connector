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

	"github.com/digitrade-e/digi-erp-connector/internal/auth"
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

	authEnabledCheck *walk.CheckBox
	authFields       *walk.Composite
	authUserEdit     *walk.LineEdit
	authPassEdit     *walk.LineEdit
	authSecretEdit   *walk.LineEdit
	authTTLEdit      *walk.LineEdit
	authShowCheck    *walk.CheckBox

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

					// ── Credential exchange ───────────────────────────────
					// A second, optional credential: callers POST a username
					// and password to /auth/token and get a short-lived token
					// back. Backends that authenticate this way share those
					// fields with other integrations and cannot simply be
					// repointed at the bearer token above.
					//
					// The credentials are set here, per installation. Nothing
					// is shipped: the predecessor's fixed password and
					// source-embedded signing secret are exactly what this
					// replaces.
					HSeparator{},
					Label{Text: "Credential exchange (POST /auth/token)"},
					CheckBox{
						AssignTo:         &f.authEnabledCheck,
						Text:             "Enabled — also issue tokens in exchange for a username and password",
						OnCheckedChanged: f.onAuthEnabledChanged,
					},
					Composite{
						AssignTo: &f.authFields,
						Layout:   VBox{MarginsZero: true},
						Children: []Widget{
							Label{Text: "These must match what the calling backend is configured with:"},
							Label{Text: "Username"},
							LineEdit{AssignTo: &f.authUserEdit},
							Label{Text: "Password"},
							Composite{
								Layout: HBox{MarginsZero: true},
								Children: []Widget{
									LineEdit{AssignTo: &f.authPassEdit, PasswordMode: true},
									PushButton{Text: "Generate", OnClicked: f.onGenerateAuthPassword},
								},
							},
							Label{Text: "Signing secret (unique to this install; changing it invalidates issued tokens)"},
							Composite{
								Layout: HBox{MarginsZero: true},
								Children: []Widget{
									LineEdit{AssignTo: &f.authSecretEdit, PasswordMode: true},
									PushButton{Text: "Regenerate", OnClicked: f.onGenerateAuthSecret},
								},
							},
							Label{Text: "Token lifetime (e.g. 30m, 1h; blank = 30m)"},
							LineEdit{AssignTo: &f.authTTLEdit},
							CheckBox{
								AssignTo:         &f.authShowCheck,
								Text:             "Show password and secret",
								OnCheckedChanged: f.onAuthShowChanged,
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

	f.populateAuth(cfg.Auth)
	f.updateSendOrderVisibility(cfg.ERP)
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

// populateAuth fills the credential-exchange widgets.
//
// The password and secret live in config.yaml in the clear, so showing them
// reveals nothing that opening the file would not — and being able to read them
// back is how an operator confirms the calling backend was given the right
// values. They start masked all the same, because this window is often open on a
// shared screen.
func (f *mainForm) populateAuth(a config.AuthConfig) {
	f.authEnabledCheck.SetChecked(a.Enabled)
	f.authUserEdit.SetText(a.Username)
	f.authPassEdit.SetText(a.Password)
	f.authSecretEdit.SetText(a.Secret)
	f.authTTLEdit.SetText(a.TokenTTL)
	f.authShowCheck.SetChecked(false)
	f.onAuthShowChanged()
	f.updateAuthFieldsEnabled(a.Enabled)
}

// onAuthEnabledChanged greys the fields out when the exchange is off, so
// credentials that are configured but inactive cannot read as active ones.
func (f *mainForm) onAuthEnabledChanged() {
	f.updateAuthFieldsEnabled(f.authEnabledCheck.Checked())
}

func (f *mainForm) updateAuthFieldsEnabled(enabled bool) {
	f.authFields.SetEnabled(enabled)
}

func (f *mainForm) onAuthShowChanged() {
	masked := !f.authShowCheck.Checked()
	f.authPassEdit.SetPasswordMode(masked)
	f.authSecretEdit.SetPasswordMode(masked)
}

// onGenerateAuthPassword produces a random password rather than letting an
// operator invent one — installations that pick their own tend to pick the same.
func (f *mainForm) onGenerateAuthPassword() {
	pw, err := auth.NewPassword()
	if err != nil {
		f.setStatus("Failed to generate a password: " + err.Error())
		return
	}
	f.authPassEdit.SetText(pw)
	f.setStatus("New password generated — give it to the calling backend before restarting the service.")
}

// onGenerateAuthSecret replaces this installation's signing secret. Every token
// already issued stops verifying, which callers recover from by re-authenticating
// on the 401 — so it is safe, but it is not silent.
func (f *mainForm) onGenerateAuthSecret() {
	secret, err := auth.NewSecret()
	if err != nil {
		f.setStatus("Failed to generate a signing secret: " + err.Error())
		return
	}
	f.authSecretEdit.SetText(secret)
	f.setStatus("New signing secret generated — tokens already issued will be rejected until callers re-authenticate.")
}
