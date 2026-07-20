//go:build windows

package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/email"
	"github.com/digitrade-e/digi-erp-connector/internal/logger"
	"github.com/digitrade-e/digi-erp-connector/internal/pdf"
	"github.com/digitrade-e/digi-erp-connector/internal/secrets"
)

// showPDFSettingsDialog opens a separate window for PDF, Print, and Email configuration.
// Changes are saved directly to config when the user clicks Save.
func showPDFSettingsDialog(owner walk.Form, cfg *config.Config, logSvc logger.LoggerService) {
	var dlg *walk.Dialog
	var statusLabel *walk.Label

	// PDF engine fields
	var chromePathEdit, sumatraPathEdit, printerNameEdit *walk.LineEdit
	var printAfterOrderCheck *walk.CheckBox

	// Email fields
	var emailAfterOrderCheck *walk.CheckBox
	var smtpHostEdit, smtpPortEdit, smtpUserEdit, smtpPassEdit *walk.LineEdit
	var smtpFromEdit *walk.LineEdit
	var smtpTLSCheck *walk.CheckBox

	// Remote template fields
	var remoteBaseURLEdit, remoteTimeoutEdit, remoteTestDocTypeEdit, remoteTestDocNumberEdit, remoteTestUserExtIDEdit *walk.LineEdit
	var remoteTokensEdit *walk.TextEdit
	var useRemoteTemplateCheck *walk.CheckBox

	setStatus := func(text string) {
		if statusLabel != nil {
			statusLabel.SetText(text)
		}
	}

	err := (Dialog{
		AssignTo: &dlg,
		Title:    "PDF & Email Settings",
		MinSize:  Size{Width: 500, Height: 600},
		Size:     Size{Width: 520, Height: 700},
		Layout:   VBox{},
		Children: []Widget{
			ScrollView{
				Layout: VBox{},
				Children: []Widget{
					// Branding (company name, logo, footer) lives in the
					// backend AppSettings now — admin configures it there.

					// ── PDF Engine ───────────────────────────────────
					Label{Text: "PDF Engine", Font: Font{Bold: true}},
					Label{Text: "Chrome Path"},
					Composite{
						Layout: HBox{MarginsZero: true},
						Children: []Widget{
							LineEdit{AssignTo: &chromePathEdit, CueBanner: "Auto-detected if empty"},
							PushButton{Text: "Browse...", OnClicked: func() {
								fd := &walk.FileDialog{
									Title:  "Select Chrome/Chromium",
									Filter: "Executable (*.exe)|*.exe|All Files (*.*)|*.*",
								}
								if ok, err := fd.ShowOpen(dlg); err != nil {
									setStatus("Error: " + err.Error())
								} else if ok {
									chromePathEdit.SetText(fd.FilePath)
								}
							}},
							PushButton{Text: "Auto-detect", OnClicked: func() {
								p := pdf.DetectChrome()
								if p == "" {
									setStatus("Chrome not found on this system")
								} else {
									chromePathEdit.SetText(p)
									setStatus("Chrome found: " + p)
								}
							}},
						},
					},
					Label{Text: "SumatraPDF Path"},
					Composite{
						Layout: HBox{MarginsZero: true},
						Children: []Widget{
							LineEdit{AssignTo: &sumatraPathEdit, CueBanner: "Auto-detected if empty"},
							PushButton{Text: "Browse...", OnClicked: func() {
								fd := &walk.FileDialog{
									Title:  "Select SumatraPDF",
									Filter: "Executable (*.exe)|*.exe|All Files (*.*)|*.*",
								}
								if ok, err := fd.ShowOpen(dlg); err != nil {
									setStatus("Error: " + err.Error())
								} else if ok {
									sumatraPathEdit.SetText(fd.FilePath)
								}
							}},
						},
					},

					// ── Print Settings ───────────────────────────────
					HSeparator{},
					Label{Text: "Print Settings", Font: Font{Bold: true}},
					CheckBox{AssignTo: &printAfterOrderCheck, Text: "Print PDF after send order"},
					Label{Text: "Printer Name (empty = default)"},
					LineEdit{AssignTo: &printerNameEdit, CueBanner: "Leave empty for default printer"},

					// ── Email Settings ───────────────────────────────
					HSeparator{},
					Label{Text: "Email Settings", Font: Font{Bold: true}},
					CheckBox{AssignTo: &emailAfterOrderCheck, Text: "Send PDF by email after send order"},
					Label{Text: "SMTP Host"},
					LineEdit{AssignTo: &smtpHostEdit},
					Label{Text: "SMTP Port"},
					LineEdit{AssignTo: &smtpPortEdit, CueBanner: "587"},
					Label{Text: "SMTP User"},
					LineEdit{AssignTo: &smtpUserEdit},
					Label{Text: "SMTP Password"},
					LineEdit{AssignTo: &smtpPassEdit, PasswordMode: true, CueBanner: "Leave blank to keep existing"},
					Label{Text: "From Address"},
					LineEdit{AssignTo: &smtpFromEdit},
					CheckBox{AssignTo: &smtpTLSCheck, Text: "Use TLS"},
					PushButton{Text: "Test Email", OnClicked: func() {
						setStatus("Sending test email...")
						go func() {
							// Plain SMTP smoke test — no PDF attachment now that the
							// connector no longer carries a local invoice template.
							smtpPass := smtpPassEdit.Text()
							if smtpPass == "" {
								if stored, err := secrets.Get("smtp_password"); err == nil {
									smtpPass = string(stored)
								}
							}
							smtpPort, _ := strconv.Atoi(smtpPortEdit.Text())
							if smtpPort <= 0 || smtpPort > 65535 {
								smtpPort = 587
							}
							smtpCfg := config.SMTPConfig{
								Host:        strings.TrimSpace(smtpHostEdit.Text()),
								Port:        smtpPort,
								User:        strings.TrimSpace(smtpUserEdit.Text()),
								FromAddress: strings.TrimSpace(smtpFromEdit.Text()),
								UseTLS:      smtpTLSCheck.Checked(),
							}
							sender := email.NewSender(smtpCfg, smtpPass)
							ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
							defer cancel()
							err := sender.SendTestEmail(ctx, nil)
							dlg.Synchronize(func() {
								if err != nil {
									setStatus("Email test failed: " + err.Error())
								} else {
									setStatus("Test email sent to " + smtpCfg.FromAddress)
								}
							})
						}()
					}},

					// ── Remote Template (mission 022) ────────────────
					HSeparator{},
					Label{Text: "Remote PDF Template", Font: Font{Bold: true}},
					Label{Text: "Backend Base URL (e.g. https://api.example.com — no /api suffix)"},
					LineEdit{AssignTo: &remoteBaseURLEdit, CueBanner: "https://api.example.com"},
					CheckBox{AssignTo: &useRemoteTemplateCheck, Text: "Use remote template (admin-customized PDF)"},
					Label{Text: "Timeout (seconds, default 15)"},
					LineEdit{AssignTo: &remoteTimeoutEdit, CueBanner: "15"},
					Label{Text: "Tokens — one per line, format: documentType=token (e.g. order=abc123…). Paste only the token (no key) to use it for ALL document types."},
					TextEdit{AssignTo: &remoteTokensEdit, MinSize: Size{Height: 80}, VScroll: true},
					Label{Text: "Test fetch — documentType / documentNumber / userExtId"},
					Composite{
						Layout: HBox{MarginsZero: true},
						Children: []Widget{
							LineEdit{AssignTo: &remoteTestDocTypeEdit, CueBanner: "order"},
							LineEdit{AssignTo: &remoteTestDocNumberEdit, CueBanner: "ORD-1"},
							LineEdit{AssignTo: &remoteTestUserExtIDEdit, CueBanner: "USR-9"},
							PushButton{Text: "Test fetch", OnClicked: func() {
								setStatus("Testing remote fetch...")
								go func() {
									baseURL := strings.TrimSpace(remoteBaseURLEdit.Text())
									rawTokens := remoteTokensEdit.Text()
									tokens := parseRemoteTokens(rawTokens)
									docType := strings.TrimSpace(remoteTestDocTypeEdit.Text())
									if docType == "" {
										docType = "order"
									}
									token := lookupTokenCaseInsensitive(tokens, docType)
									if token == "" {
										// Tolerant fallback: if the textarea contains a single
										// non-empty line with no `=`, treat it as the token for
										// any document type. Lets the operator paste only the
										// token without remembering the `documentType=` prefix.
										token = inferBareToken(rawTokens)
									}
									if token == "" {
										parsedKeys := make([]string, 0, len(tokens))
										for k := range tokens {
											parsedKeys = append(parsedKeys, k)
										}
										msg := fmt.Sprintf(
											"No token for documentType=%s. Parsed %d entries (keys: %v). Format must be `documentType=token` per line, or paste only the token.",
											docType, len(tokens), parsedKeys,
										)
										dlg.Synchronize(func() { setStatus(msg) })
										return
									}
									timeoutSecs, _ := strconv.Atoi(remoteTimeoutEdit.Text())
									if timeoutSecs <= 0 {
										timeoutSecs = 15
									}
									fetcher := pdf.NewRemoteFetcher(baseURL, time.Duration(timeoutSecs)*time.Second, "digi-erp-connector/test")
									ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSecs+5)*time.Second)
									defer cancel()
									body, err := fetcher.Fetch(ctx,
										token, docType,
										strings.TrimSpace(remoteTestDocNumberEdit.Text()),
										strings.TrimSpace(remoteTestUserExtIDEdit.Text()),
									)
									dlg.Synchronize(func() {
										if err != nil {
											setStatus("Test fetch failed (token=" + pdf.MaskToken(token) + "): " + err.Error())
											return
										}
										preview := string(body)
										if len(preview) > 200 {
											preview = preview[:200] + "..."
										}
										setStatus(fmt.Sprintf("Test fetch OK (%d bytes, token=%s) — preview: %s", len(body), pdf.MaskToken(token), preview))
									})
								}()
							}},
						},
					},

					// ── Status + Save ────────────────────────────────
					HSeparator{},
					Label{AssignTo: &statusLabel},
				},
			},
			// Save and Close buttons at the bottom, outside ScrollView
			Composite{
				Layout: HBox{},
				Children: []Widget{
					HSpacer{},
					PushButton{Text: "Save", OnClicked: func() {
						// Read all fields into config
						cfg.PDF.ChromePath = strings.TrimSpace(chromePathEdit.Text())
						cfg.PDF.SumatraPDFPath = strings.TrimSpace(sumatraPathEdit.Text())
						cfg.PDF.PrintAfterOrder = printAfterOrderCheck.Checked()
						cfg.PDF.PrinterName = strings.TrimSpace(printerNameEdit.Text())
						cfg.PDF.EmailAfterOrder = emailAfterOrderCheck.Checked()
						cfg.SMTP.Host = strings.TrimSpace(smtpHostEdit.Text())
						smtpPort, _ := strconv.Atoi(smtpPortEdit.Text())
						if smtpPort <= 0 || smtpPort > 65535 {
							smtpPort = 587
						}
						cfg.SMTP.Port = smtpPort
						cfg.SMTP.User = strings.TrimSpace(smtpUserEdit.Text())
						cfg.SMTP.FromAddress = strings.TrimSpace(smtpFromEdit.Text())
						cfg.SMTP.UseTLS = smtpTLSCheck.Checked()

						cfg.PDF.RemoteTemplateBaseURL = strings.TrimSpace(remoteBaseURLEdit.Text())
						cfg.PDF.UseRemoteTemplate = useRemoteTemplateCheck.Checked()
						remoteTimeoutSecs, _ := strconv.Atoi(strings.TrimSpace(remoteTimeoutEdit.Text()))
						if remoteTimeoutSecs > 0 {
							cfg.PDF.RemoteTimeoutSeconds = remoteTimeoutSecs
						}
						cfg.PDF.RemoteTokens = parseRemoteTokens(remoteTokensEdit.Text())

						setStatus("Saving...")
						go func() {
							// Save SMTP password
							smtpPass := smtpPassEdit.Text()
							if smtpPass != "" {
								if err := secrets.Set("smtp_password", []byte(smtpPass)); err != nil {
									if logSvc != nil {
										logSvc.Error("PDF settings: failed to save SMTP password", err)
									}
									dlg.Synchronize(func() { setStatus("Failed to save SMTP password: " + err.Error()) })
									return
								}
							}
							// Save config
							if err := config.Save(*cfg); err != nil {
								if logSvc != nil {
									logSvc.Error("PDF settings: failed to save config", err)
								}
								dlg.Synchronize(func() { setStatus("Failed to save config: " + err.Error()) })
								return
							}
							if logSvc != nil {
								logSvc.Info(fmt.Sprintf(
									"PDF settings saved: PrintAfterOrder=%v EmailAfterOrder=%v UseRemoteTemplate=%v RemoteTemplateBaseURL=%q tokenCount=%d ChromePath=%q SumatraPDFPath=%q PrinterName=%q — RESTART digi-erp-connectord daemon for changes to take effect",
									cfg.PDF.PrintAfterOrder, cfg.PDF.EmailAfterOrder, cfg.PDF.UseRemoteTemplate,
									cfg.PDF.RemoteTemplateBaseURL, len(cfg.PDF.RemoteTokens),
									cfg.PDF.ChromePath, cfg.PDF.SumatraPDFPath, cfg.PDF.PrinterName,
								))
							}
							dlg.Synchronize(func() { setStatus("נשמר בהצלחה. הפעל מחדש את digi-erp-connectord (restart the daemon) כדי שהשינויים ייכנסו לתוקף.") })
						}()
					}},
					PushButton{Text: "Close", OnClicked: func() {
						dlg.Accept()
					}},
				},
			},
		},
	}).Create(owner)

	if err != nil {
		walk.MsgBox(owner, "Error", "Failed to create PDF settings window: "+err.Error(), walk.MsgBoxIconError)
		return
	}

	// Populate fields from current config
	chromePathEdit.SetText(cfg.PDF.ChromePath)
	sumatraPathEdit.SetText(cfg.PDF.SumatraPDFPath)
	printAfterOrderCheck.SetChecked(cfg.PDF.PrintAfterOrder)
	printerNameEdit.SetText(cfg.PDF.PrinterName)
	emailAfterOrderCheck.SetChecked(cfg.PDF.EmailAfterOrder)
	smtpHostEdit.SetText(cfg.SMTP.Host)
	smtpPortEdit.SetText(strconv.Itoa(cfg.SMTP.Port))
	smtpUserEdit.SetText(cfg.SMTP.User)
	smtpFromEdit.SetText(cfg.SMTP.FromAddress)
	smtpTLSCheck.SetChecked(cfg.SMTP.UseTLS)

	remoteBaseURLEdit.SetText(cfg.PDF.RemoteTemplateBaseURL)
	useRemoteTemplateCheck.SetChecked(cfg.PDF.UseRemoteTemplate)
	if cfg.PDF.RemoteTimeoutSeconds > 0 {
		remoteTimeoutEdit.SetText(strconv.Itoa(cfg.PDF.RemoteTimeoutSeconds))
	}
	remoteTokensEdit.SetText(formatRemoteTokens(cfg.PDF.RemoteTokens))

	dlg.Run()
}

// parseRemoteTokens parses a multi-line "documentType=token" textarea into a
// map. Whitespace is trimmed; comment lines (# or //) and blank lines are
// skipped; duplicate documentTypes — last entry wins.
func parseRemoteTokens(text string) map[string]string {
	out := map[string]string{}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:eq])
		v := strings.TrimSpace(line[eq+1:])
		if k == "" || v == "" {
			continue
		}
		out[k] = v
	}
	return out
}

// lookupTokenCaseInsensitive returns the token for documentType, trying the
// raw key, lowercase, and uppercase forms — matches the runtime hook's
// lookupRemoteToken behavior so Test fetch and AfterOrder agree.
func lookupTokenCaseInsensitive(tokens map[string]string, documentType string) string {
	if len(tokens) == 0 || documentType == "" {
		return ""
	}
	if t, ok := tokens[documentType]; ok && t != "" {
		return t
	}
	lower := strings.ToLower(documentType)
	upper := strings.ToUpper(documentType)
	if t, ok := tokens[lower]; ok && t != "" {
		return t
	}
	if t, ok := tokens[upper]; ok && t != "" {
		return t
	}
	for k, v := range tokens {
		if strings.EqualFold(k, documentType) && v != "" {
			return v
		}
	}
	return ""
}

// inferBareToken returns the only non-comment, non-empty line in `text` if
// that line contains no `=`. This lets operators paste a single token without
// remembering the `documentType=` key. Returns "" when the textarea has zero
// or multiple bare lines, or when any line already has a key=value form.
func inferBareToken(text string) string {
	var bare string
	count := 0
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		if strings.ContainsRune(line, '=') {
			return "" // structured input present — do not guess
		}
		bare = line
		count++
	}
	if count == 1 {
		return bare
	}
	return ""
}

// formatRemoteTokens renders the in-memory map back to "documentType=token"
// lines for display in the textarea.
func formatRemoteTokens(tokens map[string]string) string {
	if len(tokens) == 0 {
		return ""
	}
	keys := make([]string, 0, len(tokens))
	for k := range tokens {
		keys = append(keys, k)
	}
	// stable order without importing sort: keys already random in map iteration,
	// so do a tiny insertion sort to keep the output deterministic across saves.
	for i := 1; i < len(keys); i++ {
		j := i
		for j > 0 && keys[j-1] > keys[j] {
			keys[j-1], keys[j] = keys[j], keys[j-1]
			j--
		}
	}
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(tokens[k])
		b.WriteByte('\n')
	}
	return b.String()
}
