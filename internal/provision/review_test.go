package provision

// Regression tests for the fresh-eyes review of 2026-10-20: each one is a
// scenario the review reproduced against the first version.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/filelock"
)

// A box that already logs in as erp_connector_read, with a stored password
// SQL Server no longer takes.
func leastPrivBox(t *testing.T) (*fakeSys, *fakeDB, string) {
	t.Helper()
	sys, fdb, orders := handInstallBox(t)
	cfg, _ := readConfig(t, sys.dir)
	cfg.DB.Host, cfg.DB.Port, cfg.DB.User = `127.0.0.1\WIZSOFT`, 0, LeastPrivilegeLogin
	writeConfig(t, sys.dir, cfg)
	if err := sys.SecretSet(config.DBPasswordKey(cfg.ERP), []byte("Stored0password0nobody0takes")); err != nil {
		t.Fatal(err)
	}
	sys.calls = nil
	fdb.loginExists, fdb.loginPass = true, "TheServer0has0another0one0now"
	return sys, fdb, orders
}

// Finding 1: the login is not given a new password unasked — it may be
// another connector's — and once it is, a rollback keeps the new password,
// the only one SQL Server takes.
func TestReview_ALoginIsRotatedOnlyWhenAskedAndARollbackKeepsItsPassword(t *testing.T) {
	sys, fdb, orders := leastPrivBox(t)
	_, before := readConfig(t, sys.dir)
	_, err := Apply(context.Background(), testEnv(sys, fdb), handInstallSpec(orders))
	wantRefusal(t, err, CodeLeastPrivilege)
	assertUntouched(t, sys, before)

	spec := handInstallSpec(orders)
	spec.DB.RotateLogin = true
	sys.onStart = func(cfg config.Config) {
		sys.daemon.set(func(d *fakeDaemon) { d.down = cfg.APIListen == "0.0.0.0:8082" })
	}
	res, err := Apply(context.Background(), testEnv(sys, fdb), spec)
	wantRefusal(t, err, CodePortProofFailed)
	if !res.RolledBack || fdb.loginCalls != 1 {
		t.Fatalf("rolled back %v, login calls %d", res.RolledBack, fdb.loginCalls)
	}
	if _, after := readConfig(t, sys.dir); !bytes.Equal(before, after) {
		t.Error("config.yaml is not the run's pre-image")
	}
	stored, _ := sys.SecretGet(config.DBPasswordKey(config.ERPHasavshevet))
	if string(stored) != fdb.loginPass {
		t.Error("after the rollback the stored password is not the one SQL Server has")
	}
}

// Finding 1, the other way in: the hand script made erp_connector_read and
// this connector does not use it.
func TestReview_AnExistingLoginThisConnectorDoesNotUseIsLeftAlone(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	fdb.loginExists, fdb.loginPass = true, "Another0connector0uses0this0one"
	_, before := readConfig(t, sys.dir)

	_, err := Apply(context.Background(), testEnv(sys, fdb), handInstallSpec(orders))
	r := wantRefusal(t, err, CodeLeastPrivilege)
	if !strings.Contains(r.Message, "does not use it") || fdb.loginCalls != 0 {
		t.Errorf("refusal %q, login calls %d", r.Message, fdb.loginCalls)
	}
	assertUntouched(t, sys, before)
}

// Finding 2: a re-run of a change set that fails before the restart goes back
// to the state before THIS run — what the service runs — not the first run's.
func TestReview_AReRunRollsBackToItsOwnStart(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	spec := handInstallSpec(orders)
	if _, err := Apply(context.Background(), testEnv(sys, fdb), spec); err != nil {
		t.Fatalf("first run: %v", err)
	}
	_, afterFirst := readConfig(t, sys.dir)
	sys.calls = nil

	spec.Firewall.AllowFrom = []string{"198.51.100.241", "198.51.100.120"}
	failing := &failingRuleSys{fakeSys: sys}
	res, err := Apply(context.Background(), testEnv(sys, fdb).with(failing), spec)
	wantRefusal(t, err, CodeFirewallFailed)
	if !res.RolledBack {
		t.Fatal("not rolled back")
	}
	if _, now := readConfig(t, sys.dir); !bytes.Equal(now, afterFirst) {
		t.Error("config.yaml went back past this run, to the hand install")
	}
	if countCalls(sys.calls, "stop")+countCalls(sys.calls, "start") != 0 {
		t.Errorf("calls %v: the service still runs the first run's config, nothing to restart", sys.calls)
	}
	// The change set's own pre-image is still the hand install: Restore's.
	p, err := loadPreimage(preimageDir(sys.dir, spec.ChangeSet))
	if err != nil || !p.m.Files[0].Present {
		t.Fatalf("the change set's pre-image: %v", err)
	}
}

// Finding 3: a rollback leaves a service that was stopped stopped.
func TestReview_ARollbackLeavesAStoppedServiceStopped(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	sys.svc.State = "stopped"
	sys.onStart = func(cfg config.Config) {
		sys.daemon.set(func(d *fakeDaemon) { d.down = cfg.APIListen == "0.0.0.0:8082" })
	}
	_, err := Apply(context.Background(), testEnv(sys, fdb), handInstallSpec(orders))
	wantRefusal(t, err, CodePortProofFailed)
	if sys.svc.State != "stopped" || countCalls(sys.calls, "start") != 1 {
		t.Errorf("state %s, calls %v: want it stopped again after its one start", sys.svc.State, sys.calls)
	}
}

// Finding 4: a rule of the same name the client made is never touched.
func TestReview_AForeignFirewallRuleIsLeftAlone(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	theirs := FirewallRule{Name: DefaultFirewallRule, Enabled: true, Port: 3389}
	sys.rules[DefaultFirewallRule] = theirs
	_, before := readConfig(t, sys.dir)

	_, err := Apply(context.Background(), testEnv(sys, fdb), handInstallSpec(orders))
	wantRefusal(t, err, CodeFirewallForeign)
	assertUntouched(t, sys, before)
	if !sys.rules[DefaultFirewallRule].Same(theirs) {
		t.Error("the client's rule changed")
	}
}

// Finding 5: what a spec leaves out keeps the box's value.
func TestReview_LeftOutFieldsKeepTheBox(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	cfg, _ := readConfig(t, sys.dir)
	cfg.DB.Encrypt, cfg.DB.TrustServerCertificate = true, true
	cfg.HasBatFile = filepath.Join(orders, "digi.bat")
	if err := os.WriteFile(cfg.HasBatFile, []byte("@echo off"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, sys.dir, cfg)
	spec := handInstallSpec(orders)
	spec.SendOrderDir, spec.Importer = nil, nil

	if _, err := Apply(context.Background(), testEnv(sys, fdb), spec); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, _ := readConfig(t, sys.dir)
	if !got.DB.Encrypt || !got.DB.TrustServerCertificate || got.SendOrderDir != orders || got.HasBatFile != cfg.HasBatFile {
		t.Errorf("db %+v, orders %q, bat %q", got.DB, got.SendOrderDir, got.HasBatFile)
	}
}

// Orders with no importer must be stated: leaving it out on a box that has
// none is refused, not taken as "none".
func TestReview_AnUnstatedImporterIsRefused(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	spec := handInstallSpec(orders)
	spec.Importer = nil
	_, err := Apply(context.Background(), testEnv(sys, fdb), spec)
	wantRefusal(t, err, CodeImporterRequired)
}

// Finding 5: the order counter moves with the order folder — a new folder
// without it would start again at 1 — and two counters are refused.
func TestReview_TheOrderCounterMovesWithTheFolder(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	if err := os.WriteFile(filepath.Join(orders, orderNumberFile), []byte(`{"lastOrderNumber": 41}`), 0o600); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(t.TempDir(), "neworders")
	if err := os.MkdirAll(moved, 0o700); err != nil {
		t.Fatal(err)
	}
	spec := handInstallSpec(moved)

	res, err := Apply(context.Background(), testEnv(sys, fdb), spec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(moved, orderNumberFile))
	if lastOrderNumber(b) != 41 {
		t.Errorf("the counter in the new folder is %q", b)
	}
	if s, ok := stepByName(res, "order_counter"); !ok || !s.Changed {
		t.Errorf("steps = %+v", res.Steps)
	}

	// Back to the first folder, which by now holds a different count.
	spec2 := handInstallSpec(orders)
	spec2.ChangeSet = "cs-erp01-0002"
	if err := os.WriteFile(filepath.Join(moved, orderNumberFile), []byte(`{"lastOrderNumber": 57}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Apply(context.Background(), testEnv(sys, fdb), spec2)
	wantRefusal(t, err, CodeOrderNumberConflict)
}

// Finding 6: a discover host that is not one is refused before any DSN
// exists — the driver's parse error would quote the password URL-escaped.
func TestReview_DiscoverRefusesAHostThatIsNotOne(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	in := `{"db":{"host":"my host","user":"sa","password":"P@ss word!x"}}`
	out, code := Run(context.Background(), testEnv(sys, fdb), []string{"discover", "--spec", "-"}, strings.NewReader(in))
	if code != ExitRefused || !bytes.Contains(out, []byte(CodeSpecInvalid)) || bytes.Contains(out, []byte("word")) {
		t.Fatalf("exit %d: %s", code, out)
	}
}

// Finding 7: a second run while one holds the lock is refused.
func TestReview_ASecondRunAtOnceIsRefused(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	if err := os.MkdirAll(provisionDir(sys.dir), 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := filelock.Lock(filepath.Join(provisionDir(sys.dir), ".lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Unlock()
	_, err = Apply(context.Background(), testEnv(sys, fdb), handInstallSpec(orders))
	wantRefusal(t, err, CodeBusy)
}

// Finding 8: a secret like "true" cannot break the one JSON object.
func TestReview_AShortSecretKeepsTheOutputJSON(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	spec := handInstallSpec(orders)
	spec.DB.Password = "true"
	in, _ := json.Marshal(spec)
	out, _ := Run(context.Background(), testEnv(sys, fdb), []string{"apply", "--spec", "-"}, bytes.NewReader(in))
	var e Envelope
	if err := json.Unmarshal(out, &e); err != nil {
		t.Fatalf("not one JSON object: %v\n%s", err, out)
	}
}

// Finding 9: a run that changes nothing keeps no pre-image, so it never
// pushes a real restore point out.
func TestReview_ARunThatChangesNothingKeepsNoPreimage(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	env := testEnv(sys, fdb)
	if _, err := Apply(context.Background(), env, handInstallSpec(orders)); err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= 7; i++ {
		spec := handInstallSpec(orders)
		spec.ChangeSet = "cs-erp01-noop-" + string(rune('0'+i))
		if _, err := Apply(context.Background(), env, spec); err != nil {
			t.Fatal(err)
		}
	}
	if all, _ := listPreimages(sys.dir); len(all) != 1 || all[0] != "cs-erp01-0001" {
		t.Errorf("pre-images = %v, want only the first run's", all)
	}
	if _, err := os.Stat(runDir(sys.dir, "cs-erp01-0001")); err == nil {
		t.Error("a run's pre-image outlived its run")
	}
}

// Finding 9: a config.yaml that no longer reads is what restore is for — with
// --assume-idle, since the order queue cannot be asked.
func TestReview_RestoreAnUnreadableConfig(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	env := testEnv(sys, fdb)
	_, before := readConfig(t, sys.dir)
	if _, err := Apply(context.Background(), env, handInstallSpec(orders)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sys.dir, configFile), []byte("db: [unclosed"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Restore(context.Background(), env, RestoreInput{ChangeSet: "cs-erp01-0001"})
	wantRefusal(t, err, CodePendingUnknown)
	if _, err := Restore(context.Background(), env, RestoreInput{ChangeSet: "cs-erp01-0001", AssumeIdle: true}); err != nil {
		t.Fatalf("with --assume-idle: %v", err)
	}
	if _, after := readConfig(t, sys.dir); !bytes.Equal(before, after) {
		t.Error("config.yaml is not the pre-image")
	}
}

// Restore keeps the login's latest password when the config before and after
// log in as it: SQL Server has no other.
func TestReview_RestoreKeepsTheLoginsCurrentPassword(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	env := testEnv(sys, fdb)
	if _, err := Apply(context.Background(), env, handInstallSpec(orders)); err != nil {
		t.Fatal(err)
	}
	fdb.loginPass = "Changed0on0the0server0meanwhile" // the stored one no longer logs in
	spec := handInstallSpec(orders)
	spec.ChangeSet, spec.DB.RotateLogin = "cs-erp01-0002", true
	spec.Firewall.AllowFrom = []string{"198.51.100.241", "198.51.100.120"}
	if _, err := Apply(context.Background(), env, spec); err != nil {
		t.Fatalf("second change set: %v", err)
	}
	latest := fdb.loginPass

	if _, err := Restore(context.Background(), env, RestoreInput{ChangeSet: "cs-erp01-0002"}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	cfg, _ := readConfig(t, sys.dir)
	stored, _ := sys.SecretGet(config.DBPasswordKey(cfg.ERP))
	if cfg.DB.User != LeastPrivilegeLogin || string(stored) != latest {
		t.Errorf("user %s; the stored password is not the login's current one", cfg.DB.User)
	}
}
