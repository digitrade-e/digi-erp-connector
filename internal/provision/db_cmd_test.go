package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/db"
)

// offSa is the db command that moves the pilot box off sa: the instance
// through the Browser, a least-privilege login created by sa (used once).
func offSa() DBInput {
	return DBInput{Version: 1, ChangeSet: "cs-db-000001",
		DB:    DBChange{Host: `127.0.0.1\WIZSOFT`, Database: "erp_db", LeastPrivilege: true},
		Admin: &DBAdmin{User: "sa", Password: testSaPass}}
}

// The db command moves the box off sa and nothing else: the API credential,
// the listener and the options stay byte for byte; the login is created, its
// password stored; the restart is proved including /api/health.
func TestApplyDB_MovesOffSa(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	env := testEnv(sys, fdb)
	before, _ := readConfig(t, sys.dir)

	chk, err := ApplyDB(context.Background(), env, offSa(), true)
	if err != nil || chk.Check == nil || chk.Check.Login != "create" || !chk.Check.DBSecret || !chk.Check.Restart || chk.Check.Recovery {
		t.Fatalf("--check = %+v, %v", chk.Check, err)
	}
	if _, ok := fdb.login(DefaultLogin); ok {
		t.Fatal("--check created the login")
	}

	res, err := ApplyDB(context.Background(), env, offSa(), false)
	if err != nil {
		t.Fatalf("ApplyDB: %v\n%+v", err, res)
	}
	after, _ := readConfig(t, sys.dir)
	want := before
	want.DB.Host, want.DB.Port, want.DB.User = `127.0.0.1\WIZSOFT`, 0, DefaultLogin
	wb, _ := config.Marshal(want)
	gb, _ := config.Marshal(after)
	if !bytes.Equal(wb, gb) {
		t.Errorf("config.yaml changed beyond the database:\n got %s\nwant %s", gb, wb)
	}
	stored, _ := sys.SecretGet(config.DBPasswordKey(after.ERP))
	if pw, ok := fdb.login(DefaultLogin); !ok || string(stored) != pw || pw == testSaPass {
		t.Error("the stored DB password is not the new login's")
	}
	if !res.Restarted || res.Proof == nil || res.Proof.Health != "ok" || !res.PreimageCaptured {
		t.Errorf("restarted %v, proof %+v, pre-image %v", res.Restarted, res.Proof, res.PreimageCaptured)
	}
	p, _ := loadPreimage(preimageDir(sys.dir, "cs-db-000001"))
	var files []string
	for _, f := range p.m.Files {
		files = append(files, f.Path)
	}
	if !slices.Equal(files, []string{configFile, secretFile(config.ERPHasavshevet)}) {
		t.Errorf("pre-image files %v, want config.yaml and the DB secret", files)
	}

	// restore: the old config and sa's stored password come back; the login stays on the server.
	if _, err := Restore(context.Background(), env, RestoreInput{ChangeSet: "cs-db-000001"}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	restored, _ := readConfig(t, sys.dir)
	if restored.DB != before.DB {
		t.Errorf("restore: db = %+v", restored.DB)
	}
	if got, _ := sys.SecretGet(config.DBPasswordKey(after.ERP)); string(got) != testSaPass {
		t.Error("restore did not put sa's stored password back")
	}
}

// Once the box knows the login's password, a DB-only change needs no
// administrator: the login is kept, and sa is never opened.
func TestApplyDB_KeepsAKnownLoginWithoutAnAdministrator(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	env := testEnv(sys, fdb)
	if _, err := ApplyDB(context.Background(), env, offSa(), false); err != nil {
		t.Fatalf("off sa: %v", err)
	}
	fdb.opened = nil
	in := offSa()
	in.ChangeSet, in.Admin = "cs-db-000002", nil
	in.DB.Encrypt, in.DB.Trust = ptr(true), ptr(true)
	res, err := ApplyDB(context.Background(), env, in, false)
	if err != nil {
		t.Fatalf("ApplyDB: %v", err)
	}
	cfg, _ := readConfig(t, sys.dir)
	if !cfg.DB.Encrypt || !cfg.DB.TrustServerCertificate {
		t.Errorf("encrypt %v trust %v", cfg.DB.Encrypt, cfg.DB.TrustServerCertificate)
	}
	if slices.ContainsFunc(fdb.opened, func(u string) bool { return strings.EqualFold(u, "sa") }) {
		t.Errorf("sa was opened: %v", fdb.opened)
	}
	if !slices.ContainsFunc(res.Steps, func(s Step) bool { return s.Name == "login" && !s.Changed }) {
		t.Errorf("the login was not kept: %+v", res.Steps)
	}

	// The same again changes nothing, and is still proved on the running daemon.
	in.ChangeSet = "cs-db-000003"
	sys.calls = nil
	again, err := ApplyDB(context.Background(), env, in, false)
	if err != nil || again.Restarted || again.PreimageCaptured || again.Proof == nil || again.Proof.Health != "ok" {
		t.Errorf("a no-op: %v, restarted %v, pre-image %v, proof %+v", err, again.Restarted, again.PreimageCaptured, again.Proof)
	}
}

// Without an administrator and without a password the box knows, there is no
// login to keep: refused before anything is written.
func TestApplyDB_NoAdministratorNoKnownLogin(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	in := offSa()
	in.Admin = nil
	_, err := ApplyDB(context.Background(), testEnv(sys, fdb), in, false)
	wantRefusal(t, err, CodeLeastPrivilege)
	if len(sys.calls) != 0 {
		t.Errorf("work was done: %v", sys.calls)
	}
}

// A given login (WinRM only) keeps its stored password when none comes and it
// is the login in use.
func TestApplyDB_GivenLoginWithTheStoredPassword(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	in := DBInput{Version: 1, ChangeSet: "cs-db-000004",
		DB: DBChange{Host: "127.0.0.1", Port: 63501, Database: "erp_db", User: "sa", AllowSa: true, Encrypt: ptr(true)}}
	res, err := ApplyDB(context.Background(), testEnv(sys, fdb), in, false)
	if err != nil {
		t.Fatalf("ApplyDB: %v", err)
	}
	cfg, _ := readConfig(t, sys.dir)
	if cfg.DB.User != "sa" || !cfg.DB.Encrypt {
		t.Errorf("db = %+v", cfg.DB)
	}
	if got, _ := sys.SecretGet(config.DBPasswordKey(cfg.ERP)); string(got) != testSaPass {
		t.Error("the stored password changed")
	}
	if res.Proof == nil || res.Proof.Health != "ok" {
		t.Errorf("proof %+v", res.Proof)
	}

	other := in
	other.DB.User = "erp_admin"
	_, err = ApplyDB(context.Background(), testEnv(sys, fdb), other, false)
	wantRefusal(t, err, CodeSpecInvalid)
}

// The connector answers but its database does not after the change: a failed
// proof here, and the pre-image goes back.
func TestApplyDB_UnhealthyDatabaseRollsBack(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	_, before := readConfig(t, sys.dir)
	sys.onStart = func(c config.Config) {
		sys.daemon.set(func(d *fakeDaemon) {
			d.health = http.StatusOK
			if c.DB.User == DefaultLogin {
				d.health = http.StatusServiceUnavailable
			}
		})
	}
	res, err := ApplyDB(context.Background(), testEnv(sys, fdb), offSa(), false)
	wantRefusal(t, err, CodePortProofFailed)
	if !res.RolledBack {
		t.Fatal("not rolled back")
	}
	if _, after := readConfig(t, sys.dir); !bytes.Equal(before, after) {
		t.Error("config.yaml was not put back")
	}
	if got, _ := sys.SecretGet(config.DBPasswordKey(config.ERPHasavshevet)); string(got) != testSaPass {
		t.Error("the stored password was not put back")
	}
	if sys.svc.State != "running" {
		t.Error("the service was left stopped")
	}
}

func TestApplyDB_TurnsTheDatabaseOff(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	in := DBInput{Version: 1, ChangeSet: "cs-db-000005", DB: DBChange{Mode: DBModeNone}}
	res, err := ApplyDB(context.Background(), testEnv(sys, fdb), in, false)
	if err != nil {
		t.Fatalf("ApplyDB: %v", err)
	}
	cfg, _ := readConfig(t, sys.dir)
	if db.IsConfigured(cfg) || cfg.DB.Host != "" || cfg.DB.User != "" {
		t.Errorf("db = %+v, want none", cfg.DB)
	}
	if res.Proof == nil || res.Proof.Health != HealthNoDatabase {
		t.Errorf("proof %+v, want health %s", res.Proof, HealthNoDatabase)
	}
	if len(fdb.opened) != 0 {
		t.Errorf("the database was opened as %v", fdb.opened)
	}
}

func TestApplyDB_Refusals(t *testing.T) {
	mod := func(f func(*DBInput)) DBInput { in := offSa(); f(&in); return in }
	tests := []struct {
		name   string
		in     DBInput
		linked bool
		setup  func(*fakeSys)
		code   string
	}{
		{"version", mod(func(in *DBInput) { in.Version = 0 }), false, nil, CodeSpecInvalid},
		{"a bad change set", mod(func(in *DBInput) { in.ChangeSet = "cs" }), false, nil, CodeChangeSetID},
		{"two administrators", mod(func(in *DBInput) { in.DB.User, in.DB.Password = "sa", testSaPass }), false, nil, CodeSpecInvalid},
		{"trust disagrees", mod(func(in *DBInput) { in.DB.Trust, in.DB.TrustServerCertificate = ptr(true), ptr(false) }), false, nil, CodeSpecInvalid},
		{"allowSa with least privilege", mod(func(in *DBInput) { in.DB.AllowSa = true }), false, nil, CodeSpecInvalid},
		{"mode none with a host", DBInput{Version: 1, ChangeSet: "cs-db-000001", DB: DBChange{Mode: DBModeNone, Host: "x"}}, false, nil, CodeSpecInvalid},
		{"mode none with an administrator", DBInput{Version: 1, ChangeSet: "cs-db-000001", DB: DBChange{Mode: DBModeNone},
			Admin: &DBAdmin{User: "sa", Password: testSaPass}}, false, nil, CodeSpecInvalid},
		{"an unknown mode", DBInput{Version: 1, ChangeSet: "cs-db-000001", DB: DBChange{Mode: "off"}}, false, nil, CodeSpecInvalid},
		{"sa as the connector's login", DBInput{Version: 1, ChangeSet: "cs-db-000001",
			DB: DBChange{Host: "127.0.0.1", Port: 63501, Database: "erp_db", User: "sa", Password: testSaPass}}, false, nil, CodeSaLogin},
		{"a given login behind the link", DBInput{Version: 1, ChangeSet: "cs-db-000001",
			DB: DBChange{Host: "127.0.0.1", Port: 63501, Database: "erp_db", User: "erp_admin", Password: "x"}}, true, nil, CodeSaLinkMode},
		{"admin without least privilege", DBInput{Version: 1, ChangeSet: "cs-db-000001",
			DB:    DBChange{Host: "127.0.0.1", Port: 63501, Database: "erp_db", User: "erp_admin", Password: "x"},
			Admin: &DBAdmin{User: "sa", Password: testSaPass}}, false, nil, CodeSpecInvalid},
		{"no database name", mod(func(in *DBInput) { in.DB.Database = "" }), false, nil, CodeSpecInvalid},
		{"a public database host", mod(func(in *DBInput) { in.DB.Host, in.DB.Port = "sql.example.com", 1433 }), false, nil, CodeDBHostPublic},
		{"a wrong administrator password", mod(func(in *DBInput) { in.Admin.Password = "wrong" }), false, nil, CodeDBUnreachable},
		{"orders pending", offSa(), false, func(s *fakeSys) { s.daemon.set(func(d *fakeDaemon) { d.queued = 1 }) }, CodeOrdersPending},
		{"drift", mod(func(in *DBInput) { in.Expect = &Expect{ConfigSHA256: ptr(strings.Repeat("1", 64))} }), false, nil, CodeDrift},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sys, fdb, _ := handInstallBox(t)
			if tc.linked {
				linkBox(t, sys)
			}
			if tc.setup != nil {
				tc.setup(sys)
			}
			_, before := readConfig(t, sys.dir)
			res, err := ApplyDB(context.Background(), testEnv(sys, fdb), tc.in, false)
			wantRefusal(t, err, tc.code)
			if _, after := readConfig(t, sys.dir); !bytes.Equal(before, after) {
				t.Error("config.yaml was written on a refusal")
			}
			if res.Restarted || res.PreimageCaptured || len(res.Steps) != 0 || len(sys.calls) != 0 {
				t.Errorf("work was done before the refusal: %+v, calls %v", res, sys.calls)
			}
			if _, ok := fdb.login(DefaultLogin); ok {
				t.Error("the login was created")
			}
		})
	}
	t.Run("no config.yaml", func(t *testing.T) {
		sys, fdb, _ := handInstallBox(t)
		_ = os.Remove(filepath.Join(sys.dir, configFile))
		_, err := ApplyDB(context.Background(), testEnv(sys, fdb), offSa(), false)
		wantRefusal(t, err, CodeConfigRefused)
	})
}

// Behind the link the least-privilege move works with sa as the
// administrator (used once): it is what moves BFL off sa.
func TestApplyDB_BehindTheLink(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	linkBox(t, sys)
	if _, err := ApplyDB(context.Background(), testEnv(sys, fdb), offSa(), false); err != nil {
		t.Fatalf("ApplyDB behind the link: %v", err)
	}
	if cfg, _ := readConfig(t, sys.dir); cfg.DB.User != DefaultLogin {
		t.Errorf("db.user = %s", cfg.DB.User)
	}
}

func TestRun_DB(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	env := testEnv(sys, fdb)
	in := `{"version":1,"changeSet":"cs-db-000006","db":{"host":"127.0.0.1\\WIZSOFT","database":"erp_db","leastPrivilege":true},` +
		`"admin":{"user":"sa","password":"` + testSaPass + `"}}`
	out, code := Run(context.Background(), env, []string{"db", "--spec", "-"}, strings.NewReader(in))
	var e Envelope
	if err := json.Unmarshal(out, &e); err != nil || code != ExitOK || !e.OK || e.Command != "db" {
		t.Fatalf("code %d, out %s", code, out)
	}
	pw, _ := fdb.login(DefaultLogin)
	if bytes.Contains(out, []byte(testSaPass)) || bytes.Contains(out, []byte(pw)) {
		t.Errorf("a password is in the output: %s", out)
	}
	withAuth := `{"version":1,"changeSet":"cs-db-000007","db":{"mode":"none"},"auth":{"password":"x"}}`
	out, code = Run(context.Background(), env, []string{"db", "--check", "--spec", "-"}, strings.NewReader(withAuth))
	if code != ExitRefused || !bytes.Contains(out, []byte(CodeSpecInvalid)) {
		t.Errorf("an API credential in the input: code %d, out %s", code, out)
	}
	if _, code := Run(context.Background(), env, []string{"db", "--spec"}, strings.NewReader(in)); code != ExitUsage {
		t.Errorf("db --spec without -: code %d", code)
	}
}

// --- DB-less boxes in apply, discover and verify ---

// dbLessSpec is BFL's write node: orders only, no database.
func dbLessSpec(orders string) Spec {
	s := handInstallSpec(orders)
	s.ChangeSet = "cs-dbless-0001"
	s.DB = DBSpec{Mode: DBModeNone}
	s.Service = nil
	return s
}

func TestApply_DBLess(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	env := testEnv(sys, fdb)
	res, err := Apply(context.Background(), env, dbLessSpec(orders))
	if err != nil {
		t.Fatalf("Apply: %v\n%+v", err, res)
	}
	cfg, _ := readConfig(t, sys.dir)
	if db.IsConfigured(cfg) {
		t.Errorf("db = %+v, want none", cfg.DB)
	}
	if len(fdb.opened) != 0 || fdb.procsMade {
		t.Errorf("the database was opened as %v (procedures %v)", fdb.opened, fdb.procsMade)
	}
	if res.Proof == nil || res.Proof.Health != HealthNoDatabase {
		t.Errorf("proof %+v", res.Proof)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, "database does not") {
			t.Errorf("a DB-less box warned about its database: %s", w)
		}
	}
	// A second run changes nothing.
	again, err := Apply(context.Background(), env, dbLessSpec(orders))
	if err != nil || again.Restarted {
		t.Errorf("second run: %v, restarted %v", err, again.Restarted)
	}

	// verify: with db.mode none the database check passes; without it, it fails and says why.
	vr, err := Verify(context.Background(), env, VerifyInput{DB: &VerifyDB{Mode: DBModeNone}})
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := check(vr, "database"); !c.OK {
		t.Errorf("database check with mode none: %+v", c)
	}
	vr, _ = Verify(context.Background(), env, VerifyInput{})
	if c, _ := check(vr, "database"); c.OK || !strings.Contains(c.Detail, "mode") {
		t.Errorf("database check without mode none: %+v", c)
	}
}

func TestApply_DBLessBehindTheLink(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	linkBox(t, sys)
	s := dbLessSpec(orders)
	s.APIListen, s.Firewall = "127.0.0.1:8082", nil
	if _, err := Apply(context.Background(), testEnv(sys, fdb), s); err != nil {
		t.Fatalf("a DB-less apply behind the link: %v", err)
	}
}

func TestSpec_DBModeRefusals(t *testing.T) {
	orders := t.TempDir()
	for name, d := range map[string]DBSpec{
		"none with a host":          {Mode: DBModeNone, Host: "127.0.0.1"},
		"none with least privilege": {Mode: DBModeNone, LeastPrivilege: true},
		"none with encrypt":         {Mode: DBModeNone, Encrypt: ptr(false)},
		"an unknown mode":           {Mode: "off"},
	} {
		s := dbLessSpec(orders)
		s.DB = d
		if r := s.Validate(); r == nil || r.Code != CodeSpecInvalid {
			t.Errorf("%s: %v", name, r)
		}
	}
}

func TestVerify_DBLessExpectationOnABoxWithADatabase(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	vr, err := Verify(context.Background(), testEnv(sys, fdb), VerifyInput{DB: &VerifyDB{Mode: DBModeNone}})
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := check(vr, "database"); c.OK {
		t.Errorf("a box with a database passed as DB-less: %+v", c)
	}
	if _, err := Verify(context.Background(), testEnv(sys, fdb), VerifyInput{DB: &VerifyDB{Mode: "off"}}); err == nil {
		t.Error("an unknown mode was taken")
	}
}

func TestDiscover_DBLess(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	env := testEnv(sys, fdb)
	f, err := Discover(context.Background(), env, DiscoverInput{DB: &DiscoverDB{Mode: DBModeNone}})
	if err != nil {
		t.Fatal(err)
	}
	if f.Suggested.DBMode != DBModeNone || f.Suggested.DBHost != "" || f.Suggested.LeastPriv || len(f.Suggested.DependOn) != 0 {
		t.Errorf("suggestion %+v", f.Suggested)
	}
	if len(fdb.opened) != 0 {
		t.Errorf("the database was opened as %v", fdb.opened)
	}
	out, code := Run(context.Background(), env, []string{"discover", "--spec", "-"},
		strings.NewReader(`{"db":{"mode":"none","user":"sa","password":"x"}}`))
	if code != ExitRefused || !bytes.Contains(out, []byte(CodeSpecInvalid)) {
		t.Errorf("mode none with a credential: code %d, out %s", code, out)
	}
	// A box whose config.yaml names no database is suggested as one, and noted.
	cfg, _ := readConfig(t, sys.dir)
	cfg.DB = config.DBConfig{Driver: config.DBDriverMSSQL}
	writeConfig(t, sys.dir, cfg)
	f, _ = Discover(context.Background(), env, DiscoverInput{})
	if f.Suggested.DBMode != DBModeNone || !hasNote(f, "no_database") {
		t.Errorf("a DB-less config: suggestion %+v, notes %+v", f.Suggested, f.Notes)
	}
}
