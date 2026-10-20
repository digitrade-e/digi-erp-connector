package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// Regression tests for the findings of plan 068's three-critic panel
// (2026-10-20), one per finding fixed in this package.

// Security, blocker: the client's administrator credential goes to no host
// outside the box and its LAN — not from apply, not from discover. A name
// counts by every address it resolves to.
func TestPanel_TheAdminCredentialGoesToNoPublicHost(t *testing.T) {
	for _, host := range []string{"203.0.113.10", `sql.example.com\WIZSOFT`} {
		sys, fdb, orders := handInstallBox(t)
		spec := handInstallSpec(orders)
		spec.DB.Host = host
		if !strings.Contains(host, `\`) {
			spec.DB.Port = 1433
		}
		_, err := Apply(context.Background(), testEnv(sys, fdb), spec)
		wantRefusal(t, err, CodeDBHostPublic)
		if len(fdb.opened) != 0 {
			t.Errorf("%s: logged in as %v", host, fdb.opened)
		}
	}

	sys, fdb, orders := handInstallBox(t)
	spec := handInstallSpec(orders)
	spec.DB.Host = `erp01.example.local\WIZSOFT` // a LAN name
	if _, err := Apply(context.Background(), testEnv(sys, fdb), spec); err != nil {
		t.Fatalf("a LAN host: %v", err)
	}

	sys, fdb, _ = handInstallBox(t)
	facts, err := Discover(context.Background(), testEnv(sys, fdb),
		DiscoverInput{DB: &DiscoverDB{Host: "203.0.113.10", Port: 1433, User: "sa", Password: testSaPass}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(facts.DatabaseError, CodeDBHostPublic) || len(fdb.opened) != 0 {
		t.Errorf("discover: error %q, logged in as %v", facts.DatabaseError, fdb.opened)
	}
}

// Ops: after a restore to a config without the least-privilege login, the
// next apply keeps the login — this box created it and still holds its
// password — instead of asking for rotateLogin (the pilot box, 2026-10-20).
func TestPanel_AnApplyAfterARestoreKeepsTheLogin(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	env := testEnv(sys, fdb)
	if _, err := Apply(context.Background(), env, handInstallSpec(orders)); err != nil {
		t.Fatal(err)
	}
	pw, _ := fdb.login(DefaultLogin)
	if _, err := Restore(context.Background(), env, RestoreInput{ChangeSet: "cs-erp01-0001"}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if cfg, _ := readConfig(t, sys.dir); cfg.DB.User != "sa" {
		t.Fatalf("the restore left db.user %s", cfg.DB.User)
	}

	spec := handInstallSpec(orders)
	spec.ChangeSet = "cs-erp01-0002"
	res, err := Apply(context.Background(), env, spec)
	if err != nil {
		t.Fatalf("the apply after the restore: %v", err)
	}
	if s, _ := stepByName(res, "login"); s.Changed || fdb.loginCalls != 1 {
		t.Errorf("login step %+v, login calls %d", s, fdb.loginCalls)
	}
	if now, _ := fdb.login(DefaultLogin); now != pw {
		t.Error("the login's password changed")
	}
}

// Ops: two connectors on one SQL Server each name their own login; the
// second leaves the first one's alone.
func TestPanel_ANamedLoginLeavesAnotherConnectorsAlone(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	fdb.setLogin(DefaultLogin, "Another0connector0uses0this0one")
	spec := handInstallSpec(orders)
	spec.DB.Login = "erp_connector_node2"
	res, err := Apply(context.Background(), testEnv(sys, fdb), spec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	cfg, _ := readConfig(t, sys.dir)
	stored, _ := sys.SecretGet(config.DBPasswordKey(cfg.ERP))
	mine, _ := fdb.login("erp_connector_node2")
	if cfg.DB.User != "erp_connector_node2" || string(stored) != mine {
		t.Errorf("db.user %s; the stored password is not the new login's", cfg.DB.User)
	}
	if other, _ := fdb.login(DefaultLogin); other != "Another0connector0uses0this0one" {
		t.Error("the other connector's login was changed")
	}
	if s, _ := stepByName(res, "login"); s.Detail != "erp_connector_node2 created" {
		t.Errorf("login step %+v", s)
	}
}

// Ops: a restore back to the older order folder carries the counter
// forward — the old folder's copy is stale, and Hasavshevet must never get a
// number twice.
func TestPanel_RestoreCarriesTheOrderCounter(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	writeCounter(t, orders, 41)
	moved := filepath.Join(t.TempDir(), "neworders")
	if err := os.MkdirAll(moved, 0o700); err != nil {
		t.Fatal(err)
	}
	env := testEnv(sys, fdb)
	if _, err := Apply(context.Background(), env, handInstallSpec(moved)); err != nil {
		t.Fatal(err)
	}
	writeCounter(t, moved, 57) // the orders since

	res, err := Restore(context.Background(), env, RestoreInput{ChangeSet: "cs-erp01-0001"})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if n := readCounter(t, orders); n != 57 {
		t.Errorf("the restored folder counts from %d", n)
	}
	if s, ok := stepByName(res, "order_counter"); !ok || !s.Changed {
		t.Errorf("steps = %+v", res.Steps)
	}
}

// Ops: so does a rollback, when orders came in on the new folder before the
// port proof gave up.
func TestPanel_TheRollbackCarriesTheOrderCounterBack(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	writeCounter(t, orders, 41)
	moved := filepath.Join(t.TempDir(), "neworders")
	if err := os.MkdirAll(moved, 0o700); err != nil {
		t.Fatal(err)
	}
	sys.onStart = func(cfg config.Config) {
		if cfg.SendOrderDir == moved {
			writeCounter(t, moved, 44)
		}
		sys.daemon.set(func(d *fakeDaemon) { d.down = cfg.SendOrderDir == moved })
	}
	res, err := Apply(context.Background(), testEnv(sys, fdb), handInstallSpec(moved))
	wantRefusal(t, err, CodePortProofFailed)
	if !res.RolledBack {
		t.Fatal("not rolled back")
	}
	if n := readCounter(t, orders); n != 44 {
		t.Errorf("the restored folder counts from %d", n)
	}
}

// Ops: a restore whose credential no longer reaches the database says so.
func TestPanel_RestoreSaysWhenTheDatabaseDoesNotAnswer(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	env := testEnv(sys, fdb)
	if _, err := Apply(context.Background(), env, handInstallSpec(orders)); err != nil {
		t.Fatal(err)
	}
	sys.daemon.set(func(d *fakeDaemon) { d.health = http.StatusServiceUnavailable }) // sa's password changed meanwhile
	res, err := Restore(context.Background(), env, RestoreInput{ChangeSet: "cs-erp01-0001"})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.Proof == nil || res.Proof.Health != "db_unavailable" || !strings.Contains(strings.Join(res.Warnings, "\n"), "database does not") {
		t.Errorf("proof %+v, warnings %q", res.Proof, res.Warnings)
	}
}

// panicSys is a box whose listener table makes the run panic, with a secret
// in the panic's text.
type panicSys struct{ *fakeSys }

func (panicSys) Listeners() ([]Listener, error) { panic("listener table holds " + testSaPass) }

// Security: a panic is one envelope, redacted like every other answer.
func TestPanel_APanicIsOneRedactedEnvelope(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	env := testEnv(sys, fdb)
	env.Sys = panicSys{sys}
	in := `{"db":{"user":"sa","password":"` + testSaPass + `"}}`
	out, code := Run(context.Background(), env, []string{"discover", "--spec", "-"}, strings.NewReader(in))
	var e Envelope
	if err := json.Unmarshal(out, &e); err != nil || code != ExitRefused || e.Refusal == nil || e.Refusal.Code != CodeInternal {
		t.Fatalf("exit %d: %s", code, out)
	}
	if bytes.Contains(out, []byte(testSaPass)) {
		t.Errorf("the panic's secret is in the output: %s", out)
	}
}

func writeCounter(t *testing.T, dir string, n int) {
	t.Helper()
	b, _ := json.Marshal(map[string]int{"lastOrderNumber": n})
	if err := os.WriteFile(filepath.Join(dir, orderNumberFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readCounter(t *testing.T, dir string) int64 {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, orderNumberFile))
	if err != nil {
		t.Fatal(err)
	}
	return lastOrderNumber(b)
}

// Security: an importer that accounts besides the administrators can change
// is refused — the connector runs it as LocalSystem — unless the spec says a
// human accepted it.
func TestPanel_AnImporterOthersCanChangeIsRefused(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	bat := filepath.Join(t.TempDir(), "importorders.bat")
	if err := os.WriteFile(bat, []byte("@echo off\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sys.writers = map[string][]string{bat: {`BUILTIN\Users`}}
	spec := handInstallSpec(orders)
	spec.Importer = &ImporterSpec{Mode: "bat", HasBatFile: bat}
	_, err := Apply(context.Background(), testEnv(sys, fdb), spec)
	if r := wantRefusal(t, err, CodeImporterWritable); !strings.Contains(r.Message, `BUILTIN\Users`) {
		t.Errorf("refusal %q", r.Message)
	}

	spec.Importer.AllowWritable = true
	res, err := Apply(context.Background(), testEnv(sys, fdb), spec)
	if err != nil {
		t.Fatalf("with allowWritable: %v", err)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "importer_writable") {
		t.Errorf("warnings %q", res.Warnings)
	}

	facts, err := Discover(context.Background(), testEnv(sys, fdb), DiscoverInput{})
	if err != nil {
		t.Fatal(err)
	}
	if w := facts.ImporterWriters[bat]; len(w) != 1 || !hasNote(facts, "importer_writable") {
		t.Errorf("writers %v, notes %+v", w, facts.Notes)
	}
}

// Ops: a named instance reached without a port needs the SQL Server Browser
// after every reboot; verify and discover say when it would not start.
func TestPanel_TheBrowserMustStartByItself(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	env := testEnv(sys, fdb)
	if _, err := Apply(context.Background(), env, handInstallSpec(orders)); err != nil {
		t.Fatal(err)
	}
	sys.sqlBrowser.StartType = "manual"
	res, err := Verify(context.Background(), env, VerifyInput{})
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := checkByName(res, "browser"); !ok || c.OK || res.OK {
		t.Errorf("browser check %+v, ok %v", c, res.OK)
	}
	facts, err := Discover(context.Background(), env, DiscoverInput{})
	if err != nil {
		t.Fatal(err)
	}
	if facts.SQL.BrowserStartType != "manual" || !hasNote(facts, "browser_not_automatic") {
		t.Errorf("start type %q, notes %+v", facts.SQL.BrowserStartType, facts.Notes)
	}
}

// Security: the WinRM Analytic log records what crosses WinRM, a spec's
// secrets included; discover says when it is on.
func TestPanel_DiscoverSaysWhenWinRMIsRecorded(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	sys.host.WinRMAnalytic = true
	facts, err := Discover(context.Background(), testEnv(sys, fdb), DiscoverInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !hasNote(facts, "winrm_analytic_on") {
		t.Errorf("notes %+v", facts.Notes)
	}
}

func checkByName(res *VerifyResult, name string) (Check, bool) {
	for _, c := range res.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return Check{}, false
}
