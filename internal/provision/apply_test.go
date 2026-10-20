package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

func stepByName(res *ApplyResult, name string) (Step, bool) {
	for _, s := range res.Steps {
		if s.Name == name {
			return s, true
		}
	}
	return Step{}, false
}

func wantRefusal(t *testing.T, err error, code string) *Refusal {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) || r.Code != code {
		t.Fatalf("err = %v, want refusal %s", err, code)
	}
	return r
}

// GROOPY's hand install, fixed in one apply: defects 1–4 of plan 068 §2.4 go
// (the restart, the loopback listen, the pinned dynamic port, sa), and the
// fifth is stated (importer none). The connector then answers on the new
// address with the new credential.
func TestApply_FixesTheGroopyBox(t *testing.T) {
	sys, fdb, orders := groopyBox(t)
	env := testEnv(sys, fdb)
	spec := groopySpec(orders)

	res, err := Apply(context.Background(), env, spec)
	if err != nil {
		t.Fatalf("Apply: %v\nresult: %+v", err, res)
	}

	cfg, _ := readConfig(t, sys.dir)
	if cfg.APIListen != "0.0.0.0:8082" {
		t.Errorf("apiListen = %q", cfg.APIListen)
	}
	if cfg.DB.Host != `127.0.0.1\WIZSOFT` || cfg.DB.Port != 0 {
		t.Errorf("db = %q:%d, want the instance through the Browser", cfg.DB.Host, cfg.DB.Port)
	}
	if cfg.DB.User != LeastPrivilegeLogin {
		t.Errorf("db.user = %q, want %s", cfg.DB.User, LeastPrivilegeLogin)
	}
	if cfg.Auth.Username != "digi-hub" || cfg.Auth.Password != testAuthPass {
		t.Errorf("auth = %q / (password set: %v)", cfg.Auth.Username, cfg.Auth.Password == testAuthPass)
	}
	if cfg.Auth.Secret != strings.Repeat("ab", 32) {
		t.Error("the signing secret changed: every issued token would be invalid")
	}
	stored, _ := sys.SecretGet(config.DBPasswordKey(cfg.ERP))
	if string(stored) == testSaPass || string(stored) != fdb.loginPass || fdb.loginCalls != 1 {
		t.Errorf("the stored DB password is not the new login's (login calls %d)", fdb.loginCalls)
	}
	if !fdb.procsMade {
		t.Error("the Hasavshevet procedures were not installed")
	}
	if r := sys.rules[DefaultFirewallRule]; r.Port != 8082 || !slices.Equal(r.RemoteAddress, []string{"34.154.127.241"}) || !r.Enabled {
		t.Errorf("firewall rule = %+v", r)
	}
	if !slices.Equal(sys.svc.Dependencies, []string{"MSSQL$WIZSOFT"}) || !sys.svc.RecoveryPlanned {
		t.Errorf("service = %+v", sys.svc)
	}
	if !res.Restarted || res.Proof == nil || res.Proof.Health != "ok" || res.Proof.Version != "v1.0.99" {
		t.Errorf("restart/proof = %v %+v", res.Restarted, res.Proof)
	}
	if !res.PreimageCaptured {
		t.Error("no pre-image was captured")
	}
	if i, j := slices.Index(sys.calls, "stop"), slices.Index(sys.calls, "start"); i < 0 || j < i {
		t.Errorf("calls = %v: want stop then start", sys.calls)
	}
}

// A second identical apply changes nothing: no write, no new password, no
// restart.
func TestApply_SecondRunChangesNothing(t *testing.T) {
	sys, fdb, orders := groopyBox(t)
	env := testEnv(sys, fdb)
	spec := groopySpec(orders)
	if _, err := Apply(context.Background(), env, spec); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	_, before := readConfig(t, sys.dir)
	loginCalls := fdb.loginCalls
	sys.calls = nil

	res, err := Apply(context.Background(), env, spec)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	for _, s := range res.Steps {
		if s.Changed {
			t.Errorf("step %s changed on the second run (%s)", s.Name, s.Detail)
		}
	}
	if res.Restarted || fdb.loginCalls != loginCalls || res.PreimageCaptured {
		t.Errorf("restarted %v, login calls %d→%d, pre-image again %v", res.Restarted, loginCalls, fdb.loginCalls, res.PreimageCaptured)
	}
	if _, after := readConfig(t, sys.dir); !bytes.Equal(before, after) {
		t.Error("config.yaml was rewritten")
	}
	for _, c := range sys.calls {
		if c != "protect" {
			t.Errorf("the second run called %s", c)
		}
	}
}

// No secret leaves: not the client's sa password, not the connector's
// credential, not the password born for the least-privilege login — even
// when a driver error carries a DSN.
func TestRun_NoSecretInTheOutput(t *testing.T) {
	sys, fdb, orders := groopyBox(t)
	env := testEnv(sys, fdb)
	spec := groopySpec(orders)
	in, _ := json.Marshal(spec)

	out, code := Run(context.Background(), env, []string{"apply", "--spec", "-"}, bytes.NewReader(in))
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, out)
	}
	for _, s := range []string{testSaPass, testAuthPass, fdb.loginPass} {
		if bytes.Contains(out, []byte(s)) {
			t.Fatalf("the output carries a secret: %s", out)
		}
	}

	// A refused login whose error quotes the DSN, password and all.
	spec.ChangeSet = "cs-groopy-0002"
	spec.DB.Password = "Wrong-pass-with-a-quote-\"-inside"
	in, _ = json.Marshal(spec)
	out, code = Run(context.Background(), env, []string{"apply", "--spec", "-"}, bytes.NewReader(in))
	if code != ExitRefused || !bytes.Contains(out, []byte(CodeDBUnreachable)) {
		t.Fatalf("exit %d: %s", code, out)
	}
	if bytes.Contains(out, []byte("Wrong-pass-with")) {
		t.Fatalf("the driver's DSN reached the output: %s", out)
	}
	var env2 Envelope
	if err := json.Unmarshal(out, &env2); err != nil {
		t.Fatalf("the output is not one JSON object: %v", err)
	}
}

// Accepted orders live in the daemon's memory: with one queued, nothing is
// written and nothing restarts.
func TestApply_RefusesWhileOrdersArePending(t *testing.T) {
	sys, fdb, orders := groopyBox(t)
	sys.daemon.set(func(d *fakeDaemon) { d.queued = 1 })
	_, before := readConfig(t, sys.dir)

	_, err := Apply(context.Background(), testEnv(sys, fdb), groopySpec(orders))
	wantRefusal(t, err, CodeOrdersPending)
	assertUntouched(t, sys, before)
}

// GROOPY's defect 1: the daemon runs a config whose credential is no longer
// on disk, so its queue cannot be read. Only assumeIdle gets past.
func TestApply_PendingUnknownNeedsAssumeIdle(t *testing.T) {
	sys, fdb, orders := groopyBox(t)
	sys.daemon.set(func(d *fakeDaemon) { d.pass = "the-config-of-2026-10-06" })
	_, before := readConfig(t, sys.dir)

	_, err := Apply(context.Background(), testEnv(sys, fdb), groopySpec(orders))
	wantRefusal(t, err, CodePendingUnknown)
	assertUntouched(t, sys, before)

	spec := groopySpec(orders)
	spec.AssumeIdle = true
	res, err := Apply(context.Background(), testEnv(sys, fdb), spec)
	if err != nil {
		t.Fatalf("with assumeIdle: %v", err)
	}
	if len(res.Warnings) == 0 || !strings.Contains(strings.Join(res.Warnings, " "), "assumeIdle") {
		t.Errorf("warnings = %v, want the assumption stated", res.Warnings)
	}
}

// A connector older than GET /api/sendOrder answers 404 there: unknown, not
// idle.
func TestApply_AnOldDaemonCannotSayItIsIdle(t *testing.T) {
	sys, fdb, orders := groopyBox(t)
	sys.daemon.set(func(d *fakeDaemon) { d.noQueueRoute = true })
	_, err := Apply(context.Background(), testEnv(sys, fdb), groopySpec(orders))
	wantRefusal(t, err, CodePendingUnknown)
}

// The connector does not come up on the new config: everything goes back —
// config.yaml, the secret, the firewall rule, the dependencies — and the
// service is started again on the old config.
func TestApply_RollsBackWhenThePortProofFails(t *testing.T) {
	sys, fdb, orders := groopyBox(t)
	_, before := readConfig(t, sys.dir)
	sys.onStart = func(cfg config.Config) {
		sys.daemon.set(func(d *fakeDaemon) { d.down = cfg.APIListen == "0.0.0.0:8082" })
	}

	res, err := Apply(context.Background(), testEnv(sys, fdb), groopySpec(orders))
	wantRefusal(t, err, CodePortProofFailed)
	if !res.RolledBack {
		t.Fatal("not rolled back")
	}
	if _, after := readConfig(t, sys.dir); !bytes.Equal(before, after) {
		t.Error("config.yaml is not the pre-image")
	}
	if pw, _ := sys.SecretGet(config.DBPasswordKey(config.ERPHasavshevet)); string(pw) != testSaPass {
		t.Error("the DB secret is not the pre-image")
	}
	if _, ok := sys.rules[DefaultFirewallRule]; ok {
		t.Error("the firewall rule this run created is still there")
	}
	if len(sys.svc.Dependencies) != 0 {
		t.Errorf("dependencies = %v, want none again", sys.svc.Dependencies)
	}
	if n := countCalls(sys.calls, "start"); n != 2 {
		t.Errorf("started %d times, want 2 (new config, then the restored one)", n)
	}
}

// A failure before the restart rolls the files back without restarting:
// the service still runs the old config from memory.
func TestApply_AFailureBeforeTheRestartDoesNotRestart(t *testing.T) {
	sys, fdb, orders := groopyBox(t)
	failing := &failingRuleSys{fakeSys: sys}
	_, before := readConfig(t, sys.dir)

	res, err := Apply(context.Background(), testEnv(failing.fakeSys, fdb).with(failing), groopySpec(orders))
	wantRefusal(t, err, CodeFirewallFailed)
	if !res.RolledBack {
		t.Error("not rolled back")
	}
	if _, after := readConfig(t, sys.dir); !bytes.Equal(before, after) {
		t.Error("config.yaml is not the pre-image")
	}
	if countCalls(sys.calls, "stop")+countCalls(sys.calls, "start") != 0 {
		t.Errorf("calls = %v: nothing should restart", sys.calls)
	}
}

type failingRuleSys struct{ *fakeSys }

func (f *failingRuleSys) SetFirewallRule(FirewallRule) error { return errors.New("access denied") }

func (e Env) with(sys System) Env { e.Sys = sys; return e }

// config.yaml changed after the plan read it.
func TestApply_RefusesDrift(t *testing.T) {
	sys, fdb, orders := groopyBox(t)
	_, before := readConfig(t, sys.dir)
	spec := groopySpec(orders)
	old := strings.Repeat("0", 64)
	spec.Expect = &Expect{ConfigSHA256: &old}

	_, err := Apply(context.Background(), testEnv(sys, fdb), spec)
	wantRefusal(t, err, CodeDrift)
	assertUntouched(t, sys, before)

	right := fileSHA(before)
	spec.Expect.ConfigSHA256 = &right
	if _, err := Apply(context.Background(), testEnv(sys, fdb), spec); err != nil {
		t.Fatalf("with the right hash: %v", err)
	}
}

// What the daemon would refuse to start on is refused before any write — here
// a receipt folder that the spec's order folder lands on.
func TestApply_RefusesAConfigTheDaemonWouldRefuse(t *testing.T) {
	sys, fdb, orders := groopyBox(t)
	cfg, _ := readConfig(t, sys.dir)
	cfg.SendReceiptDir = orders
	before := writeConfig(t, sys.dir, cfg)

	_, err := Apply(context.Background(), testEnv(sys, fdb), groopySpec(orders))
	wantRefusal(t, err, CodeConfigRefused)
	assertUntouched(t, sys, before)
}

// An importer that is not on the machine is a typo found now, not on the
// first order.
func TestApply_RefusesAMissingImporter(t *testing.T) {
	sys, fdb, orders := groopyBox(t)
	spec := groopySpec(orders)
	spec.Importer = &ImporterSpec{Mode: "bat", HasBatFile: filepath.Join(orders, "digi.bat")}
	_, err := Apply(context.Background(), testEnv(sys, fdb), spec)
	wantRefusal(t, err, CodeImporterMissing)
}

// Restore puts the pre-image back and the connector answers on it again.
func TestRestore_PutsThePreimageBack(t *testing.T) {
	sys, fdb, orders := groopyBox(t)
	env := testEnv(sys, fdb)
	_, before := readConfig(t, sys.dir)
	if _, err := Apply(context.Background(), env, groopySpec(orders)); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	res, err := Restore(context.Background(), env, RestoreInput{ChangeSet: "cs-groopy-0001"})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, after := readConfig(t, sys.dir); !bytes.Equal(before, after) {
		t.Error("config.yaml is not the pre-image")
	}
	if _, ok := sys.rules[DefaultFirewallRule]; ok {
		t.Error("the firewall rule is still there")
	}
	if !res.Restarted || res.Proof == nil {
		t.Errorf("restarted %v, proof %+v", res.Restarted, res.Proof)
	}

	_, err = Restore(context.Background(), env, RestoreInput{ChangeSet: "cs-never-applied"})
	wantRefusal(t, err, CodePreimageMissing)
}

func TestVerify_AfterApply(t *testing.T) {
	sys, fdb, orders := groopyBox(t)
	env := testEnv(sys, fdb)
	if _, err := Apply(context.Background(), env, groopySpec(orders)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	res, err := Verify(context.Background(), env, VerifyInput{ExpectVersion: "v1.0.99"})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.OK {
		t.Errorf("checks: %+v", res.Checks)
	}
	sys.daemon.set(func(d *fakeDaemon) { d.health = 503 })
	if res, _ := Verify(context.Background(), env, VerifyInput{}); res.OK {
		t.Error("a database that does not answer passed")
	}
}

func assertUntouched(t *testing.T, sys *fakeSys, before []byte) {
	t.Helper()
	if _, after := readConfig(t, sys.dir); !bytes.Equal(before, after) {
		t.Error("config.yaml changed")
	}
	// The lock file may be there; no pre-image may.
	if all, _ := listPreimages(sys.dir); len(all) != 0 {
		t.Errorf("a pre-image was written: %v", all)
	}
	for _, c := range sys.calls {
		t.Errorf("called %s", c)
	}
}

func countCalls(calls []string, name string) int {
	n := 0
	for _, c := range calls {
		if c == name {
			n++
		}
	}
	return n
}

// has.exe gets its parameter file as written and runs from sendOrderDir: a
// bare name is checked there.
func TestApply_ARelativeParameterFileIsFoundInTheOrderFolder(t *testing.T) {
	sys, fdb, orders := groopyBox(t)
	exe := filepath.Join(t.TempDir(), "has.exe")
	if err := os.WriteFile(exe, []byte("MZ"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := groopySpec(orders)
	spec.Importer = &ImporterSpec{Mode: "hasExe", HasExePath: exe, HasParamFile: "digi_perm.bat"}

	_, err := Apply(context.Background(), testEnv(sys, fdb), spec)
	wantRefusal(t, err, CodeImporterMissing)

	if err := os.WriteFile(filepath.Join(orders, "digi_perm.bat"), []byte("@echo off"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(context.Background(), testEnv(sys, fdb), spec); err != nil {
		t.Fatalf("with the file in the order folder: %v", err)
	}
	if cfg, _ := readConfig(t, sys.dir); cfg.HasParamFile != "digi_perm.bat" || cfg.HasExePath != exe {
		t.Errorf("importer = %q %q", cfg.HasExePath, cfg.HasParamFile)
	}
}

// A spec without a firewall rule leaves verify's firewall check passing; a
// rule the caller names must exist.
func TestVerify_TheFirewallRuleOnlyWhenAskedFor(t *testing.T) {
	sys, fdb, orders := groopyBox(t)
	env := testEnv(sys, fdb)
	spec := groopySpec(orders)
	spec.Firewall = nil
	if _, err := Apply(context.Background(), env, spec); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res, _ := Verify(context.Background(), env, VerifyInput{}); !res.OK {
		t.Errorf("checks: %+v", res.Checks)
	}
	if res, _ := Verify(context.Background(), env, VerifyInput{FirewallRule: DefaultFirewallRule}); res.OK {
		t.Error("a rule that was asked for and is missing passed")
	}
}
