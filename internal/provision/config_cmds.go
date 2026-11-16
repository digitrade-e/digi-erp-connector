package provision

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/api"
	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// Plan 086 C9: the receipts and orders settings of config.yaml, each without
// restating the credential or the database — today a hand edit of config.yaml
// over RDP. Both run options' flow (runConfigChange): every check before the
// first write, the change set's pre-image of config.yaml, the order queue, the
// restart only when the file changed, the proof with the box's own
// credential, and the rollback on a failed proof.

// ReceiptsInput is the receipts command's stdin. Each field left out keeps
// the box's value.
type ReceiptsInput struct {
	Version   int    `json:"version"` // 1
	ChangeSet string `json:"changeSet"`
	// SendReceiptDir is the folder Hashavshevet's receipt importer reads; ""
	// turns receipts off (POST /api/sendReceipt answers 503).
	SendReceiptDir *string `json:"sendReceiptDir,omitempty"`
	// HasReceiptBatFile and HasReceiptParamFile are model (b)'s importer: a
	// BAT, or the orders' has.exe with this parameter file — not both; ""
	// clears one. Both run as LocalSystem once per receipt.
	HasReceiptBatFile   *string `json:"hasReceiptBatFile,omitempty"`
	HasReceiptParamFile *string `json:"hasReceiptParamFile,omitempty"`
	// ReceiptCardNames replaces the whole receiptCardNames block; {} clears it.
	ReceiptCardNames *config.ReceiptCardNamesConfig `json:"receiptCardNames,omitempty"`
	// AllowWritable is apply's importer.allowWritable (WinRM only): it
	// accepts a receipt importer — and a receipt folder (plan 087 review M1)
	// — that accounts besides the administrators can change.
	AllowWritable bool    `json:"allowWritable,omitempty"`
	AssumeIdle    bool    `json:"assumeIdle,omitempty"`
	Expect        *Expect `json:"expect,omitempty"`
}

// OrdersInput is the orders command's stdin. Each field left out keeps the
// box's value.
type OrdersInput struct {
	Version   int    `json:"version"` // 1
	ChangeSet string `json:"changeSet"`
	// SendOrderDir and Importer are apply's (the counter moves with the
	// folder; orders without an importer say {"mode":"none"}).
	SendOrderDir *string       `json:"sendOrderDir,omitempty"`
	Importer     *ImporterSpec `json:"importer,omitempty"`
	// SendOrderPickupDir switches orders to model (a) (DIG-1297): one file per
	// order into this folder, imported by the ERP's scheduler; "" turns it
	// off. SendOrderFormat is official or legacy ("" = official), required
	// with a pickup folder.
	SendOrderPickupDir *string `json:"sendOrderPickupDir,omitempty"`
	SendOrderFormat    *string `json:"sendOrderFormat,omitempty"`
	// AllowWritable accepts, over WinRM only, a sendOrderDir or pickup folder
	// that accounts besides the administrators can write to (plan 087 review
	// M1); the importer keeps importer.allowWritable. Behind the link it is
	// refused.
	AllowWritable bool    `json:"allowWritable,omitempty"`
	AssumeIdle    bool    `json:"assumeIdle,omitempty"`
	Expect        *Expect `json:"expect,omitempty"`
}

// DecodeReceipts and DecodeOrders read one JSON input, strictly like
// DecodeSpec.
func DecodeReceipts(r io.Reader) (ReceiptsInput, *Refusal) {
	var in ReceiptsInput
	if err := decodeStrict(r, &in); err != nil {
		return ReceiptsInput{}, refuse(CodeSpecInvalid, "send one JSON object with only the documented fields", "the receipts input does not decode: %v", err)
	}
	return in, nil
}

func DecodeOrders(r io.Reader) (OrdersInput, *Refusal) {
	var in OrdersInput
	if err := decodeStrict(r, &in); err != nil {
		return OrdersInput{}, refuse(CodeSpecInvalid, "send one JSON object with only the documented fields", "the orders input does not decode: %v", err)
	}
	return in, nil
}

func validateHeader(version int, changeSet string, e *Expect) *Refusal {
	if version != 1 {
		return refuse(CodeSpecInvalid, `send "version": 1`, "version %d is not supported", version)
	}
	if !changeSetRe.MatchString(changeSet) {
		return refuse(CodeChangeSetID, "use 8 to 64 letters, digits, - or _", "changeSet is not a valid id")
	}
	return validateExpect(e)
}

// absOrEmpty refuses a value that is neither "" nor an absolute path, and,
// behind the link, one link mode does not take (linkPathRefusal).
func absOrEmpty(field string, p *string, linked bool) *Refusal {
	if p == nil || strings.TrimSpace(*p) == "" {
		return nil
	}
	v := strings.TrimSpace(*p)
	if hasControl(v) || !filepath.IsAbs(v) {
		return refuse(CodeSpecInvalid, "use an absolute path, or \"\" to clear it", "%s %q is not an absolute path", field, v)
	}
	if linked {
		if why := linkPathRefusal(v); why != "" {
			return refuse(CodeSpecInvalid, "name it by its plain path on a local drive", "%s %q: %s", field, v, why)
		}
	}
	return nil
}

// Validate checks the receipts input before anything is read.
func (in ReceiptsInput) Validate(linked bool, systemDirs []string) *Refusal {
	if r := validateHeader(in.Version, in.ChangeSet, in.Expect); r != nil {
		return r
	}
	if in.SendReceiptDir == nil && in.HasReceiptBatFile == nil && in.HasReceiptParamFile == nil && in.ReceiptCardNames == nil {
		return refuse(CodeSpecInvalid, "name at least one receipt setting", "the input changes nothing")
	}
	for field, p := range map[string]*string{"sendReceiptDir": in.SendReceiptDir, "hasReceiptBatFile": in.HasReceiptBatFile,
		"hasReceiptParamFile": in.HasReceiptParamFile} {
		if r := absOrEmpty(field, p, linked); r != nil {
			return r
		}
	}
	if linked {
		if p := in.SendReceiptDir; p != nil && winUnder(*p, systemDirs) {
			return refuse(CodeSpecInvalid, "name the folder the receipt importer reads, outside the system folders",
				"sendReceiptDir %s is in a system folder", strings.TrimSpace(*p))
		}
		if in.AllowWritable {
			return refuse(CodeImporterNotERP, "leave allowWritable out: behind the link the importer is one only administrators can change",
				"allowWritable is refused behind the link: the receipt importer runs as LocalSystem")
		}
		if p := in.HasReceiptBatFile; p != nil && strings.TrimSpace(*p) != "" {
			ext := strings.ToLower(winExt(*p))
			if (ext != ".bat" && ext != ".cmd") || winUnder(*p, systemDirs) {
				return refuse(CodeImporterNotERP, "name a .bat or .cmd outside the Windows and Program Files folders",
					"hasReceiptBatFile %s is not a batch file outside the system folders", strings.TrimSpace(*p))
			}
		}
	}
	return nil
}

// Validate checks the orders input before anything is read.
func (in OrdersInput) Validate(linked bool, systemDirs []string) *Refusal {
	if r := validateHeader(in.Version, in.ChangeSet, in.Expect); r != nil {
		return r
	}
	if in.SendOrderDir == nil && in.Importer == nil && in.SendOrderPickupDir == nil && in.SendOrderFormat == nil {
		return refuse(CodeSpecInvalid, "name at least one order setting", "the input changes nothing")
	}
	for field, p := range map[string]*string{"sendOrderDir": in.SendOrderDir, "sendOrderPickupDir": in.SendOrderPickupDir} {
		if r := absOrEmpty(field, p, linked); r != nil {
			return r
		}
	}
	if f := in.SendOrderFormat; f != nil {
		switch strings.TrimSpace(*f) {
		case "", config.OrderFormatOfficial, config.OrderFormatLegacy:
		default:
			return refuse(CodeSpecInvalid, `use "official", "legacy", or "" (official)`, "sendOrderFormat %q is not a layout", *f)
		}
	}
	if linked {
		if r := refuseAllowWritableLinked(in.AllowWritable); r != nil {
			return r
		}
		for field, p := range map[string]*string{"sendOrderDir": in.SendOrderDir, "sendOrderPickupDir": in.SendOrderPickupDir} {
			if p != nil && winUnder(*p, systemDirs) {
				return refuse(CodeSpecInvalid, "name a folder outside the system folders", "%s %s is in a system folder", field, strings.TrimSpace(*p))
			}
		}
		return validateImporterLinked(in.Importer, systemDirs)
	}
	return nil
}

// ApplyReceipts sets the receipt settings of config.yaml and nothing else.
func ApplyReceipts(ctx context.Context, env Env, in ReceiptsInput, check bool) (*ApplyResult, error) {
	res := &ApplyResult{ChangeSet: in.ChangeSet, Steps: []Step{}}
	if r := in.Validate(LinkMode(env.Sys.DataDir()), SystemDirs()); r != nil {
		return res, r
	}
	a := &applier{env: env, s: Spec{Version: in.Version, ChangeSet: in.ChangeSet, AssumeIdle: in.AssumeIdle, Expect: in.Expect},
		dir: env.Sys.DataDir(), res: res, check: check}
	unlock, err := lockRuns(a.dir)
	if err != nil {
		return res, err
	}
	defer unlock()
	set := func(t *config.Config) {
		setTrimmed(&t.SendReceiptDir, in.SendReceiptDir)
		setTrimmed(&t.HasReceiptBatFile, in.HasReceiptBatFile)
		setTrimmed(&t.HasReceiptParamFile, in.HasReceiptParamFile)
		if in.ReceiptCardNames != nil {
			t.ReceiptCardNames = *in.ReceiptCardNames
		}
	}
	err = a.runConfigChange(ctx, set, func() error { return a.checkReceipts(in) })
	a.settle()
	return res, err
}

// checkReceipts: receipts are Hasavshevet's; the folder and the importer are
// on this machine; and the importer — run as LocalSystem once per receipt —
// and the receipt folder (review M1: receipts are written there for an
// importer, which may run with it as its working directory) are ones only the
// administrators can change (allowWritable, WinRM only).
func (a *applier) checkReceipts(in ReceiptsInput) error {
	t := a.tgt
	if strings.TrimSpace(t.SendReceiptDir) != "" && t.ERP != config.ERPHasavshevet {
		return refuse(CodeSpecInvalid, "leave sendReceiptDir empty: only Hasavshevet takes receipt files", "sendReceiptDir is set for erp %s", t.ERP)
	}
	// A reserved receipt is finished into the receipt folder in use: it does
	// not move under one (review m5, as the documents command).
	if err := a.documentsIdle(a.cur, t); err != nil {
		return err
	}
	if !t.ReceiptsConfigured() {
		return nil
	}
	// The receipt folder: documents.receipts.folder's path when bound (plan
	// 087), else sendReceiptDir.
	if dir := t.ReceiptDir(); dir != "" {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return refuse(CodeReceiptDirMissing, "create the folder the receipt importer reads", "the receipt folder %s is not a folder on this machine", dir)
		}
		if err := a.checkFolderWriters("the receipt folder", dir, in.AllowWritable); err != nil {
			return err
		}
	}
	runs := []string{t.HasReceiptBatFile}
	if t.HasReceiptParamFile != "" {
		runs = append(runs, t.HasExePath)
	}
	for _, p := range append(runs, t.HasReceiptParamFile) {
		if p == "" {
			continue
		}
		if info, err := os.Stat(p); err != nil || info.IsDir() {
			return refuse(CodeImporterMissing, "name a file that exists", "receipt importer file %s is not on this machine", p)
		}
	}
	for _, p := range runs {
		if p == "" {
			continue
		}
		who, err := a.env.Sys.Writers(p)
		if err != nil {
			return refuse(CodeImporterWritable, "run it elevated (as an administrator)", "read who can change %s: %v", p, err)
		}
		if len(who) == 0 {
			continue
		}
		if in.AllowWritable {
			a.warn("importer_writable: %s can be changed by %s, and the connector runs it as LocalSystem", p, strings.Join(who, ", "))
			continue
		}
		return refuse(CodeImporterWritable, "let only the administrators change it and its folder, or set allowWritable once a human accepted the risk",
			"%s can be changed by %s, and the connector would run it as LocalSystem for every receipt", p, strings.Join(who, ", "))
	}
	return nil
}

// ApplyOrders sets the order settings of config.yaml and nothing else, with
// apply's order checks: the importer stated, its files on this machine and
// changeable by the administrators only, the counter carried to a new
// sendOrderDir at the restart (two different counters refused).
func ApplyOrders(ctx context.Context, env Env, in OrdersInput, check bool) (*ApplyResult, error) {
	res := &ApplyResult{ChangeSet: in.ChangeSet, Steps: []Step{}}
	if r := in.Validate(LinkMode(env.Sys.DataDir()), SystemDirs()); r != nil {
		return res, r
	}
	a := &applier{env: env, s: Spec{Version: in.Version, ChangeSet: in.ChangeSet, SendOrderDir: in.SendOrderDir, Importer: in.Importer,
		AssumeIdle: in.AssumeIdle, Expect: in.Expect}, dir: env.Sys.DataDir(), res: res, check: check}
	unlock, err := lockRuns(a.dir)
	if err != nil {
		return res, err
	}
	defer unlock()
	set := func(t *config.Config) {
		setTrimmed(&t.SendOrderDir, in.SendOrderDir)
		if imp := in.Importer; imp != nil {
			t.HasExePath, t.HasParamFile, t.HasBatFile = "", "", ""
			switch imp.Mode {
			case "hasExe":
				t.HasExePath, t.HasParamFile = strings.TrimSpace(imp.HasExePath), strings.TrimSpace(imp.HasParamFile)
			case "bat":
				t.HasBatFile = strings.TrimSpace(imp.HasBatFile)
			}
		}
		setTrimmed(&t.SendOrderPickupDir, in.SendOrderPickupDir)
		setTrimmed(&t.SendOrderFormat, in.SendOrderFormat)
	}
	err = a.runConfigChange(ctx, set, func() error { return a.checkOrders(in.AllowWritable) })
	a.settle()
	return res, err
}

// checkOrders is apply's order checks, model (a)'s pickup folder on this
// machine, and sendOrderDir and the pickup folder ones only the
// administrators can write to (review M1; allowWritable, WinRM only):
// sendOrderDir holds the order counter and the history the recovery reads,
// and has.exe runs there.
func (a *applier) checkOrders(allowWritable bool) error {
	a.s.ERP = string(a.tgt.ERP)
	if r := a.s.validateOrders(); r != nil {
		return r
	}
	checks := []func() error{a.checkFilesExist, a.checkImporterWriters, a.checkOrderNumbers}
	if !a.tgt.OrdersPickup() { // model (a): the ERP's scheduler imports
		checks = append([]func() error{a.checkImporterStated}, checks...)
	}
	for _, f := range checks {
		if err := f(); err != nil {
			return err
		}
	}
	if d := strings.TrimSpace(a.tgt.SendOrderPickupDir); d != "" && a.tgt.OrdersPickup() {
		if info, err := os.Stat(d); err != nil || !info.IsDir() {
			return refuse(CodeSendOrderDirMissing, "create the folder the ERP's scheduler reads", "sendOrderPickupDir %s is not a folder on this machine", d)
		}
	}
	for _, f := range a.tgt.WriteFolders() {
		if f.Typed || f.Receipts {
			continue // the documents and receipts commands' to check
		}
		if err := a.checkFolderWriters(f.Name, f.Path, allowWritable); err != nil {
			return err
		}
	}
	return nil
}

func setTrimmed(dst *string, v *string) {
	if v != nil {
		*dst = strings.TrimSpace(*v)
	}
}

// runConfigChange is the flow of a command that changes config.yaml alone:
// set applies the input to the target, checks refuse before any write.
func (a *applier) runConfigChange(ctx context.Context, set func(*config.Config), checks ...func() error) error {
	if err := a.readCurrent(); err != nil {
		return err
	}
	if a.curBytes == nil {
		return refuse(CodeConfigRefused, "provision the connector with apply first", "there is no config.yaml: this command changes a configured connector")
	}
	a.tgt = a.cur
	a.tgt.ImageFolders = append([]string(nil), a.cur.ImageFolders...)
	set(&a.tgt)
	startCheck := a.tgt
	startCheck.TLS = config.TLSConfig{} // the pair is not this command's
	if err := api.CheckConfig(startCheck); err != nil {
		return refuse(CodeConfigRefused, "fix the input: the connector would refuse to start on this config", "%v", err)
	}
	for _, f := range checks {
		if err := f(); err != nil {
			return err
		}
	}
	out, err := config.Marshal(a.tgt)
	if err != nil {
		return refuse(CodeWriteFailed, "report it", "%v", err)
	}
	changed := !bytes.Equal(out, a.curBytes)
	if a.check {
		c := &CheckResult{Config: "unchanged", OrderCounter: a.moveOrders}
		if changed {
			c.Config = configDiff(a.cur, a.tgt)
		}
		c.Restart = changed || (a.running && configStale(a.curMod, a.svc.ProcessStart))
		a.res.Check = c
		return nil
	}
	if !changed {
		a.step("config", false, "")
		if !a.running {
			a.step("restart", false, "the service is stopped, and a command with nothing to change does not start it")
			return nil
		}
		return a.restartAndProve(ctx)
	}
	if err := a.idle(ctx, "before anything is written"); err != nil {
		return err
	}
	if err := a.env.Sys.ProtectDataDir(); err != nil {
		return refuse(CodeWriteFailed, `see "The service will not start" in docs/troubleshooting.md`,
			"the data directory could not be protected, so nothing was written: %v", err)
	}
	if err := a.captureFiles(configFile); err != nil {
		return err
	}
	if err := a.writeConfig(); err != nil {
		return a.fail(err)
	}
	if err := a.env.Sys.ProtectDataDir(); err != nil {
		a.warn("the data directory could not be protected after the write (%v); the service sets it at its start, and refuses to start if it cannot", err)
	}
	return a.restartAndProve(ctx)
}
