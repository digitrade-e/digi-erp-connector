//go:build windows

//go:generate rsrc -manifest app.manifest -o rsrc.syso

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/db"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
	"github.com/digitrade-e/digi-erp-connector/internal/logger"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/autostart"
	"github.com/digitrade-e/digi-erp-connector/internal/secrets"
)

const connectordWindowsServiceName = "digi-erp-connectord"

const documentationURL = "https://drive.google.com/file/d/1wWlVuB6Gyab6_SGN11e3TIl-14JVQ2Zz/view?usp=sharing"

// mainForm holds all widget references and application state.
type mainForm struct {
	*walk.MainWindow

	cfg    config.Config
	logSvc logger.LoggerService
	busy   bool // set on UI thread only; prevents concurrent save/start

	erpCombo        *walk.ComboBox
	apiListenEdit   *walk.LineEdit
	debugCheck      *walk.CheckBox
	bearerTokenEdit *walk.LineEdit

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

	// Populate widget values from config.
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

	// Populate dynamic folder list.
	if len(cfg.ImageFolders) == 0 {
		f.addFolderRow("")
	} else {
		for _, p := range cfg.ImageFolders {
			f.addFolderRow(p)
		}
	}

	f.updateSendOrderVisibility(cfg.ERP)

	return f, nil
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

// addFolderRow appends a folder entry row (text field + Browse button) to foldersComposite.
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
		dlg := &walk.FileDialog{Title: "Select folder"}
		if ok, err := dlg.ShowBrowseFolder(f.MainWindow); err != nil {
			f.setStatus("Folder selection error: " + err.Error())
		} else if ok {
			edit.SetText(dlg.FilePath)
		}
	})

	f.folderEdits = append(f.folderEdits, edit)
}

func (f *mainForm) updateSendOrderVisibility(erp config.ERPType) {
	f.sendOrderSection.SetVisible(erp == config.ERPHasavshevet)
}

func (f *mainForm) setStatus(text string) {
	f.statusLabel.SetText(text)
}

// ── Event handlers (UI thread) ──────────────────────────────────────────────

func (f *mainForm) onERPChanged() {
	erp := config.ERPType(comboValue(f.erpCombo, config.ErpOption()))
	f.updateSendOrderVisibility(erp)
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
	dlg := &walk.FileDialog{Title: "Select send order folder"}
	if ok, err := dlg.ShowBrowseFolder(f.MainWindow); err != nil {
		f.setStatus("Folder selection error: " + err.Error())
	} else if ok {
		f.sendOrderEdit.SetText(dlg.FilePath)
	}
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

func (f *mainForm) onTestUser() {
	loginName := strings.TrimSpace(f.erpUserEdit.Text())
	if loginName == "" {
		f.setStatus("ERP user is required")
		return
	}
	p, ok := f.parsePort()
	if !ok {
		f.setStatus("Invalid DB Port")
		return
	}
	tmp := f.cfg
	tmp.DB.Driver = config.DBDriver(comboValue(f.driverCombo, config.DBDriverOptions()))
	tmp.DB.Host = f.hostEdit.Text()
	tmp.DB.Port = p
	tmp.DB.User = f.userEdit.Text()
	tmp.DB.Database = f.dbNameEdit.Text()
	pass := f.passEdit.Text()

	f.setStatus("Testing user...")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		dbConn, err := db.Open(tmp, pass, db.DefaultOptions())
		if err != nil {
			f.Synchronize(func() { f.setStatus("Connection failed: " + err.Error()) })
			return
		}
		defer dbConn.Close()
		var found string
		err = dbConn.QueryRowContext(ctx, "SELECT LoginName FROM USERS WHERE LoginName = @p1", loginName).Scan(&found)
		f.Synchronize(func() {
			if err != nil {
				f.setStatus("User not found: " + loginName)
			} else {
				f.setStatus("User OK: " + found)
			}
		})
	}()
}

func (f *mainForm) onTestConnection() {
	p, ok := f.parsePort()
	if !ok {
		f.setStatus("Invalid DB Port")
		return
	}
	tmp := f.cfg
	tmp.ERP = config.ERPType(comboValue(f.erpCombo, config.ErpOption()))
	tmp.APIListen = f.apiListenEdit.Text()
	tmp.DB.Driver = config.DBDriver(comboValue(f.driverCombo, config.DBDriverOptions()))
	tmp.DB.Host = f.hostEdit.Text()
	tmp.DB.Port = p
	tmp.DB.User = f.userEdit.Text()
	tmp.DB.Database = f.dbNameEdit.Text()
	pass := f.passEdit.Text()

	f.setStatus("Testing connection...")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		err := db.TestConnection(ctx, tmp, pass)
		f.Synchronize(func() {
			if err != nil {
				f.setStatus("Connection failed: " + err.Error())
			} else {
				f.setStatus("Connection OK")
			}
		})
	}()
}

func (f *mainForm) onSave() {
	if f.busy {
		return
	}
	cfg, pass, err := f.readFormConfig()
	if err != nil {
		f.setStatus(err.Error())
		return
	}
	f.busy = true
	f.setStatus("Saving...")
	go func() {
		err := persistConfig(cfg, pass, f.logSvc)
		f.Synchronize(func() {
			f.busy = false
			if err != nil {
				f.setStatus(err.Error())
			} else {
				f.cfg = cfg
				f.setStatus("נשמר בהצלחה.")
			}
		})
	}()
}

func (f *mainForm) onStartServer() {
	if f.busy {
		return
	}
	cfg, pass, err := f.readFormConfig()
	if err != nil {
		f.setStatus(err.Error())
		return
	}
	f.busy = true
	f.setStatus("Saving and starting server...")
	go func() {
		if err := persistConfig(cfg, pass, f.logSvc); err != nil {
			f.Synchronize(func() {
				f.busy = false
				f.setStatus(err.Error())
			})
			return
		}
		daemonPath, err := findConnectordBinary()
		if err != nil {
			f.Synchronize(func() {
				f.busy = false
				f.setStatus("Start failed: " + err.Error())
			})
			return
		}

		var msg string
		if runtime.GOOS == "windows" {
			created, err := autostart.EnsureWindowsServiceAutoStart(connectordWindowsServiceName, daemonPath)
			if err != nil {
				f.Synchronize(func() {
					f.busy = false
					f.setStatus("Failed to create/update server service: " + err.Error())
				})
				return
			}
			if err := autostart.StartWindowsService(connectordWindowsServiceName); err != nil {
				f.Synchronize(func() {
					f.busy = false
					f.setStatus("Failed to start server service: " + err.Error())
				})
				return
			}
			msg = "Server service started."
			if created {
				msg = "Server service created and started."
			}
		} else {
			cmd := exec.Command(daemonPath)
			if err := cmd.Start(); err != nil {
				f.Synchronize(func() {
					f.busy = false
					f.setStatus("Failed to start server: " + err.Error())
				})
				return
			}
			_ = cmd.Process.Release()
			msg = "Server started."
		}

		f.Synchronize(func() {
			f.busy = false
			f.cfg = cfg
			f.setStatus(msg)
		})
	}()
}

func (f *mainForm) onStopServer() {
	if f.busy {
		return
	}
	f.busy = true
	f.setStatus("Stopping server...")
	go func() {
		err := autostart.StopWindowsService(connectordWindowsServiceName, 20*time.Second)
		f.Synchronize(func() {
			f.busy = false
			if err != nil {
				f.setStatus("Failed to stop server service: " + err.Error())
			} else {
				f.setStatus("Server service stopped.")
			}
		})
	}()
}

func (f *mainForm) onRestartServer() {
	if f.busy {
		return
	}
	f.busy = true
	f.setStatus("Restarting server...")
	go func() {
		stopErr := autostart.StopWindowsService(connectordWindowsServiceName, 20*time.Second)
		startErr := autostart.StartWindowsService(connectordWindowsServiceName)
		f.Synchronize(func() {
			f.busy = false
			switch {
			case startErr != nil && stopErr != nil:
				f.setStatus("Failed to restart server service: " + stopErr.Error() + "; " + startErr.Error())
			case startErr != nil:
				f.setStatus("Failed to restart server service: " + startErr.Error())
			default:
				f.setStatus("Server service restarted.")
			}
		})
	}()
}

// ── Config helpers ───────────────────────────────────────────────────────────

// readFormConfig reads all widget values on the UI thread and returns a Config
// and the password string. Must be called from the UI goroutine.
func (f *mainForm) readFormConfig() (config.Config, string, error) {
	p, ok := f.parsePort()
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
	cfg.DB.Port = p
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

// persistConfig performs all I/O: DB procedure setup, password save, config save.
// Safe to call from a background goroutine.
func persistConfig(cfg config.Config, password string, logSvc logger.LoggerService) error {
	pw, err := resolveDBPassword(cfg.ERP, password, cfg.ERP == config.ERPHasavshevet)
	if err != nil {
		return err
	}

	if cfg.ERP == config.ERPHasavshevet {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()

		dbConn, err := db.Open(cfg, pw, db.DefaultOptions())
		if err != nil {
			return fmt.Errorf("failed to connect for Hasavshevet procedure setup: %w", err)
		}
		defer dbConn.Close()

		created, err := hasavshevet.EnsureGPriceBulkProcedure(ctx, dbConn)
		if err != nil {
			logSvc.Error("failed to initialize GPRICE_Bulk", err)
			return fmt.Errorf("failed to initialize GPRICE_Bulk: %w", err)
		}
		if created {
			logSvc.Success("GPRICE_Bulk created")
		} else {
			logSvc.Info("GPRICE_Bulk already exists")
		}

		created, err = hasavshevet.EnsureOnHandStockForSkusProcedure(ctx, dbConn)
		if err != nil {
			logSvc.Error("failed to initialize GetOnHandStockForSkus", err)
			return fmt.Errorf("failed to initialize GetOnHandStockForSkus: %w", err)
		}
		if created {
			logSvc.Success("GetOnHandStockForSkus created")
		} else {
			logSvc.Info("GetOnHandStockForSkus already exists")
		}
	}

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

// ── Binary discovery ─────────────────────────────────────────────────────────

func findConnectordBinary() (string, error) {
	var searchDirs []string
	var candidates []string

	if exePath, err := os.Executable(); err == nil {
		dir := filepath.Dir(exePath)
		searchDirs = append(searchDirs, dir)
		candidates = append(candidates,
			filepath.Join(dir, "digi-erp-connectord"),
			filepath.Join(dir, "digi-erp-connectord.exe"),
		)
	}
	if wd, err := os.Getwd(); err == nil {
		searchDirs = append(searchDirs, wd)
		candidates = append(candidates,
			filepath.Join(wd, "digi-erp-connectord"),
			filepath.Join(wd, "digi-erp-connectord.exe"),
		)
	}

	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c, nil
		}
	}
	for _, name := range []string{"digi-erp-connectord", "digi-erp-connectord.exe"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	for _, dir := range searchDirs {
		if dir == "" {
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(dir, "digi-erp-connectord*.exe"))
		if len(matches) == 0 {
			matches, _ = filepath.Glob(filepath.Join(dir, "digi-erp-connectord*"))
		}
		if len(matches) == 0 {
			continue
		}
		best, bestTime := matches[0], time.Time{}
		if info, err := os.Stat(best); err == nil {
			bestTime = info.ModTime()
		}
		for _, c := range matches[1:] {
			if info, err := os.Stat(c); err == nil && info.ModTime().After(bestTime) {
				best, bestTime = c, info.ModTime()
			}
		}
		return best, nil
	}
	return "", fmt.Errorf("digi-erp-connectord binary not found")
}

// ── Entry point ──────────────────────────────────────────────────────────────

func main() {
	runtime.LockOSThread()

	uiLog := newUILogger()
	defer uiLog.Close()

	defer func() {
		if rec := recover(); rec != nil {
			uiLog.Printf("panic: %v", rec)
			uiStartupAlert(fmt.Errorf("unexpected error; see UI log for details"))
		}
	}()

	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.SetOutput(uiLog.Writer())

	uiLog.Printf("startup begin")
	if session := os.Getenv("SESSIONNAME"); session != "" {
		uiLog.Printf("session: %s", session)
	}

	if err := uiStartupGuard(); err != nil {
		uiLog.Printf("startup blocked: %v", err)
		uiStartupAlert(err)
		return
	}

	if exe, err := os.Executable(); err == nil {
		uiLog.Printf("exe: %s", exe)
	}
	if wd, err := os.Getwd(); err == nil {
		uiLog.Printf("working dir: %s", wd)
	}

	cfg, err := config.LoadOrDefault()
	if err != nil {
		uiLog.Printf("config load error: %v", err)
	} else {
		uiLog.Printf("config loaded")
	}

	logSvc, logErr := logger.New(cfg)
	if logErr != nil {
		logSvc = logger.NewStderr()
		logSvc.Warn("logger init failed; using stderr")
	}
	defer logSvc.Close()

	uiLog.Printf("building window")
	f, err := newMainForm(cfg, logSvc)
	if err != nil {
		uiLog.Printf("window create error: %v", err)
		uiStartupAlert(err)
		return
	}

	if err != nil {
		f.statusLabel.SetText("Error loading config: " + err.Error())
	}

	uiLog.Printf("run loop")
	f.MainWindow.Run()
	uiLog.Printf("run loop exit")
}
