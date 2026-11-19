package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/link"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/atomicfile"
)

// SQL setup (Digitrade plan 090 D5). The client's IT types an SQL Server administrator's login on this machine, once,
// at its console: sql-setup creates the connector's own least-privilege login and Hashavshevet's procedures with it,
// keeps the login's password (DPAPI), and forgets the administrator's. Nobody at Digitrade sees that password, and it
// never crosses the link. Apply then sets the connector up with no administrator at all (adminless): it keeps the login
// this box knows. A person running Digitrade's installer key may only do the latter (ApplyAsInstaller).
//
// It is Hashavshevet's (the only ERP with procedures to install) and the link's: the login is named after the box, so
// it is this box's own and a re-run may give it a new password. It writes no config.yaml: the record below is its
// result, written last, so a run that stopped half-way leaves none and is simply run again.

// sqlSetupFile is the record, in the provision folder.
const sqlSetupFile = "sql-setup.json"

// CodeSQLSetupNeedsLink: sql-setup on a box that is not paired with the link.
const CodeSQLSetupNeedsLink = "sql_setup_needs_link"

// CodeSQLSetupNeedsAdmin: sql-setup in a console that cannot read the connector's data folder (not elevated).
const CodeSQLSetupNeedsAdmin = "sql_setup_needs_admin"

// CodeSQLSetupInService: sql-setup would change the database or the password of a login the running connector uses.
const CodeSQLSetupInService = "sql_setup_in_service"

// CodeSQLSetupConsoleOnly: sql-setup without a person at this machine's console (over WinRM, the link, a pipe).
const CodeSQLSetupConsoleOnly = "sql_setup_console_only"

// CodeDBAdminRequired: an apply without an administrator on a box that knows no login (sql-setup not run, or its
// login no longer logs in).
const CodeDBAdminRequired = "db_admin_required"

// SQLSetupRecord is what sql-setup leaves: the database and the login, never a password.
type SQLSetupRecord struct {
	V                      int    `json:"v"` // 1
	ERP                    string `json:"erp"`
	Host                   string `json:"host"`
	Port                   int    `json:"port,omitempty"`
	Database               string `json:"database"`
	Login                  string `json:"login"`
	Encrypt                bool   `json:"encrypt,omitempty"`
	TrustServerCertificate bool   `json:"trustServerCertificate,omitempty"`
	At                     string `json:"at"` // RFC 3339, UTC
}

// SQLSetupInput is what the person typed.
type SQLSetupInput struct {
	Host                   string // host or host\instance
	Port                   int    // 0 with an instance: the SQL Server Browser finds it
	Database               string
	AdminUser              string
	AdminPassword          string
	Encrypt                bool
	TrustServerCertificate bool
}

// SQLSetupResult is what it did.
type SQLSetupResult struct {
	Login      string `json:"login"`
	LoginDone  string `json:"loginDone"` // created | given a new password | kept
	Procedures string `json:"procedures"`
	Record     string `json:"record"`

	secrets []string // the administrator's password and the login's: what the caller redacts
}

// SQLSetupLogin is the login sql-setup creates for the box paired as box: erp_connector_ and the box id with its
// dashes as underscores. "" when the id does not fit a login's name.
func SQLSetupLogin(box string) string {
	name := "erp_connector_" + strings.ReplaceAll(box, "-", "_")
	if !loginNameRe.MatchString(name) {
		return ""
	}
	return name
}

// ReadSQLSetup reads the record; ok false when there is none. An unreadable record is an error.
func ReadSQLSetup(dataDir string) (SQLSetupRecord, bool, error) {
	b, err := os.ReadFile(filepath.Join(provisionDir(dataDir), sqlSetupFile))
	if errors.Is(err, os.ErrNotExist) {
		return SQLSetupRecord{}, false, nil
	}
	if err != nil {
		return SQLSetupRecord{}, false, err
	}
	var rec SQLSetupRecord
	if err := decodeStrict(strings.NewReader(string(b)), &rec); err != nil {
		return SQLSetupRecord{}, false, fmt.Errorf("%s does not parse: %w", sqlSetupFile, err)
	}
	if rec.V != 1 || rec.ERP != string(config.ERPHasavshevet) || !loginNameRe.MatchString(rec.Login) {
		return SQLSetupRecord{}, false, fmt.Errorf("%s is not a record this tool wrote", sqlSetupFile)
	}
	return rec, true, nil
}

// sqlSetupBox is the box sql-setup names its login after: refused before anything is asked when this console cannot
// read link.yaml (not elevated) or the server is not paired.
func sqlSetupBox(dir string) (login string, err error) {
	st, err := (&link.Store{Dir: dir}).LoadState()
	if err != nil {
		return "", refuse(CodeSQLSetupNeedsAdmin, "open the console with Run as administrator, then run it again",
			"this console cannot read the connector's data folder: %v", err)
	}
	if st.Box == "" {
		return "", refuse(CodeSQLSetupNeedsLink, "pair this server with Digitrade first (the code from the installer), then run it again",
			"this server is not paired with the link: sql-setup names the connector's login after its box")
	}
	if login = SQLSetupLogin(st.Box); login == "" {
		return "", refuse(CodeSQLSetupNeedsLink, "ask Digitrade for a shorter box id", "box id %s does not fit a SQL login's name", st.Box)
	}
	return login, nil
}

// SQLSetup does the work once the person answered: every check before the first write, then the procedures, the
// login, its password kept, and the record last. The result is never nil: its secrets are what the caller redacts.
//
// On a box already in service — config.yaml logs in as this login — it never gives the login a new password (the
// running connector would lose its database): it takes only the server and database the connector uses, and keeps the
// password the box knows. A re-run then only puts the procedures back (a release that needs new ones). Moving the
// connector to another database, or a new password, is the team's db command, which stops the connector first.
func SQLSetup(ctx context.Context, env Env, in SQLSetupInput) (*SQLSetupResult, error) {
	res := &SQLSetupResult{secrets: []string{in.AdminPassword}}
	dir := env.Sys.DataDir()
	login, err := sqlSetupBox(dir)
	if err != nil {
		return res, err
	}
	res.Login = login
	d := DBSpec{Host: strings.TrimSpace(in.Host), Port: in.Port, Database: strings.TrimSpace(in.Database),
		User: strings.TrimSpace(in.AdminUser), Password: in.AdminPassword, LeastPrivilege: true, Login: login}
	if r := validateDBSpec(d, true, true); r != nil {
		return res, r
	}
	unlock, err := lockRuns(dir)
	if err != nil {
		return res, err
	}
	defer unlock()
	cur, inService, err := connectorUses(dir, login)
	if err != nil {
		return res, err
	}
	if inService && (!strings.EqualFold(cur.DB.Host, d.Host) || cur.DB.Port != d.Port || !strings.EqualFold(cur.DB.Database, d.Database)) {
		return res, refuse(CodeSQLSetupInService, "type the server and database the connector uses ("+serverAddress(cur.DB.Host, cur.DB.Port)+", "+cur.DB.Database+"), or ask Digitrade to move it",
			"the connector already logs in as %s to another server or database", login)
	}
	if r := checkDBHost(ctx, env, d.Host); r != nil {
		return res, r
	}
	enc, trust := in.Encrypt, in.TrustServerCertificate
	cfg := config.Config{ERP: config.ERPHasavshevet, DB: config.DBConfig{Driver: "mssql", Host: d.Host, Port: d.Port,
		Database: d.Database, User: d.User, Encrypt: enc, TrustServerCertificate: trust}}
	admin, err := env.OpenDB(cfg, d.Password)
	if err != nil {
		return res, refuse(CodeDBUnreachable, `check the server (host\instance needs the SQL Server Browser; host:port does not), TCP/IP and SQL logins being on, the database, the login and its password`,
			"the database does not answer with that login: %v", err)
	}
	defer func() { _ = admin.Close() }()
	asLogin := cfg
	asLogin.DB.User = login

	// The passwords this box knows for the login: its own key, and the running connector's while it logs in as it.
	var known []string
	if b, err := env.Sys.SecretGet(loginKey(login)); err == nil {
		known = append(known, string(b))
	}
	if inService {
		if b, err := env.Sys.SecretGet(config.DBPasswordKey(cur.ERP)); err == nil {
			known = append(known, string(b))
		}
	}
	res.secrets = append(res.secrets, known...)
	var keep string
	for _, pw := range known {
		if dbc, err := env.OpenDB(asLogin, pw); err == nil {
			_ = dbc.Close()
			keep = pw
			break
		}
	}
	if inService && keep == "" {
		return res, refuse(CodeSQLSetupInService, "ask Digitrade: they give the login a new password with the db command, which stops the connector first",
			"the connector logs in as %s, and no password this box keeps logs in as it now", login)
	}
	exists := keep != ""
	if !exists {
		if exists, err = admin.LoginExists(ctx, login); err != nil {
			return res, refuse(CodeLeastPrivilege, "use a login that is sysadmin, or securityadmin with db_owner on the database",
				"look for %s: %v", login, err)
		}
	}
	if err := env.Sys.ProtectDataDir(); err != nil {
		return res, refuse(CodeWriteFailed, "run it as an administrator", "the data directory could not be protected, so nothing was written: %v", err)
	}
	procs, err := admin.EnsureProcedures(ctx)
	if err != nil {
		return res, refuse(CodeProceduresFailed, "the login needs CREATE PROCEDURE in the database", "the Hashavshevet procedures: %v", err)
	}
	res.Procedures = "present"
	var made []string
	for _, p := range procs {
		if p.Created {
			made = append(made, p.Name)
		}
	}
	if len(made) > 0 {
		res.Procedures = "created " + strings.Join(made, ", ")
	}
	if keep != "" {
		// Nothing is sent to SQL Server for a kept login: setting the same password again can be refused by a
		// password-history policy, and apply's keep does not either.
		res.LoginDone = "kept"
	} else {
		res.LoginDone = "created"
		if exists {
			res.LoginDone = "given a new password"
		}
		pw, err := NewSQLPassword()
		if err != nil {
			return res, refuse(CodeLeastPrivilege, "run it again", "%v", err)
		}
		res.secrets = append(res.secrets, pw)
		// Kept before SQL Server hears it: a password set there but lost here could not be taken back.
		if err := env.Sys.SecretSet(loginKey(login), []byte(pw)); err != nil {
			return res, refuse(CodeWriteFailed, "run it as an administrator", "keep %s's password: %v", login, err)
		}
		if err := admin.EnsureLeastPrivilegeLogin(ctx, login, d.Database, pw); err != nil {
			return res, refuse(CodeLeastPrivilege, "use a login that is sysadmin, or securityadmin with db_owner on the database",
				"set up %s: %v", login, err)
		}
		dbc, err := env.OpenDB(asLogin, pw)
		if err != nil {
			return res, refuse(CodeLeastPrivilege, "check the server allows SQL logins (mixed mode)", "%s was set up but does not log in: %v", login, err)
		}
		_ = dbc.Close()
	}

	rec := SQLSetupRecord{V: 1, ERP: string(config.ERPHasavshevet), Host: d.Host, Port: d.Port, Database: d.Database,
		Login: login, Encrypt: enc, TrustServerCertificate: trust, At: env.Now().UTC().Format("2006-01-02T15:04:05Z")}
	b, _ := json.MarshalIndent(rec, "", "  ")
	path := filepath.Join(provisionDir(dir), sqlSetupFile)
	if err := atomicfile.Write(path, append(b, '\n'), 0o600); err != nil {
		return res, refuse(CodeWriteFailed, "run it as an administrator", "write %s: %v", sqlSetupFile, err)
	}
	res.Record = path
	return res, nil
}

// connectorUses reads config.yaml: the running connector's settings, and whether it logs in as login. No file: not in
// service. A file that cannot be read or parsed is a refusal: sql-setup will not guess whether a connector depends on
// the login.
func connectorUses(dir, login string) (config.Config, bool, error) {
	b, err := os.ReadFile(filepath.Join(dir, configFile))
	if errors.Is(err, os.ErrNotExist) {
		return config.Config{}, false, nil
	}
	if err == nil {
		var cfg config.Config
		if cfg, err = config.Parse(b); err == nil {
			return cfg, strings.EqualFold(strings.TrimSpace(cfg.DB.User), login), nil
		}
	}
	return config.Config{}, false, refuse(CodeConfigRefused, "ask Digitrade to look at the connector's config.yaml first",
		"config.yaml cannot be read: %v", err)
}

// adminRequired turns planLogin's refusal on an adminless apply into the one that says what to do.
func adminRequired(err error, login string) error {
	var r *Refusal
	if errors.As(err, &r) && r.Code == CodeLeastPrivilege {
		return refuse(CodeDBAdminRequired, "have the client's IT run sql-setup on this server (it makes the connector's login), or send db.user and db.password",
			"no administrator came: %s", r.Message)
	}
	return err
}

// The installer's refusals (plan 090 D3), as the box's link names them.
const (
	CodeInstallerNotAllowed  = "installer_not_allowed"
	CodeInstallerConfigured  = "installer_box_configured"
	CodeInstallerDC          = "installer_dc"
	CodeInstallerRoleUnknown = "installer_role_unknown"
)

// ApplyAsInstaller is apply for a command an installer signed (the link passes --installer; plan 090 D3): the first
// setup of a fresh box, from a short list of fields, with the database sql-setup recorded and no administrator. It
// refuses a domain controller, a box sql-setup has not set up, and — under the run lock — a box whose config.yaml
// already holds an API credential. Whatever else a box needs (orders, receipts, folders, TLS, options) is the team's,
// later.
func ApplyAsInstaller(ctx context.Context, env Env, s Spec, check bool) (*ApplyResult, error) {
	res := &ApplyResult{ChangeSet: s.ChangeSet, Steps: []Step{}}
	if r := s.installerRules(); r != nil {
		return res, r
	}
	host, err := env.Sys.Host()
	if err != nil {
		return res, refuse(CodeInstallerRoleUnknown, "ask the team to set this server up", "this server's domain role could not be read: %v", err)
	}
	if host.DomainController() {
		return res, refuse(CodeInstallerDC, "ask the team to set this server up", "this server is a domain controller (role %d)", host.DomainRole)
	}
	rec, ok, err := ReadSQLSetup(env.Sys.DataDir())
	if err != nil {
		return res, refuse(CodeDBAdminRequired, "have the client's IT run sql-setup on this server again", "%v", err)
	}
	if !ok {
		return res, refuse(CodeDBAdminRequired, "have the client's IT run sql-setup on this server first: it creates the connector's login",
			"sql-setup has not been run on this server")
	}
	if r := s.fillFromSQLSetup(rec); r != nil {
		return res, r
	}
	return applyAs(ctx, env, s, check, true)
}

// installerRules is what an installer's first setup may name: version, changeSet, erp (hashavshevet), apiListen,
// auth, service, expect, importer {"mode":"none"}, and db.leastPrivilege with, optionally, db.database and db.login as
// sql-setup recorded them. Nothing else.
func (s Spec) installerRules() *Refusal {
	no := func(field string) *Refusal {
		return refuse(CodeInstallerNotAllowed, "leave "+field+" out: the team sets it after the first setup",
			"an installer's first setup may not set %s", field)
	}
	switch {
	case s.ERP != "" && s.ERP != string(config.ERPHasavshevet):
		return no("an ERP other than hasavshevet")
	case s.TLS != nil:
		return no("tls")
	case s.SendOrderDir != nil:
		return no("sendOrderDir")
	case s.Firewall != nil:
		return no("firewall")
	case s.Options != nil:
		return no("options")
	case s.AssumeIdle:
		return no("assumeIdle")
	case s.Importer != nil && *s.Importer != (ImporterSpec{Mode: "none"}):
		return no("an importer")
	}
	d := s.DB
	if d.Mode != "" || d.Host != "" || d.Port != 0 || d.User != "" || d.Password != "" || d.Encrypt != nil ||
		d.TrustServerCertificate != nil || d.AllowSa || d.RotateLogin {
		return refuse(CodeInstallerNotAllowed, "send only db.leastPrivilege (and db.database, db.login as sql-setup recorded them)",
			"an installer's first setup takes the database from sql-setup: it may not name a server, a credential, encryption or a rotation")
	}
	if !d.LeastPrivilege {
		return refuse(CodeInstallerNotAllowed, "set db.leastPrivilege: true", "an installer's first setup uses the login sql-setup made")
	}
	// The allow-list itself: the spec rebuilt from the fields an installer may send must be the spec. The cases above
	// name the field; this one also catches a field Spec gains later that nobody added to them.
	allowed := Spec{Version: s.Version, ChangeSet: s.ChangeSet, ERP: s.ERP, APIListen: s.APIListen, Auth: s.Auth,
		Service: s.Service, Expect: s.Expect, Importer: s.Importer,
		DB: DBSpec{LeastPrivilege: d.LeastPrivilege, Database: d.Database, Login: d.Login}}
	if !reflect.DeepEqual(allowed, s) {
		return refuse(CodeInstallerNotAllowed, "send only the first setup's fields (provisioning.md: An installer's first setup)",
			"the spec sets a field outside an installer's first setup")
	}
	return nil
}

// fillFromSQLSetup completes an installer's spec from the record: the server, the database, the login and its
// encryption settings. A database or login that differs from the record is refused, not replaced.
func (s *Spec) fillFromSQLSetup(rec SQLSetupRecord) *Refusal {
	return s.fillFromRecord(rec, CodeInstallerNotAllowed)
}

// fillFromRecord is fillFromSQLSetup with the refusal's code: an installer's (installer_not_allowed) or the team's
// apply that names no server (spec_invalid, plan 090 D7). An ERP other than the record's is refused too.
func (s *Spec) fillFromRecord(rec SQLSetupRecord, code string) *Refusal {
	if s.DB.Database != "" && s.DB.Database != rec.Database {
		return refuse(code, "send the database sql-setup recorded ("+rec.Database+"), or leave it out",
			"db.database %q is not the one sql-setup set up", s.DB.Database)
	}
	if s.DB.Login != "" && s.DB.Login != rec.Login {
		return refuse(code, "send the login sql-setup made ("+rec.Login+"), or leave it out",
			"db.login %q is not the one sql-setup made", s.DB.Login)
	}
	if s.ERP != "" && !strings.EqualFold(s.ERP, string(rec.ERP)) {
		return refuse(code, "send the ERP sql-setup set up ("+string(rec.ERP)+"), or leave it out", "erp %q is not the one sql-setup set up", s.ERP)
	}
	s.ERP = rec.ERP
	enc, trust := rec.Encrypt, rec.TrustServerCertificate
	s.DB = DBSpec{Host: rec.Host, Port: rec.Port, Database: rec.Database, Login: rec.Login, LeastPrivilege: true,
		Encrypt: &enc, TrustServerCertificate: &trust}
	return nil
}

// serverAddress is how sql-setup's console takes a server back: host:port when there is a port, else the host (or
// host\instance).
func serverAddress(host string, port int) string {
	if port > 0 && !strings.Contains(host, `\`) {
		return host + ":" + strconv.Itoa(port)
	}
	return host
}
