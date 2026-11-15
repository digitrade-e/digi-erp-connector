package provision

import (
	"bytes"
	"context"
	"io"

	"github.com/digitrade-e/digi-erp-connector/internal/api"
	"github.com/digitrade-e/digi-erp-connector/internal/auth"
	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// AuthInput is the auth command's stdin (Digitrade plan 086 C1): the
// connector's API credential and nothing else. apply restates the database
// credential with every run; this command touches only the auth block of
// config.yaml, so a caller that holds no database credential can set or
// rotate the one erp-manager logs in with.
type AuthInput struct {
	Version   int        `json:"version"` // 1
	ChangeSet string     `json:"changeSet"`
	Auth      AuthChange `json:"auth"`
	// RotateSecret gives the connector a new signing secret (the GUI's
	// "Regenerate"): every token issued until now stops working, and callers
	// log in again on their next 401.
	RotateSecret bool `json:"rotateSecret,omitempty"`
	// AssumeIdle is apply's: go on when the running connector's order queue
	// cannot be read.
	AssumeIdle bool    `json:"assumeIdle,omitempty"`
	Expect     *Expect `json:"expect,omitempty"`
}

// AuthChange is the new credential. Username "" keeps the box's.
type AuthChange struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password"`
}

// DecodeAuth reads one JSON AuthInput, strictly like DecodeSpec.
func DecodeAuth(r io.Reader) (AuthInput, *Refusal) {
	var in AuthInput
	if err := decodeStrict(r, &in); err != nil {
		return AuthInput{}, refuse(CodeSpecInvalid, "send one JSON object with only the documented fields",
			"the auth input does not decode: %v", err)
	}
	return in, nil
}

// Validate checks the input before anything is read: the version, the change
// set, the user name when one is given, and the password with apply's guard.
func (in AuthInput) Validate() *Refusal {
	if in.Version != 1 {
		return refuse(CodeSpecInvalid, `send "version": 1`, "version %d is not supported", in.Version)
	}
	if !changeSetRe.MatchString(in.ChangeSet) {
		return refuse(CodeChangeSetID, "use 8 to 64 letters, digits, - or _", "changeSet is not a valid id")
	}
	if in.Auth.Username != "" && !authUserRe.MatchString(in.Auth.Username) {
		return refuse(CodeSpecInvalid, "use up to 64 letters, digits, . _ @ or -, or leave username out to keep the box's",
			"auth.username is not a valid user name")
	}
	if r := validateAuthPassword(in.Auth.Password); r != nil {
		return r
	}
	return validateExpect(in.Expect)
}

func validateExpect(e *Expect) *Refusal {
	if e != nil && e.ConfigSHA256 != nil && *e.ConfigSHA256 != "" && !sha256HexRe.MatchString(*e.ConfigSHA256) {
		return refuse(CodeSpecInvalid, "send 64 lower-case hex digits, or an empty string for no file", "expect.configSha256 is not a sha256")
	}
	return nil
}

// ApplyAuth sets the connector's API credential (and, on request, a new
// signing secret) in config.yaml and nothing else, with apply's guarantees:
// every check before the first write, the change set's pre-image of
// config.yaml (restore <changeSet> undoes it), the order queue asked before
// the write, the restart only when the file changed (or the running daemon is
// older than the file), and the port proved with the NEW credential —
// /auth/token, /api/ping, /api/health. A failed proof puts config.yaml back
// and the service as it was. check stops before the first write.
func ApplyAuth(ctx context.Context, env Env, in AuthInput, check bool) (*ApplyResult, error) {
	res := &ApplyResult{ChangeSet: in.ChangeSet, Steps: []Step{}}
	if r := in.Validate(); r != nil {
		return res, r
	}
	s := Spec{Version: in.Version, ChangeSet: in.ChangeSet, AssumeIdle: in.AssumeIdle, Expect: in.Expect}
	a := &applier{env: env, s: s, dir: env.Sys.DataDir(), res: res, check: check}
	unlock, err := lockRuns(a.dir)
	if err != nil {
		return res, err
	}
	defer unlock()
	err = a.runAuth(ctx, in)
	a.settle()
	return res, err
}

func (a *applier) runAuth(ctx context.Context, in AuthInput) error {
	if err := a.readCurrent(); err != nil {
		return err
	}
	if a.curBytes == nil {
		return refuse(CodeConfigRefused, "provision the connector with apply first",
			"there is no config.yaml: auth changes a configured connector")
	}
	a.tgt = a.cur
	a.tgt.Auth.Password = in.Auth.Password
	if in.Auth.Username != "" {
		a.tgt.Auth.Username = in.Auth.Username
	}
	if in.RotateSecret {
		secret, err := auth.NewSecret()
		if err != nil {
			return refuse(CodeWriteFailed, "run it again", "generate the signing secret: %v", err)
		}
		a.res.secrets = append(a.res.secrets, secret)
		a.tgt.Auth.Secret = secret
	}
	startCheck := a.tgt
	startCheck.TLS = config.TLSConfig{} // the pair is not this command's: it stays as it is
	if err := api.CheckConfig(startCheck); err != nil {
		return refuse(CodeConfigRefused, "fix config.yaml first: the connector would refuse to start on it", "%v", err)
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
		c.Restart = changed || (a.running && configStale(a.curMod, a.svc.ProcessStart))
		a.res.Check = c
		return nil
	}
	if !changed {
		a.step("config", false, "")
		if !a.running {
			a.step("restart", false, "the service is stopped, and auth with nothing to change does not start it")
			return nil
		}
		if configStale(a.curMod, a.svc.ProcessStart) {
			return a.restartAndProve(ctx) // the daemon runs an older file: bring it onto this one
		}
		return a.proveRunning(ctx)
	}
	if err := a.idle(ctx, "before anything is written"); err != nil {
		return err
	}
	if err := a.env.Sys.ProtectDataDir(); err != nil {
		return refuse(CodeWriteFailed, `see "The service will not start" in docs/troubleshooting.md`,
			"the data directory could not be protected, so nothing was written: %v", err)
	}
	// Only config.yaml: the credential and the signing secret live there, and
	// restore <changeSet> must put back exactly what this command changed.
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

// proveRunning proves the running daemon with the target credential, without
// a restart: a re-run of a command that changed nothing still ends with the
// evidence the caller asked for.
func (a *applier) proveRunning(ctx context.Context) error {
	a.step("restart", false, "nothing the service reads changed")
	proof, err := a.prove(ctx, a.tgt)
	if err != nil {
		return refuse(CodePortProofFailed, "restart the connector (daemon-restart) and run it again", "%v", err)
	}
	a.res.Proof = proof
	if proof.Health != "ok" && proof.Health != HealthNoDatabase {
		a.warn("the connector answers but its database does not (/api/health: %s)", proof.Health)
	}
	return nil
}

func (in AuthInput) secrets() []string { return []string{in.Auth.Password} }
