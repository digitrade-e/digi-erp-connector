package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

const testBoxLogin = "erp_connector_acme_erp"

// freshLinkedBox is a server right after setup and pairing (plan 090): the services installed, the daemon stopped
// (it has no config.yaml), link.yaml naming the box, one SQL Server instance.
func freshLinkedBox(t *testing.T) (*fakeSys, *fakeDB) {
	t.Helper()
	sys := newFakeSys(t, newFakeDaemon(t))
	sys.svc.State, sys.svc.ProcessID = "stopped", 0
	sys.instances = []SQLInstance{{Name: "WIZSOFT", Service: "MSSQL$WIZSOFT", TCPEnabled: true, MixedAuth: true}}
	if err := os.WriteFile(filepath.Join(sys.dir, "link.yaml"), []byte("box: acme-erp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return sys, &fakeDB{saPass: testSaPass}
}

func sqlSetupInput() SQLSetupInput {
	return SQLSetupInput{Host: `127.0.0.1\WIZSOFT`, Database: "erp_db", AdminUser: "sa", AdminPassword: testSaPass}
}

func refusalCode(err error) string {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Code
	}
	if err != nil {
		return "other: " + err.Error()
	}
	return ""
}

func TestSQLSetupMakesTheLoginAndTheRecord(t *testing.T) {
	sys, fdb := freshLinkedBox(t)
	res, err := SQLSetup(context.Background(), testEnv(sys, fdb), sqlSetupInput())
	if err != nil {
		t.Fatal(err)
	}
	if res.Login != testBoxLogin || res.LoginDone != "created" || !strings.HasPrefix(res.Procedures, "created") {
		t.Fatalf("result %+v", res)
	}
	stored, err := sys.SecretGet(loginKey(testBoxLogin))
	if err != nil {
		t.Fatal(err)
	}
	if onServer, ok := fdb.login(testBoxLogin); !ok || onServer != string(stored) {
		t.Fatal("the login's password on the server is not the one the box keeps")
	}
	rec, ok, err := ReadSQLSetup(sys.dir)
	if err != nil || !ok || rec.Login != testBoxLogin || rec.Host != `127.0.0.1\WIZSOFT` || rec.Database != "erp_db" || rec.At == "" {
		t.Fatalf("record %+v %v %v", rec, ok, err)
	}
	raw, _ := os.ReadFile(filepath.Join(sys.dir, "provision", sqlSetupFile))
	if bytes.Contains(raw, []byte(testSaPass)) || bytes.Contains(raw, stored) {
		t.Fatal("the record holds a password")
	}
	if _, err := os.Stat(filepath.Join(sys.dir, configFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("sql-setup wrote config.yaml")
	}
	if _, err := sys.SecretGet(config.DBPasswordKey(config.ERPHasavshevet)); err == nil {
		t.Fatal("sql-setup stored the daemon's DB password: apply does that")
	}
	for _, c := range sys.calls {
		if strings.Contains(c, "sa") && strings.HasPrefix(c, "secret:") {
			t.Fatalf("something of the administrator was kept: %s", c)
		}
	}
}

func TestSQLSetupAgainKeepsOrRenewsTheLogin(t *testing.T) {
	sys, fdb := freshLinkedBox(t)
	env := testEnv(sys, fdb)
	if _, err := SQLSetup(context.Background(), env, sqlSetupInput()); err != nil {
		t.Fatal(err)
	}
	first, _ := sys.SecretGet(loginKey(testBoxLogin))
	res, err := SQLSetup(context.Background(), env, sqlSetupInput())
	if err != nil || res.LoginDone != "kept" || res.Procedures != "present" {
		t.Fatalf("a second run: %+v %v", res, err)
	}
	if again, _ := sys.SecretGet(loginKey(testBoxLogin)); string(again) != string(first) {
		t.Fatal("a second run changed a password that worked")
	}
	// The box's copy lost (or changed by a person): the login is this box's own, so it gets a new one.
	if err := sys.SecretSet(loginKey(testBoxLogin), []byte("not-the-password")); err != nil {
		t.Fatal(err)
	}
	res, err = SQLSetup(context.Background(), env, sqlSetupInput())
	if err != nil || res.LoginDone != "given a new password" {
		t.Fatalf("after a lost password: %+v %v", res, err)
	}
	now, _ := sys.SecretGet(loginKey(testBoxLogin))
	if onServer, _ := fdb.login(testBoxLogin); onServer != string(now) {
		t.Fatal("the new password is not the one kept")
	}
}

func TestSQLSetupRefusesBeforeWritingAnything(t *testing.T) {
	cases := []struct {
		name string
		prep func(*fakeSys, *fakeDB)
		in   func(*SQLSetupInput)
		want string
	}{
		{"a box not paired", func(s *fakeSys, _ *fakeDB) { _ = os.Remove(filepath.Join(s.dir, "link.yaml")) }, nil, CodeSQLSetupNeedsLink},
		{"a wrong administrator password", nil, func(in *SQLSetupInput) { in.AdminPassword = "wrong" }, CodeDBUnreachable},
		{"a database name that is not one", nil, func(in *SQLSetupInput) { in.Database = "erp]; DROP LOGIN sa;--" }, CodeSpecInvalid},
		{"the login itself as the administrator", nil, func(in *SQLSetupInput) { in.AdminUser = testBoxLogin }, CodeSpecInvalid},
		{"no password", nil, func(in *SQLSetupInput) { in.AdminPassword = "" }, CodeSpecInvalid},
		{"a server on the internet", nil, func(in *SQLSetupInput) { in.Host, in.Port = "203.0.113.9", 1433 }, CodeDBHostPublic},
		{"procedures refused", func(_ *fakeSys, d *fakeDB) { d.failProcs = true }, nil, CodeProceduresFailed},
	}
	for _, c := range cases {
		sys, fdb := freshLinkedBox(t)
		if c.prep != nil {
			c.prep(sys, fdb)
		}
		in := sqlSetupInput()
		if c.in != nil {
			c.in(&in)
		}
		_, err := SQLSetup(context.Background(), testEnv(sys, fdb), in)
		if got := refusalCode(err); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
		if _, ok, _ := ReadSQLSetup(sys.dir); ok {
			t.Errorf("%s: a record was written", c.name)
		}
		if _, err := sys.SecretGet(loginKey(testBoxLogin)); err == nil {
			t.Errorf("%s: a login password was kept", c.name)
		}
		if _, ok := fdb.login(testBoxLogin); ok {
			t.Errorf("%s: the login was created", c.name)
		}
	}
}

// installerSpec is all an installer's first setup sends: the database comes from sql-setup.
func installerSpec() Spec {
	return Spec{Version: 1, ChangeSet: "cs-acme-0001", APIListen: "127.0.0.1:8082", DB: DBSpec{LeastPrivilege: true},
		Auth: AuthSpec{Username: "digi-hub", Password: testAuthPass}, Service: &ServiceSpec{DependOn: []string{"MSSQL$WIZSOFT"}}}
}

func TestAnInstallerSetsUpABoxSQLSetupPrepared(t *testing.T) {
	sys, fdb := freshLinkedBox(t)
	env := testEnv(sys, fdb)
	if _, err := SQLSetup(context.Background(), env, sqlSetupInput()); err != nil {
		t.Fatal(err)
	}
	fdb.opened = nil
	if _, err := ApplyAsInstaller(context.Background(), env, installerSpec(), true); err != nil {
		t.Fatalf("the check: %v", err)
	}
	res, err := ApplyAsInstaller(context.Background(), env, installerSpec(), false)
	if err != nil {
		t.Fatalf("the first setup: %v (%+v)", err, res)
	}
	cfg, _ := readConfig(t, sys.dir)
	if cfg.DB.User != testBoxLogin || cfg.DB.Host != `127.0.0.1\WIZSOFT` || cfg.DB.Database != "erp_db" ||
		cfg.APIListen != "127.0.0.1:8082" || cfg.Auth.Username != "digi-hub" || cfg.Auth.Password != testAuthPass {
		t.Fatalf("config.yaml %+v", cfg)
	}
	login, _ := sys.SecretGet(loginKey(testBoxLogin))
	if pw, _ := sys.SecretGet(config.DBPasswordKey(config.ERPHasavshevet)); string(pw) != string(login) {
		t.Fatal("the daemon does not log in with the login sql-setup made")
	}
	for _, u := range fdb.opened {
		if strings.EqualFold(u, "sa") {
			t.Fatal("the installer's setup opened the administrator: it has none")
		}
	}
	if !stepHas(res, "procedures", "sql-setup set them up") {
		t.Errorf("steps %+v", res.Steps)
	}
	// from now on the box is set up: another installer apply is refused under the lock
	if _, err := ApplyAsInstaller(context.Background(), env, installerSpec(), false); refusalCode(err) != CodeInstallerConfigured {
		t.Fatalf("a second installer apply: %v", err)
	}
}

func stepHas(res *ApplyResult, name, sub string) bool {
	for _, s := range res.Steps {
		if s.Name == name && strings.Contains(s.Detail, sub) {
			return true
		}
	}
	return false
}

func TestAnInstallersSetupIsRefused(t *testing.T) {
	str := func(s string) *string { return &s }
	yes := true
	cases := []struct {
		name  string
		prep  func(*fakeSys)
		setup bool // sql-setup ran first
		spec  func(*Spec)
		want  string
	}{
		{"before sql-setup", nil, false, nil, CodeDBAdminRequired},
		{"on a domain controller", func(s *fakeSys) { s.host.DomainRole = 5 }, true, nil, CodeInstallerDC},
		{"on a box set up already", func(s *fakeSys) { writeConfig(t, s.dir, handInstallConfig("")) }, true, nil, CodeInstallerConfigured},
		{"naming a server", nil, true, func(s *Spec) { s.DB.Host = "10.0.0.5\\OTHER" }, CodeInstallerNotAllowed},
		{"with an administrator", nil, true, func(s *Spec) { s.DB.User, s.DB.Password = "sa", testSaPass }, CodeInstallerNotAllowed},
		{"rotating the login", nil, true, func(s *Spec) { s.DB.RotateLogin = true }, CodeInstallerNotAllowed},
		{"turning encryption off", nil, true, func(s *Spec) { f := false; s.DB.Encrypt = &f }, CodeInstallerNotAllowed},
		{"another database", nil, true, func(s *Spec) { s.DB.Database = "other_db" }, CodeInstallerNotAllowed},
		{"another login", nil, true, func(s *Spec) { s.DB.Login = "erp_connector_other" }, CodeInstallerNotAllowed},
		{"no least privilege", nil, true, func(s *Spec) { s.DB.LeastPrivilege = false }, CodeInstallerNotAllowed},
		{"orders", nil, true, func(s *Spec) { s.SendOrderDir = str(`D:\orders`) }, CodeInstallerNotAllowed},
		{"an importer", nil, true, func(s *Spec) { s.Importer = &ImporterSpec{Mode: "bat", HasBatFile: `D:\x.bat`} }, CodeInstallerNotAllowed},
		{"tls", nil, true, func(s *Spec) { s.TLS = &TLSSpec{Mode: "off"} }, CodeInstallerNotAllowed},
		{"options", nil, true, func(s *Spec) { s.Options = &OptionsSpec{Debug: &yes} }, CodeInstallerNotAllowed},
		{"a firewall rule", nil, true, func(s *Spec) { s.Firewall = &FirewallSpec{AllowFrom: []string{"198.51.100.1"}} }, CodeInstallerNotAllowed},
		{"assumeIdle", nil, true, func(s *Spec) { s.AssumeIdle = true }, CodeInstallerNotAllowed},
		{"sap", nil, true, func(s *Spec) { s.ERP = "sap" }, CodeInstallerNotAllowed},
	}
	for _, c := range cases {
		sys, fdb := freshLinkedBox(t)
		env := testEnv(sys, fdb)
		if c.setup {
			if _, err := SQLSetup(context.Background(), env, sqlSetupInput()); err != nil {
				t.Fatal(err)
			}
		}
		if c.prep != nil {
			c.prep(sys)
		}
		before, _ := os.ReadFile(filepath.Join(sys.dir, configFile))
		s := installerSpec()
		if c.spec != nil {
			c.spec(&s)
		}
		_, err := ApplyAsInstaller(context.Background(), env, s, false)
		if got := refusalCode(err); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
		if after, _ := os.ReadFile(filepath.Join(sys.dir, configFile)); !bytes.Equal(before, after) {
			t.Errorf("%s: config.yaml changed", c.name)
		}
	}
}

// The team's apply can be adminless too: the login sql-setup made, no administrator, no secrets push (plan 090 D5).
func TestATeamApplyWithoutAnAdministrator(t *testing.T) {
	sys, fdb := freshLinkedBox(t)
	env := testEnv(sys, fdb)
	s := installerSpec()
	s.ERP = "hasavshevet"
	s.DB = DBSpec{Host: `127.0.0.1\WIZSOFT`, Database: "erp_db", LeastPrivilege: true, Login: testBoxLogin}
	if _, err := Apply(context.Background(), env, s); refusalCode(err) != CodeDBAdminRequired {
		t.Fatalf("before sql-setup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sys.dir, configFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused apply wrote config.yaml")
	}
	if _, err := SQLSetup(context.Background(), env, sqlSetupInput()); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(context.Background(), env, s); err != nil {
		t.Fatalf("after sql-setup: %v", err)
	}
	cfg, _ := readConfig(t, sys.dir)
	if cfg.DB.User != testBoxLogin {
		t.Fatalf("config.yaml logs in as %s", cfg.DB.User)
	}
	s.DB.RotateLogin = true
	if r := s.ValidateFor(true, nil); r == nil || r.Code != CodeSpecInvalid {
		t.Fatalf("rotateLogin with no administrator: %v", r)
	}
}

func TestProvisionRunsTheInstallerRulesOnlyWithTheFlagLast(t *testing.T) {
	sys, fdb := freshLinkedBox(t)
	env := testEnv(sys, fdb)
	if _, err := SQLSetup(context.Background(), env, sqlSetupInput()); err != nil {
		t.Fatal(err)
	}
	s := installerSpec()
	s.TLS = &TLSSpec{Mode: "off"}
	in, _ := json.Marshal(s)
	out, code := Run(context.Background(), env, []string{"apply", "--check", "--spec", "-", "--installer"}, bytes.NewReader(in))
	var e struct {
		Refusal *Refusal `json:"refusal"`
	}
	_ = json.Unmarshal(out, &e)
	if code != ExitRefused || e.Refusal == nil || e.Refusal.Code != CodeInstallerNotAllowed {
		t.Fatalf("with --installer: %d %s", code, out)
	}
	if _, code := Run(context.Background(), env, []string{"apply", "--installer", "--check", "--spec", "-"}, bytes.NewReader(in)); code != ExitUsage {
		t.Fatalf("--installer elsewhere than last: exit %d", code)
	}
}

// fakeConsole answers sql-setup's questions in order and records what it said.
type fakeConsole struct {
	answers []string
	said    []string
	asked   []string
}

func (c *fakeConsole) next(prompt string) (string, error) {
	c.asked = append(c.asked, prompt)
	if len(c.answers) == 0 {
		return "", errors.New("EOF")
	}
	a := c.answers[0]
	c.answers = c.answers[1:]
	return a, nil
}
func (c *fakeConsole) Ask(p string) (string, error)       { return c.next(p) }
func (c *fakeConsole) AskSecret(p string) (string, error) { return c.next("secret:" + p) }
func (c *fakeConsole) Say(line string)                    { c.said = append(c.said, line) }

func TestSQLSetupAtTheConsole(t *testing.T) {
	sys, fdb := freshLinkedBox(t)
	env := testEnv(sys, fdb)
	// the default server, the database, the administrator, its password, no encryption
	con := &fakeConsole{answers: []string{"", "erp_db", "sa", testSaPass, ""}}
	env.Console = con
	out, code := Run(context.Background(), env, []string{"sql-setup"}, strings.NewReader(""))
	all := strings.Join(con.said, "\n")
	if code != ExitOK || len(out) != 0 || !strings.Contains(all, "Done. Login "+testBoxLogin+": created") {
		t.Fatalf("exit %d out %q said:\n%s", code, out, all)
	}
	if !strings.Contains(con.asked[3], "secret:") {
		t.Fatalf("the password was asked as an ordinary line: %q", con.asked)
	}
	if strings.Contains(all, testSaPass) {
		t.Fatal("the console showed the administrator's password")
	}
	// a wrong password: the driver's error quotes it, and the console must not
	sys2, fdb2 := freshLinkedBox(t)
	env2 := testEnv(sys2, fdb2)
	con2 := &fakeConsole{answers: []string{"", "erp_db", "sa", "Wrong-Pa55word!", ""}}
	env2.Console = con2
	if _, code := Run(context.Background(), env2, []string{"sql-setup"}, strings.NewReader("")); code != ExitRefused {
		t.Fatalf("a wrong password: exit %d", code)
	}
	all2 := strings.Join(con2.said, "\n")
	if !strings.Contains(all2, "Not done (db_unreachable)") || strings.Contains(all2, "Wrong-Pa55word!") {
		t.Fatalf("said:\n%s", all2)
	}
	// no console (WinRM, the link, a pipe): refused, nothing asked
	env3 := testEnv(sys2, fdb2)
	out3, code3 := Run(context.Background(), env3, []string{"sql-setup"}, strings.NewReader(""))
	if code3 != ExitRefused || !bytes.Contains(out3, []byte(CodeSQLSetupConsoleOnly)) {
		t.Fatalf("without a console: %d %s", code3, out3)
	}
}

func TestStateSaysWhetherSQLSetupRan(t *testing.T) {
	sys, fdb := freshLinkedBox(t)
	env := testEnv(sys, fdb)
	res, err := State(context.Background(), env, StateInput{})
	if err != nil || res.SQLSetup.State != "absent" {
		t.Fatalf("before: %+v %v", res.SQLSetup, err)
	}
	if _, err := SQLSetup(context.Background(), env, sqlSetupInput()); err != nil {
		t.Fatal(err)
	}
	res, _ = State(context.Background(), env, StateInput{})
	if res.SQLSetup.State != "done" || res.SQLSetup.Login != testBoxLogin || res.SQLSetup.Database != "erp_db" {
		t.Fatalf("after: %+v", res.SQLSetup)
	}
	b, _ := json.Marshal(res)
	if bytes.Contains(b, []byte(testSaPass)) {
		t.Fatal("state shows the administrator's password")
	}
}

func TestSQLSetupHelpers(t *testing.T) {
	for box, want := range map[string]string{"acme-erp": "erp_connector_acme_erp", "x9": "erp_connector_x9",
		strings.Repeat("a", 51): ""} {
		if got := SQLSetupLogin(box); got != want {
			t.Errorf("SQLSetupLogin(%q) = %q, want %q", box, got, want)
		}
	}
	for in, want := range map[string]struct {
		host string
		port int
	}{`127.0.0.1\WIZSOFT`: {`127.0.0.1\WIZSOFT`, 0}, "10.0.0.5:1433": {"10.0.0.5", 1433}, "erp01": {"erp01", 0},
		"10.0.0.5:x": {"10.0.0.5:x", 0}} {
		if h, p := splitHostPort(in); h != want.host || p != want.port {
			t.Errorf("splitHostPort(%q) = %q %d", in, h, p)
		}
	}
}

// An unpaired box is told to pair first, not that its box id is too long.
func TestSQLSetupOnAnUnpairedBoxSaysPairFirst(t *testing.T) {
	sys, fdb := freshLinkedBox(t)
	_ = os.Remove(filepath.Join(sys.dir, "link.yaml"))
	_, err := SQLSetup(context.Background(), testEnv(sys, fdb), sqlSetupInput())
	var r *Refusal
	if !errors.As(err, &r) || r.Code != CodeSQLSetupNeedsLink || !strings.Contains(r.Message, "not paired") || !strings.Contains(r.Fix, "pair") {
		t.Fatalf("%v", err)
	}
}

// A box in service: config.yaml logs in as the login sql-setup manages. sql-setup never gives that login a new password
// (the running connector would lose its database), takes only the server and database the connector uses, and a re-run
// then only puts the procedures back (plan 090 review, major).
func TestSQLSetupOnABoxInService(t *testing.T) {
	setUp := func(t *testing.T) (*fakeSys, *fakeDB, Env) {
		sys, fdb := freshLinkedBox(t)
		env := testEnv(sys, fdb)
		if _, err := SQLSetup(context.Background(), env, sqlSetupInput()); err != nil {
			t.Fatal(err)
		}
		if _, err := ApplyAsInstaller(context.Background(), env, installerSpec(), false); err != nil {
			t.Fatal(err)
		}
		return sys, fdb, env
	}
	t.Run("another database", func(t *testing.T) {
		sys, fdb, env := setUp(t)
		before, _ := sys.SecretGet(loginKey(testBoxLogin))
		calls := fdb.loginCalls
		in := sqlSetupInput()
		in.Database = "erp_2027"
		if _, err := SQLSetup(context.Background(), env, in); refusalCode(err) != CodeSQLSetupInService {
			t.Fatalf("%v", err)
		}
		after, _ := sys.SecretGet(loginKey(testBoxLogin))
		if string(before) != string(after) || fdb.loginCalls != calls {
			t.Fatal("the running connector's login was touched")
		}
		if rec, _, _ := ReadSQLSetup(sys.dir); rec.Database != "erp_db" {
			t.Fatalf("the record moved to %s", rec.Database)
		}
	})
	t.Run("the same database again", func(t *testing.T) {
		sys, fdb, env := setUp(t)
		before, _ := sys.SecretGet(loginKey(testBoxLogin))
		calls := fdb.loginCalls
		res, err := SQLSetup(context.Background(), env, sqlSetupInput())
		if err != nil || res.LoginDone != "kept" {
			t.Fatalf("%+v %v", res, err)
		}
		after, _ := sys.SecretGet(loginKey(testBoxLogin))
		if string(before) != string(after) || fdb.loginCalls != calls {
			t.Fatal("a kept login was sent to SQL Server again (a password-history policy can refuse the same password)")
		}
	})
	t.Run("the box's passwords no longer log in", func(t *testing.T) {
		sys, fdb, env := setUp(t)
		fdb.setLogin(testBoxLogin, "changed-by-a-dba")
		if _, err := SQLSetup(context.Background(), env, sqlSetupInput()); refusalCode(err) != CodeSQLSetupInService {
			t.Fatalf("%v", err)
		}
		if pw, _ := fdb.login(testBoxLogin); pw != "changed-by-a-dba" {
			t.Fatal("sql-setup rotated the login of a connector in service")
		}
		_ = sys
	})
}

func TestSQLSetupInAConsoleThatIsNotElevated(t *testing.T) {
	sys, fdb := freshLinkedBox(t)
	// link.yaml that cannot be read, as a protected folder is to a console that is not elevated
	if err := os.Remove(filepath.Join(sys.dir, "link.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(sys.dir, "link.yaml"), 0o700); err != nil {
		t.Fatal(err)
	}
	env := testEnv(sys, fdb)
	if _, err := SQLSetup(context.Background(), env, sqlSetupInput()); refusalCode(err) != CodeSQLSetupNeedsAdmin {
		t.Fatalf("%v", err)
	}
	con := &fakeConsole{answers: []string{"", "erp_db", "sa", testSaPass, ""}}
	env.Console = con
	if _, code := Run(context.Background(), env, []string{"sql-setup"}, strings.NewReader("")); code != ExitRefused {
		t.Fatalf("exit %d", code)
	}
	if len(con.asked) != 0 {
		t.Fatalf("questions asked before it could know it cannot run: %q", con.asked)
	}
	if all := strings.Join(con.said, "\n"); !strings.Contains(all, CodeSQLSetupNeedsAdmin) || !strings.Contains(all, "Run as administrator") {
		t.Fatalf("said:\n%s", all)
	}
}

func TestATeamApplyWithoutALoginTakesSQLSetups(t *testing.T) {
	sys, fdb := freshLinkedBox(t)
	env := testEnv(sys, fdb)
	if _, err := SQLSetup(context.Background(), env, sqlSetupInput()); err != nil {
		t.Fatal(err)
	}
	s := installerSpec()
	s.ERP = "hasavshevet"
	s.DB = DBSpec{Host: `127.0.0.1\WIZSOFT`, Database: "erp_db", LeastPrivilege: true} // no login named
	if _, err := Apply(context.Background(), env, s); err != nil {
		t.Fatalf("%v", err)
	}
	if cfg, _ := readConfig(t, sys.dir); cfg.DB.User != testBoxLogin {
		t.Fatalf("config.yaml logs in as %s, not sql-setup's login", cfg.DB.User)
	}
}

func TestSQLSetupResultCarriesWhatToRedact(t *testing.T) {
	sys, fdb := freshLinkedBox(t)
	res, err := SQLSetup(context.Background(), testEnv(sys, fdb), sqlSetupInput())
	if err != nil {
		t.Fatal(err)
	}
	pw, _ := sys.SecretGet(loginKey(testBoxLogin))
	has := func(v string) bool {
		for _, s := range res.secrets {
			if s == v {
				return true
			}
		}
		return false
	}
	if !has(testSaPass) || !has(string(pw)) {
		t.Fatal("the result does not carry the administrator's and the login's passwords for redaction")
	}
	res, err = SQLSetup(context.Background(), testEnv(sys, fdb), SQLSetupInput{Host: `127.0.0.1\WIZSOFT`, Database: "erp_db", AdminUser: "sa", AdminPassword: "Wrong-1"})
	if err == nil || res == nil || len(res.secrets) == 0 || res.secrets[0] != "Wrong-1" {
		t.Fatalf("a refused run must still say what to redact: %+v %v", res, err)
	}
}

func TestSQLSetupOffersWhatWorks(t *testing.T) {
	for _, c := range []struct {
		in    SQLInstance
		addr  string
		notes string
	}{
		{SQLInstance{Name: "WIZSOFT", StaticPort: 1500, TCPEnabled: true, MixedAuth: true}, "127.0.0.1:1500", ""},
		{SQLInstance{Name: "WIZSOFT", DynamicPort: 63501, TCPEnabled: true, MixedAuth: true}, `127.0.0.1\WIZSOFT`, ""},
		{SQLInstance{Name: "MSSQLSERVER", DynamicPort: 50111, TCPEnabled: true, MixedAuth: true}, "127.0.0.1:50111", "dynamic port"},
		{SQLInstance{Name: "SQLEXPRESS"}, `127.0.0.1\SQLEXPRESS`, "TCP/IP is off; SQL logins are off"},
	} {
		if got := instanceAddress(c.in); got != c.addr {
			t.Errorf("%+v: address %q, want %q", c.in, got, c.addr)
		}
		if got := instanceNotes(c.in); (c.notes == "") != (got == "") || !strings.Contains(got, c.notes) {
			t.Errorf("%+v: notes %q, want %q", c.in, got, c.notes)
		}
	}
}

// The record's login is the default only for the server and database sql-setup set up.
func TestATeamApplyForAnotherDatabaseDoesNotTakeSQLSetupsLogin(t *testing.T) {
	sys, fdb := freshLinkedBox(t)
	env := testEnv(sys, fdb)
	if _, err := SQLSetup(context.Background(), env, sqlSetupInput()); err != nil {
		t.Fatal(err)
	}
	s := installerSpec()
	s.ERP = "hasavshevet"
	s.DB = DBSpec{Host: `127.0.0.1\WIZSOFT`, Database: "erp_2027", LeastPrivilege: true}
	_, err := Apply(context.Background(), env, s)
	var r *Refusal
	if !errors.As(err, &r) || r.Code != CodeDBAdminRequired || !strings.Contains(r.Message, DefaultLogin) {
		t.Fatalf("%v", err)
	}
}

// On a box in service the console offers the connector's own server, the one answer sql-setup takes there.
func TestSQLSetupAtTheConsoleOfABoxInService(t *testing.T) {
	sys, fdb := freshLinkedBox(t)
	env := testEnv(sys, fdb)
	if _, err := SQLSetup(context.Background(), env, sqlSetupInput()); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyAsInstaller(context.Background(), env, installerSpec(), false); err != nil {
		t.Fatal(err)
	}
	sys.instances[0].StaticPort = 1500 // the instance list alone would offer 127.0.0.1:1500
	con := &fakeConsole{answers: []string{"", "erp_db", "sa", testSaPass, ""}}
	env.Console = con
	if _, code := Run(context.Background(), env, []string{"sql-setup"}, strings.NewReader("")); code != ExitOK {
		t.Fatalf("exit %d, said:\n%s", code, strings.Join(con.said, "\n"))
	}
	all := strings.Join(con.said, "\n")
	if !strings.Contains(all, "already uses "+testBoxLogin) || !strings.Contains(all, ": kept.") {
		t.Fatalf("said:\n%s", all)
	}
	if !strings.Contains(con.asked[0], `[127.0.0.1\WIZSOFT]`) {
		t.Fatalf("offered %q, not the connector's own server", con.asked[0])
	}
	if got := serverAddress("10.0.0.5", 1500); got != "10.0.0.5:1500" {
		t.Errorf("serverAddress: %q", got)
	}
}

// plan 090 D7: the team's apply on a box sql-setup prepared names no server at all — the record's, with its port and
// encryption (which state does not show) — so a domain controller, or a box the team paired, needs no secrets push.
func TestATeamApplyThatNamesNoServerTakesTheRecord(t *testing.T) {
	sys, fdb := freshLinkedBox(t)
	env := testEnv(sys, fdb)
	s := installerSpec()
	s.ERP = "hasavshevet"
	s.DB = DBSpec{LeastPrivilege: true} // no server, no database, no login, no administrator
	if _, err := Apply(context.Background(), env, s); refusalCode(err) != CodeDBAdminRequired {
		t.Fatalf("before sql-setup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sys.dir, configFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused apply wrote config.yaml")
	}
	if _, err := SQLSetup(context.Background(), env, sqlSetupInput()); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(context.Background(), env, s); err != nil {
		t.Fatalf("after sql-setup: %v", err)
	}
	cfg, _ := readConfig(t, sys.dir)
	if cfg.DB.User != testBoxLogin || cfg.DB.Host != `127.0.0.1\WIZSOFT` || cfg.DB.Database != "erp_db" {
		t.Fatalf("config.yaml %+v", cfg.DB)
	}
	// what differs from the record is refused, not replaced
	for name, db := range map[string]DBSpec{
		"another database": {LeastPrivilege: true, Database: "other_db"},
		"another login":    {LeastPrivilege: true, Login: "erp_connector_other"},
	} {
		s2 := s
		s2.DB = db
		if _, err := Apply(context.Background(), env, s2); refusalCode(err) != CodeSpecInvalid {
			t.Errorf("%s: %v", name, err)
		}
	}
	s3 := s
	s3.ERP = "sap"
	if _, err := Apply(context.Background(), env, s3); refusalCode(err) != CodeSpecInvalid {
		t.Errorf("another ERP: %v", err)
	}
}
