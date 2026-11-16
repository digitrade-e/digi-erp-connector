package provision

import (
	"context"
	"io"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/api"
	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// DBInput is the db command's stdin (plan 086 C6, G-125 part 1): the
// database the connector logs in to, and nothing else — the API credential is
// not restated, so a caller that does not hold it (or must not change it) can
// move a box off sa, change its host, port or encryption, or turn its
// database off.
type DBInput struct {
	Version   int      `json:"version"` // 1
	ChangeSet string   `json:"changeSet"`
	DB        DBChange `json:"db"`
	// Admin is an administrator's login, used once — to create or take over
	// the least-privilege login and to install the Hasavshevet procedures —
	// and never stored. Behind the link it may be sa; the connector's own
	// login never is.
	Admin      *DBAdmin `json:"admin,omitempty"`
	AssumeIdle bool     `json:"assumeIdle,omitempty"`
	Expect     *Expect  `json:"expect,omitempty"`
}

// DBChange is the database part of apply's spec, with the credential split
// out: with LeastPrivilege the connector logs in as Login, whose password the
// box keeps (an administrator is needed only to create or rotate it); without
// it the connector logs in as User, with Password or, when Password is left
// out and User is the login in use, the stored one.
type DBChange struct {
	// Mode "none" turns the database off (alone: every other field is
	// refused).
	Mode     string `json:"mode,omitempty"`
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
	Database string `json:"database,omitempty"`
	// User and Password: the connector's own login when LeastPrivilege is
	// off. With LeastPrivilege on they are taken as the administrator, as in
	// apply's spec — or send them in Admin, not both.
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
	// Encrypt and TrustServerCertificate: nil keeps the box's. Trust is
	// accepted as another name of TrustServerCertificate (plan 086 §5).
	Encrypt                *bool  `json:"encrypt,omitempty"`
	TrustServerCertificate *bool  `json:"trustServerCertificate,omitempty"`
	Trust                  *bool  `json:"trust,omitempty"`
	AllowSa                bool   `json:"allowSa,omitempty"`
	LeastPrivilege         bool   `json:"leastPrivilege,omitempty"`
	Login                  string `json:"login,omitempty"`
	RotateLogin            bool   `json:"rotateLogin,omitempty"`
}

// DBAdmin is the administrator's login.
type DBAdmin struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

// DecodeDB reads one JSON DBInput, strictly like DecodeSpec.
func DecodeDB(r io.Reader) (DBInput, *Refusal) {
	var in DBInput
	if err := decodeStrict(r, &in); err != nil {
		return DBInput{}, refuse(CodeSpecInvalid, "send one JSON object with only the documented fields",
			"the db input does not decode: %v", err)
	}
	return in, nil
}

// hasAdmin: an administrator came with the input, in admin or (least
// privilege) as db.user/db.password.
func (in DBInput) hasAdmin() bool {
	return in.Admin != nil || (in.DB.LeastPrivilege && (in.DB.User != "" || in.DB.Password != ""))
}

// dbSpec is the input as apply's DBSpec: with least privilege, User and
// Password are the administrator's (empty when none came).
func (in DBInput) dbSpec() DBSpec {
	d := in.DB
	if d.Mode != "" {
		return DBSpec{Mode: d.Mode}
	}
	trust := d.TrustServerCertificate
	if trust == nil {
		trust = d.Trust
	}
	s := DBSpec{Host: d.Host, Port: d.Port, Database: d.Database, User: d.User, Password: d.Password,
		Encrypt: d.Encrypt, TrustServerCertificate: trust, AllowSa: d.AllowSa, LeastPrivilege: d.LeastPrivilege,
		Login: d.Login, RotateLogin: d.RotateLogin}
	if d.LeastPrivilege && in.Admin != nil {
		s.User, s.Password = in.Admin.User, in.Admin.Password
	}
	return s
}

// Validate checks the input before anything is read, with link mode's rules
// when linked: the connector's own login is never one the client gave.
func (in DBInput) Validate(linked bool) *Refusal {
	if in.Version != 1 {
		return refuse(CodeSpecInvalid, `send "version": 1`, "version %d is not supported", in.Version)
	}
	if !changeSetRe.MatchString(in.ChangeSet) {
		return refuse(CodeChangeSetID, "use 8 to 64 letters, digits, - or _", "changeSet is not a valid id")
	}
	if r := validateExpect(in.Expect); r != nil {
		return r
	}
	d := in.DB
	if d.Mode != "" {
		if d.Mode != DBModeNone {
			return refuse(CodeSpecInvalid, `use "mode": "none" to turn the database off, or leave mode out`, "db.mode %q is not known", d.Mode)
		}
		if d != (DBChange{Mode: DBModeNone}) || in.Admin != nil {
			return refuse(CodeSpecInvalid, `with "mode": "none" leave every other db field, and admin, out`, "db.mode none carries other fields")
		}
		return nil
	}
	if d.Trust != nil && d.TrustServerCertificate != nil && *d.Trust != *d.TrustServerCertificate {
		return refuse(CodeSpecInvalid, "send trustServerCertificate once", "db.trust and db.trustServerCertificate disagree")
	}
	if d.LeastPrivilege {
		if in.Admin != nil && (d.User != "" || d.Password != "") {
			return refuse(CodeSpecInvalid, "send the administrator once: in admin, or as db.user and db.password",
				"both admin and db.user/db.password name an administrator")
		}
		if d.AllowSa {
			return refuse(CodeSpecInvalid, "leave allowSa out: with leastPrivilege the connector never keeps the administrator's login",
				"db.allowSa is set with leastPrivilege")
		}
	} else {
		if linked {
			return refuse(CodeSaLinkMode, "set leastPrivilege: true — behind the link the connector keeps a login of its own",
				"behind the link the connector never keeps the login the client gave")
		}
		if in.Admin != nil {
			return refuse(CodeSpecInvalid, "set leastPrivilege, or leave admin out: the given login is the connector's own",
				"admin is set but leastPrivilege is off")
		}
		if d.RotateLogin {
			return refuse(CodeSpecInvalid, "set leastPrivilege, or leave rotateLogin out", "db.rotateLogin is set but leastPrivilege is off")
		}
	}
	s := in.dbSpec()
	if r := validateDBSpec(s, !d.LeastPrivilege || in.hasAdmin(), d.LeastPrivilege && in.hasAdmin()); r != nil {
		return r
	}
	if in.Admin != nil && (strings.TrimSpace(in.Admin.User) == "" || in.Admin.Password == "") {
		return refuse(CodeSpecInvalid, "send the administrator's login and its password", "admin.user or admin.password is missing")
	}
	return nil
}

func (in DBInput) secrets() []string {
	out := []string{in.DB.Password}
	if in.Admin != nil {
		out = append(out, in.Admin.Password)
	}
	return out
}

// ApplyDB changes the database the connector logs in to and nothing else,
// with apply's guarantees: every check before the first write — the spec, the
// drift guard, the daemon's own startup check, the database host on this
// machine or its LAN, the order queue, and the login the connector will use
// opened before anything is written; the change set's pre-image of
// config.yaml and the DB secret (restore <changeSet> undoes it; a
// least-privilege login's own key never goes back: SQL Server knows no older
// password); then the login, the secret, config.yaml, the restart, and the
// proof including /api/health — a database the connector cannot use is a
// failed proof here, and the pre-image goes back. Mode none turns the
// database off: the proof then does not ask /api/health. check stops before
// the first write.
func ApplyDB(ctx context.Context, env Env, in DBInput, check bool) (*ApplyResult, error) {
	res := &ApplyResult{ChangeSet: in.ChangeSet, Steps: []Step{}}
	if r := in.Validate(LinkMode(env.Sys.DataDir())); r != nil {
		return res, r
	}
	s := Spec{Version: in.Version, ChangeSet: in.ChangeSet, DB: in.dbSpec(), AssumeIdle: in.AssumeIdle, Expect: in.Expect}
	a := &applier{env: env, s: s, dir: env.Sys.DataDir(), res: res, check: check}
	unlock, err := lockRuns(a.dir)
	if err != nil {
		return res, err
	}
	defer unlock()
	err = a.runDB(ctx, in)
	a.settle()
	return res, err
}

func (a *applier) runDB(ctx context.Context, in DBInput) error {
	if err := a.readCurrent(); err != nil {
		return err
	}
	if a.curBytes == nil {
		return refuse(CodeConfigRefused, "provision the connector with apply first",
			"there is no config.yaml: db changes a configured connector")
	}
	login := strings.TrimSpace(a.s.DB.User)
	switch {
	case a.s.DB.None():
		login = ""
	case a.s.DB.LeastPrivilege:
		login = a.s.DB.login()
	}
	a.tgt = a.cur
	a.tgt.ImageFolders = append([]string(nil), a.cur.ImageFolders...)
	setDB(&a.tgt, a.s.DB, login)
	startCheck := a.tgt
	startCheck.TLS = config.TLSConfig{} // the pair is not this command's
	if err := api.CheckConfig(startCheck); err != nil {
		return refuse(CodeConfigRefused, "fix the input: the connector would refuse to start on this config", "%v", err)
	}
	if err := a.idle(ctx, "before anything is written"); err != nil {
		return err
	}
	admin, err := a.planDB(ctx, in)
	if admin != nil {
		defer admin.Close()
	}
	if err != nil {
		return err
	}
	if a.check {
		c := a.wouldDo()
		c.Recovery = false // not this command's
		a.res.Check = c
		return nil
	}
	if err := a.env.Sys.ProtectDataDir(); err != nil {
		return refuse(CodeWriteFailed, `see "The service will not start" in docs/troubleshooting.md`,
			"the data directory could not be protected, so nothing was written: %v", err)
	}
	if err := a.captureFiles(configFile, secretFile(a.cur.ERP), secretFile(a.tgt.ERP)); err != nil {
		return err
	}
	for _, f := range []func(context.Context, Database) error{a.procedures, a.applyLogin} {
		if err := f(ctx, admin); err != nil {
			return a.fail(err)
		}
	}
	for _, f := range []func() error{a.writeSecret, a.writeConfig} {
		if err := f(); err != nil {
			return a.fail(err)
		}
	}
	if err := a.env.Sys.ProtectDataDir(); err != nil {
		a.warn("the data directory could not be protected after the writes (%v); the service sets it at its start, and refuses to start if it cannot", err)
	}
	if err := a.restartAndProve(ctx); err != nil {
		return err
	}
	if a.res.Proof == nil && a.running {
		// Nothing the service reads changed: prove the running daemon.
		proof, err := a.prove(ctx, a.tgt)
		if err != nil {
			return refuse(CodePortProofFailed, "restart the connector (daemon-restart) and run it again", "%v", err)
		}
		a.res.Proof = proof
	}
	if p := a.res.Proof; p != nil && p.Health != "ok" && p.Health != HealthNoDatabase {
		cause := refuse(CodePortProofFailed, "check the database, the login and the network, then run it again",
			"the connector answers but not its database (/api/health: %s)", p.Health)
		if a.res.Restarted {
			return a.fail(cause) // this run's change broke it: put the pre-image back
		}
		return cause
	}
	return nil
}

// planDB settles the login before anything is written, and opens the
// administrator when one came (the caller closes it): the given login opened
// with its password (or the stored one), or the least-privilege login kept,
// created or rotated as apply decides it — without an administrator, only
// kept.
func (a *applier) planDB(ctx context.Context, in DBInput) (Database, error) {
	if a.s.DB.None() {
		a.loginPlan = loginNone
		return nil, nil
	}
	if r := checkDBHost(ctx, a.env, a.tgt.DB.Host); r != nil {
		return nil, r
	}
	if !a.s.DB.LeastPrivilege {
		pw := a.s.DB.Password
		if pw == "" {
			if !strings.EqualFold(strings.TrimSpace(a.cur.DB.User), a.tgt.DB.User) {
				return nil, refuse(CodeSpecInvalid, "send db.password: the stored password is "+a.cur.DB.User+"'s",
					"db.user %s is not the login in use, and no password came", a.tgt.DB.User)
			}
			b, err := a.env.Sys.SecretGet(config.DBPasswordKey(a.cur.ERP))
			if err != nil {
				return nil, refuse(CodeSpecInvalid, "send db.password", "no DB password is stored: %v", err)
			}
			pw = string(b)
			a.res.secrets = append(a.res.secrets, pw)
		}
		a.s.DB.Password = pw
		admin, err := a.openAdmin() // the given login is its own administrator, as in apply
		if err != nil {
			return nil, err
		}
		return admin, a.planLogin(ctx, admin)
	}
	var admin Database
	if in.hasAdmin() {
		var err error
		if admin, err = a.openAdmin(); err != nil {
			return nil, err
		}
	}
	return admin, a.planLogin(ctx, admin)
}
