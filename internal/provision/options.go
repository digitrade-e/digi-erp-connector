package provision

import (
	"bytes"
	"context"
	"io"

	"github.com/digitrade-e/digi-erp-connector/internal/api"
	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// OptionsInput is the options command's stdin: the GUI's remaining settings
// on a box that is already provisioned, and nothing else. It carries no
// credential, so the connection a caller already uses (its username and
// password) is never touched; apply's spec has to restate them.
type OptionsInput struct {
	Version   int         `json:"version"` // 1
	ChangeSet string      `json:"changeSet"`
	Options   OptionsSpec `json:"options"`
	// AssumeIdle is apply's: go on when the running connector's order queue
	// cannot be read.
	AssumeIdle bool    `json:"assumeIdle,omitempty"`
	Expect     *Expect `json:"expect,omitempty"`
}

// DecodeOptions reads one JSON OptionsInput, strictly like DecodeSpec.
func DecodeOptions(r io.Reader) (OptionsInput, *Refusal) {
	var in OptionsInput
	if err := decodeStrict(r, &in); err != nil {
		return OptionsInput{}, refuse(CodeSpecInvalid, "send one JSON object with only the documented fields",
			"the options input does not decode: %v", err)
	}
	return in, nil
}

// Validate checks the input before anything is read: the version, the change
// set, at least one option, and every option's bounds (validateOptions, the
// same as apply's).
func (in OptionsInput) Validate() *Refusal {
	if in.Version != 1 {
		return refuse(CodeSpecInvalid, `send "version": 1`, "version %d is not supported", in.Version)
	}
	if !changeSetRe.MatchString(in.ChangeSet) {
		return refuse(CodeChangeSetID, "use 8 to 64 letters, digits, - or _", "changeSet is not a valid id")
	}
	o := in.Options
	if o.ImageFolders == nil && o.ERPUser == nil && o.Debug == nil && o.QueryTimeoutSeconds == nil &&
		o.QueryMaxRows == nil && o.TokenTTL == nil {
		return refuse(CodeSpecInvalid, "name at least one option", "options is empty")
	}
	return validateOptions(&o)
}

// ApplyOptions sets the given options in config.yaml and nothing else, with
// apply's guarantees: every check before the first write, the change set's
// pre-image (restore <changeSet> undoes it), the restart only when the file
// changed, and the port proved with the box's own credential. A failure after
// the pre-image puts the box back. check stops before the first write.
func ApplyOptions(ctx context.Context, env Env, in OptionsInput, check bool) (*ApplyResult, error) {
	res := &ApplyResult{ChangeSet: in.ChangeSet, Steps: []Step{}}
	if r := in.Validate(); r != nil {
		return res, r
	}
	opts := in.Options
	s := Spec{Version: in.Version, ChangeSet: in.ChangeSet, Options: &opts, AssumeIdle: in.AssumeIdle, Expect: in.Expect}
	a := &applier{env: env, s: s, dir: env.Sys.DataDir(), res: res, check: check}
	unlock, err := lockRuns(a.dir)
	if err != nil {
		return res, err
	}
	defer unlock()
	err = a.runOptions(ctx)
	a.settle()
	return res, err
}

func (a *applier) runOptions(ctx context.Context) error {
	if err := a.readCurrent(); err != nil {
		return err
	}
	if a.curBytes == nil {
		return refuse(CodeConfigRefused, "provision the connector with apply first",
			"there is no config.yaml: options change a configured connector")
	}
	a.tgt = a.cur
	a.tgt.ImageFolders = append([]string(nil), a.cur.ImageFolders...)
	setOptions(&a.tgt, a.s.Options)
	startCheck := a.tgt
	startCheck.TLS = config.TLSConfig{} // the pair is not this command's: it stays as it is
	if err := api.CheckConfig(startCheck); err != nil {
		return refuse(CodeConfigRefused, "fix the options: the connector would refuse to start on this config", "%v", err)
	}
	if err := a.checkImageFolders(); err != nil {
		return err
	}
	out, err := config.Marshal(a.tgt)
	if err != nil {
		return refuse(CodeWriteFailed, "report it", "%v", err)
	}
	changed := !bytes.Equal(out, a.curBytes)
	if a.check {
		c := &CheckResult{Config: "unchanged"}
		if changed {
			c.Config = configDiff(a.cur, a.tgt)
		}
		c.Restart = changed || configStale(a.curMod, a.svc.ProcessStart) || a.svc.State != "running"
		a.res.Check = c
		return nil
	}
	if !changed {
		a.step("config", false, "")
		return a.restartAndProve(ctx)
	}
	if err := a.idle(ctx, "before anything is written"); err != nil {
		return err
	}
	if err := a.env.Sys.ProtectDataDir(); err != nil {
		return refuse(CodeWriteFailed, `see "The service will not start" in docs/troubleshooting.md`,
			"the data directory could not be protected, so nothing was written: %v", err)
	}
	if err := a.capture(); err != nil {
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
