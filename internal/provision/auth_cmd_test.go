package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

const testNewAuthPass = "rotated-by-digi-mcp-on-the-box-0123456789AB"

func authInput(pw string) AuthInput {
	return AuthInput{Version: 1, ChangeSet: "cs-auth-00001", Auth: AuthChange{Password: pw}}
}

// The auth command changes the credential and nothing else: the database, the
// listener, the options and the DB secret stay byte for byte, the database is
// never opened, and the restart is proved with the NEW credential.
func TestApplyAuth_SetsOnlyTheCredential(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	env := testEnv(sys, fdb)
	before, _ := readConfig(t, sys.dir)
	secretBefore, _ := sys.SecretGet(config.DBPasswordKey(before.ERP))

	res, err := ApplyAuth(context.Background(), env, authInput(testNewAuthPass), false)
	if err != nil {
		t.Fatalf("ApplyAuth: %v", err)
	}
	after, _ := readConfig(t, sys.dir)
	want := before
	want.Auth.Password = testNewAuthPass
	wantBytes, _ := config.Marshal(want)
	gotBytes, _ := config.Marshal(after)
	if !bytes.Equal(wantBytes, gotBytes) {
		t.Errorf("config.yaml changed beyond the password:\n got %s\nwant %s", gotBytes, wantBytes)
	}
	if !res.Restarted || res.Proof == nil || !res.PreimageCaptured || res.Proof.Health != "ok" {
		t.Errorf("restarted %v, proof %+v, pre-image %v", res.Restarted, res.Proof, res.PreimageCaptured)
	}
	if len(fdb.opened) > 0 {
		t.Errorf("the database was opened as %v; auth never touches it", fdb.opened)
	}
	if got, _ := sys.SecretGet(config.DBPasswordKey(before.ERP)); !bytes.Equal(got, secretBefore) {
		t.Error("the DB secret changed")
	}
	// The daemon now takes the new credential, and only it.
	d := env.daemonFor(after)
	if _, err := d.login(context.Background(), before.Auth.Username, testNewAuthPass); err != nil {
		t.Errorf("the new credential does not log in: %v", err)
	}
	if _, err := d.login(context.Background(), before.Auth.Username, before.Auth.Password); err == nil {
		t.Error("the old credential still logs in")
	}
	// The pre-image is config.yaml alone: restore puts back exactly this.
	p, err := loadPreimage(preimageDir(sys.dir, "cs-auth-00001"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.m.Files) != 1 || p.m.Files[0].Path != configFile || p.m.Service != nil || p.m.Firewall != nil {
		t.Errorf("pre-image = %+v, want config.yaml only", p.m)
	}
}

func TestApplyAuth_UsernameAndSecret(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	env := testEnv(sys, fdb)
	before, _ := readConfig(t, sys.dir)
	in := authInput(testNewAuthPass)
	in.Auth.Username = "digi-hub"
	in.RotateSecret = true

	res, err := ApplyAuth(context.Background(), env, in, false)
	if err != nil {
		t.Fatalf("ApplyAuth: %v", err)
	}
	after, _ := readConfig(t, sys.dir)
	if after.Auth.Username != "digi-hub" || after.Auth.Password != testNewAuthPass {
		t.Errorf("auth = %s / %s", after.Auth.Username, after.Auth.Password)
	}
	if after.Auth.Secret == before.Auth.Secret || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(after.Auth.Secret) {
		t.Errorf("the signing secret was not regenerated: %q", after.Auth.Secret)
	}
	if after.Auth.TokenTTL != before.Auth.TokenTTL || after.DB != before.DB || after.APIListen != before.APIListen {
		t.Error("something besides the credential changed")
	}
	if res.Proof == nil {
		t.Error("no proof")
	}
	out := envelope("auth", res, nil, append(in.secrets(), res.secrets...))
	if bytes.Contains(out, []byte(after.Auth.Secret)) || bytes.Contains(out, []byte(testNewAuthPass)) {
		t.Errorf("the envelope carries a secret: %s", out)
	}
}

// A re-run with the credential the box already has writes nothing and does not
// restart, but still proves the credential on the running daemon.
func TestApplyAuth_SameCredentialProvesWithoutRestart(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	sys.svc.ProcessStart = time.Now().Add(time.Minute) // the daemon runs the file on disk
	env := testEnv(sys, fdb)
	cur, before := readConfig(t, sys.dir)

	res, err := ApplyAuth(context.Background(), env, authInput(cur.Auth.Password+"-padded-to-16"), true)
	if err != nil || res.Check == nil || res.Check.Config != "changed: auth" || !res.Check.Restart {
		t.Fatalf("--check of a new password = %+v, %v", res.Check, err)
	}
	same := authInput(cur.Auth.Password)
	res, err = ApplyAuth(context.Background(), env, same, false)
	if err != nil {
		t.Fatalf("ApplyAuth: %v", err)
	}
	if _, after := readConfig(t, sys.dir); !bytes.Equal(before, after) {
		t.Error("config.yaml was rewritten for a no-op")
	}
	if res.Restarted || res.PreimageCaptured || slices.Contains(sys.calls, "stop") {
		t.Errorf("a no-op restarted (%v) or kept a pre-image (%v): %v", res.Restarted, res.PreimageCaptured, sys.calls)
	}
	if res.Proof == nil || res.Proof.Health != "ok" {
		t.Errorf("a no-op on a running daemon was not proved: %+v", res.Proof)
	}
	chk, err := ApplyAuth(context.Background(), env, same, true)
	if err != nil || chk.Check == nil || chk.Check.Config != "unchanged" || chk.Check.Restart {
		t.Errorf("--check of the same credential = %+v, %v; want unchanged and no restart", chk.Check, err)
	}
}

func TestApplyAuth_NoOpLeavesAStoppedServiceStopped(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	sys.svc.State = "stopped"
	cur, _ := readConfig(t, sys.dir)
	res, err := ApplyAuth(context.Background(), testEnv(sys, fdb), authInput(cur.Auth.Password), false)
	if err != nil {
		t.Fatalf("ApplyAuth: %v", err)
	}
	if res.Restarted || sys.svc.State != "stopped" || slices.Contains(sys.calls, "start") {
		t.Errorf("a no-op started a stopped service: %v", sys.calls)
	}
}

func TestApplyAuth_CheckWritesNothing(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	_, before := readConfig(t, sys.dir)
	res, err := ApplyAuth(context.Background(), testEnv(sys, fdb), authInput(testNewAuthPass), true)
	if err != nil {
		t.Fatalf("ApplyAuth --check: %v", err)
	}
	if _, after := readConfig(t, sys.dir); !bytes.Equal(before, after) {
		t.Error("--check wrote config.yaml")
	}
	if res.Restarted || res.PreimageCaptured || len(sys.calls) != 0 {
		t.Errorf("--check did something: restarted %v, pre-image %v, calls %v", res.Restarted, res.PreimageCaptured, sys.calls)
	}
}

func TestApplyAuth_RestoreUndoesIt(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	env := testEnv(sys, fdb)
	cur, before := readConfig(t, sys.dir)
	if _, err := ApplyAuth(context.Background(), env, authInput(testNewAuthPass), false); err != nil {
		t.Fatalf("ApplyAuth: %v", err)
	}
	res, err := Restore(context.Background(), env, RestoreInput{ChangeSet: "cs-auth-00001"})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, after := readConfig(t, sys.dir); !bytes.Equal(before, after) {
		t.Error("restore did not put config.yaml back")
	}
	if !res.Restarted || res.Proof == nil {
		t.Errorf("restore did not restart and prove the old credential: %+v", res)
	}
	if _, err := env.daemonFor(cur).login(context.Background(), cur.Auth.Username, cur.Auth.Password); err != nil {
		t.Errorf("the old credential does not log in after the restore: %v", err)
	}
}

// The new credential does not come up: config.yaml goes back, and the service
// runs on the old one again.
func TestApplyAuth_FailedProofRollsBack(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	cur, before := readConfig(t, sys.dir)
	sys.onStart = func(c config.Config) {
		sys.daemon.set(func(d *fakeDaemon) { d.down = c.Auth.Password == testNewAuthPass })
	}
	res, err := ApplyAuth(context.Background(), testEnv(sys, fdb), authInput(testNewAuthPass), false)
	wantRefusal(t, err, CodePortProofFailed)
	if !res.RolledBack {
		t.Fatal("not rolled back")
	}
	if _, after := readConfig(t, sys.dir); !bytes.Equal(before, after) {
		t.Error("config.yaml was not put back")
	}
	if sys.svc.State != "running" {
		t.Errorf("the service is %s after the rollback, want running", sys.svc.State)
	}
	if _, err := testEnv(sys, fdb).daemonFor(cur).login(context.Background(), cur.Auth.Username, cur.Auth.Password); err != nil {
		t.Errorf("the old credential does not log in after the rollback: %v", err)
	}
}

// Every refusal comes before the first write.
func TestApplyAuth_Refusals(t *testing.T) {
	withUser := func(u string) AuthInput { in := authInput(testNewAuthPass); in.Auth.Username = u; return in }
	tests := []struct {
		name  string
		in    AuthInput
		setup func(*fakeSys)
		code  string
	}{
		{"an empty password", authInput(""), nil, CodeAuthPasswordWeak},
		{"a short password", authInput("123456"), nil, CodeAuthPasswordWeak},
		{"an edge space", authInput(" " + testNewAuthPass), nil, CodeAuthPasswordWeak},
		{"a control character", authInput(testNewAuthPass + "\n"), nil, CodeAuthPasswordWeak},
		{"too long", authInput(strings.Repeat("x", 257)), nil, CodeAuthPasswordWeak},
		{"a bad user name", withUser("digi hub"), nil, CodeSpecInvalid},
		{"version", AuthInput{Version: 2, ChangeSet: "cs-auth-00001", Auth: AuthChange{Password: testNewAuthPass}}, nil, CodeSpecInvalid},
		{"a bad change set", AuthInput{Version: 1, ChangeSet: "x", Auth: AuthChange{Password: testNewAuthPass}}, nil, CodeChangeSetID},
		{"orders pending", authInput(testNewAuthPass),
			func(s *fakeSys) { s.daemon.set(func(d *fakeDaemon) { d.queued = 2 }) }, CodeOrdersPending},
		{"queue unreadable", authInput(testNewAuthPass),
			func(s *fakeSys) { s.daemon.set(func(d *fakeDaemon) { d.pass = "the-daemon-runs-another-one" }) }, CodePendingUnknown},
		{"drift", func() AuthInput {
			in := authInput(testNewAuthPass)
			in.Expect = &Expect{ConfigSHA256: ptr(strings.Repeat("0", 64))}
			return in
		}(), nil, CodeDrift},
		{"service missing", authInput(testNewAuthPass), func(s *fakeSys) { s.svc.Installed = false }, CodeServiceMissing},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sys, fdb, _ := handInstallBox(t)
			if tc.setup != nil {
				tc.setup(sys)
			}
			_, before := readConfig(t, sys.dir)
			res, err := ApplyAuth(context.Background(), testEnv(sys, fdb), tc.in, false)
			wantRefusal(t, err, tc.code)
			if _, after := readConfig(t, sys.dir); !bytes.Equal(before, after) {
				t.Error("config.yaml was written on a refusal")
			}
			if res.Restarted || res.PreimageCaptured || res.RolledBack || len(res.Steps) != 0 {
				t.Errorf("work was done before the refusal: %+v", res)
			}
		})
	}

	t.Run("no config.yaml", func(t *testing.T) {
		sys, fdb, _ := handInstallBox(t)
		if err := os.Remove(filepath.Join(sys.dir, configFile)); err != nil {
			t.Fatal(err)
		}
		_, err := ApplyAuth(context.Background(), testEnv(sys, fdb), authInput(testNewAuthPass), false)
		wantRefusal(t, err, CodeConfigRefused)
		if _, err := os.Stat(filepath.Join(sys.dir, configFile)); err == nil {
			t.Error("a config.yaml was created")
		}
	})
	t.Run("assumeIdle gets past an unreadable queue", func(t *testing.T) {
		sys, fdb, _ := handInstallBox(t)
		sys.daemon.set(func(d *fakeDaemon) { d.pass = "the-daemon-runs-another-one" })
		in := authInput(testNewAuthPass)
		in.AssumeIdle = true
		res, err := ApplyAuth(context.Background(), testEnv(sys, fdb), in, false)
		if err != nil || len(res.Warnings) == 0 {
			t.Fatalf("assumeIdle: %v, warnings %v", err, res.Warnings)
		}
	})
}

// The command line: auth [--check] --spec -, strict stdin with no place for a
// database credential, the password never in the output.
func TestRun_Auth(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	env := testEnv(sys, fdb)

	in := `{"version":1,"changeSet":"cs-auth-00002","auth":{"password":"` + testNewAuthPass + `"},"rotateSecret":true}`
	out, code := Run(context.Background(), env, []string{"auth", "--spec", "-"}, strings.NewReader(in))
	var e struct {
		Command string `json:"command"`
		OK      bool   `json:"ok"`
	}
	if err := json.Unmarshal(out, &e); err != nil || code != ExitOK || !e.OK || e.Command != "auth" {
		t.Fatalf("code %d, out %s", code, out)
	}
	cfg, _ := readConfig(t, sys.dir)
	if cfg.Auth.Password != testNewAuthPass {
		t.Error("the password was not set")
	}
	if bytes.Contains(out, []byte(testNewAuthPass)) || bytes.Contains(out, []byte(cfg.Auth.Secret)) {
		t.Errorf("a secret is in the output: %s", out)
	}

	withDB := `{"version":1,"changeSet":"cs-auth-00003","auth":{"password":"` + testNewAuthPass + `"},"db":{"password":"x"}}`
	out, code = Run(context.Background(), env, []string{"auth", "--check", "--spec", "-"}, strings.NewReader(withDB))
	if code != ExitRefused || !bytes.Contains(out, []byte(CodeSpecInvalid)) {
		t.Errorf("a DB credential in the input: code %d, out %s", code, out)
	}
	weak := `{"version":1,"changeSet":"cs-auth-00003","auth":{"password":"short-secret"}}`
	out, code = Run(context.Background(), env, []string{"auth", "--spec", "-"}, strings.NewReader(weak))
	if code != ExitRefused || !bytes.Contains(out, []byte(CodeAuthPasswordWeak)) || bytes.Contains(out, []byte("short-secret")) {
		t.Errorf("a weak password: code %d, out %s", code, out)
	}
	if _, code := Run(context.Background(), env, []string{"auth"}, strings.NewReader(in)); code != ExitUsage {
		t.Errorf("auth without --spec -: code %d, want usage", code)
	}
}
