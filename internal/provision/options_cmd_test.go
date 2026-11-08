package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

func optionsInput(o OptionsSpec) OptionsInput {
	return OptionsInput{Version: 1, ChangeSet: "cs-options-0001", Options: o}
}

// The options command changes the options and nothing else: the credential a
// caller already uses, the database, the listener and the secret stay byte for
// byte, and the restart is proved with the box's own credential.
func TestApplyOptions_SetsOnlyTheOptions(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	env := testEnv(sys, fdb)
	before, _ := readConfig(t, sys.dir)
	img := filepath.Join(t.TempDir(), "תמונות")
	if err := os.MkdirAll(img, 0o700); err != nil {
		t.Fatal(err)
	}

	res, err := ApplyOptions(context.Background(), env, optionsInput(OptionsSpec{
		ImageFolders: &[]string{img}, QueryMaxRows: ptr(50000), QueryTimeoutSeconds: ptr(120),
	}), false)
	if err != nil {
		t.Fatalf("ApplyOptions: %v", err)
	}
	after, _ := readConfig(t, sys.dir)
	want := before
	want.ImageFolders = []string{img}
	want.Queries = config.QueriesConfig{TimeoutSeconds: 120, MaxRows: 50000}
	wantBytes, _ := config.Marshal(want)
	gotBytes, _ := config.Marshal(after)
	if !bytes.Equal(wantBytes, gotBytes) {
		t.Errorf("config.yaml changed beyond the options:\n got %s\nwant %s", gotBytes, wantBytes)
	}
	if after.Auth != before.Auth {
		t.Error("the credential changed")
	}
	if !res.Restarted || res.Proof == nil || !res.PreimageCaptured {
		t.Errorf("restarted %v, proof %+v, pre-image %v", res.Restarted, res.Proof, res.PreimageCaptured)
	}
	if len(fdb.opened) > 0 {
		t.Errorf("the database was opened as %v; options never touch it", fdb.opened)
	}
}

func TestApplyOptions_SameValuesNoRestart(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	sys.svc.ProcessStart = time.Now().Add(time.Minute) // the daemon runs the file on disk
	env := testEnv(sys, fdb)
	_, before := readConfig(t, sys.dir)
	cur, _ := readConfig(t, sys.dir)

	res, err := ApplyOptions(context.Background(), env, optionsInput(OptionsSpec{Debug: ptr(cur.Debug)}), false)
	if err != nil {
		t.Fatalf("ApplyOptions: %v", err)
	}
	if _, after := readConfig(t, sys.dir); !bytes.Equal(before, after) {
		t.Error("config.yaml was rewritten for a no-op")
	}
	if res.Restarted || res.PreimageCaptured {
		t.Errorf("a no-op restarted (%v) or kept a pre-image (%v)", res.Restarted, res.PreimageCaptured)
	}
	chk, err := ApplyOptions(context.Background(), env, optionsInput(OptionsSpec{Debug: ptr(cur.Debug)}), true)
	if err != nil || chk.Check == nil || chk.Check.Config != "unchanged" || chk.Check.Restart {
		t.Errorf("--check of the same values = %+v, %v; want unchanged and no restart", chk.Check, err)
	}
}

func TestApplyOptions_CheckWritesNothing(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	env := testEnv(sys, fdb)
	_, before := readConfig(t, sys.dir)

	res, err := ApplyOptions(context.Background(), env, optionsInput(OptionsSpec{QueryMaxRows: ptr(50000)}), true)
	if err != nil {
		t.Fatalf("ApplyOptions --check: %v", err)
	}
	if _, after := readConfig(t, sys.dir); !bytes.Equal(before, after) {
		t.Error("--check wrote config.yaml")
	}
	if res.Check == nil || res.Check.Config != "changed: options" || !res.Check.Restart {
		t.Errorf("check = %+v, want config changed: options and a restart", res.Check)
	}
	if res.Restarted || res.PreimageCaptured {
		t.Error("--check restarted or captured")
	}
}

func TestApplyOptions_RestoreUndoesIt(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	env := testEnv(sys, fdb)
	_, before := readConfig(t, sys.dir)
	if _, err := ApplyOptions(context.Background(), env, optionsInput(OptionsSpec{QueryMaxRows: ptr(50000)}), false); err != nil {
		t.Fatalf("ApplyOptions: %v", err)
	}
	if _, err := Restore(context.Background(), env, RestoreInput{ChangeSet: "cs-options-0001"}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, after := readConfig(t, sys.dir); !bytes.Equal(before, after) {
		t.Error("restore did not put config.yaml back")
	}
}

// Every refusal comes before the first write.
func TestApplyOptions_Refusals(t *testing.T) {
	missing := filepath.Join(os.TempDir(), "no-such-image-folder-plan068")
	tests := []struct {
		name  string
		in    OptionsInput
		setup func(*fakeSys)
		code  string
	}{
		{"no option", optionsInput(OptionsSpec{}), nil, CodeSpecInvalid},
		{"a bound", optionsInput(OptionsSpec{QueryMaxRows: ptr(100001)}), nil, CodeSpecInvalid},
		{"a bad change set", OptionsInput{Version: 1, ChangeSet: "x", Options: OptionsSpec{Debug: ptr(true)}}, nil, CodeChangeSetID},
		{"version", OptionsInput{Version: 2, ChangeSet: "cs-options-0001", Options: OptionsSpec{Debug: ptr(true)}}, nil, CodeSpecInvalid},
		{"a missing folder", optionsInput(OptionsSpec{ImageFolders: &[]string{missing}}), nil, CodeImageFolderMissing},
		{"orders pending", optionsInput(OptionsSpec{Debug: ptr(true)}),
			func(s *fakeSys) { s.daemon.set(func(d *fakeDaemon) { d.queued = 1 }) }, CodeOrdersPending},
		{"drift", OptionsInput{Version: 1, ChangeSet: "cs-options-0001", Options: OptionsSpec{Debug: ptr(true)},
			Expect: &Expect{ConfigSHA256: ptr(strings.Repeat("0", 64))}}, nil, CodeDrift},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sys, fdb, _ := handInstallBox(t)
			if tc.setup != nil {
				tc.setup(sys)
			}
			_, before := readConfig(t, sys.dir)
			res, err := ApplyOptions(context.Background(), testEnv(sys, fdb), tc.in, false)
			wantRefusal(t, err, tc.code)
			if _, after := readConfig(t, sys.dir); !bytes.Equal(before, after) {
				t.Error("config.yaml was written on a refusal")
			}
			if res.Restarted || res.PreimageCaptured || res.RolledBack || len(res.Steps) != 0 {
				t.Errorf("work was done before the refusal: restarted %v, pre-image %v, rolled back %v, steps %+v",
					res.Restarted, res.PreimageCaptured, res.RolledBack, res.Steps)
			}
		})
	}

	t.Run("no config.yaml", func(t *testing.T) {
		sys, fdb, _ := handInstallBox(t)
		if err := os.Remove(filepath.Join(sys.dir, configFile)); err != nil {
			t.Fatal(err)
		}
		_, err := ApplyOptions(context.Background(), testEnv(sys, fdb), optionsInput(OptionsSpec{Debug: ptr(true)}), false)
		wantRefusal(t, err, CodeConfigRefused)
		if _, err := os.Stat(filepath.Join(sys.dir, configFile)); err == nil {
			t.Error("a config.yaml was created")
		}
	})
}

// The command line: options [--check] --spec -, strict stdin with no place
// for a credential, one envelope out.
func TestRun_Options(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	env := testEnv(sys, fdb)

	in := `{"version":1,"changeSet":"cs-options-0002","options":{"queryMaxRows":50000}}`
	out, code := Run(context.Background(), env, []string{"options", "--spec", "-"}, strings.NewReader(in))
	var e struct {
		Command string `json:"command"`
		OK      bool   `json:"ok"`
	}
	if err := json.Unmarshal(out, &e); err != nil || code != ExitOK || !e.OK || e.Command != "options" {
		t.Fatalf("code %d, out %s", code, out)
	}
	if cfg, _ := readConfig(t, sys.dir); cfg.Queries.MaxRows != 50000 {
		t.Errorf("maxRows = %d", cfg.Queries.MaxRows)
	}

	withAuth := `{"version":1,"changeSet":"cs-options-0003","options":{"debug":true},"auth":{"username":"x","password":"y"}}`
	out, code = Run(context.Background(), env, []string{"options", "--spec", "-"}, strings.NewReader(withAuth))
	if code != ExitRefused || !bytes.Contains(out, []byte(CodeSpecInvalid)) {
		t.Errorf("a credential in the input: code %d, out %s", code, out)
	}

	if _, code := Run(context.Background(), env, []string{"options"}, strings.NewReader(in)); code != ExitUsage {
		t.Errorf("options without --spec -: code %d, want usage", code)
	}
	if !slices.Contains(strings.Split(Usage, "\n"), "  digi-erp-connector-provision options [--check] --spec - set config.yaml options only (image folders, query limits, ...); no credential on stdin") {
		t.Error("the usage text does not name the options command")
	}
}

// Same values on a daemon older than config.yaml (a hand edit it never loaded):
// nothing is written, and the restart brings the daemon onto the file.
func TestApplyOptions_SameValuesStaleDaemonRestarts(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	sys.svc.ProcessStart = time.Now().Add(-time.Hour)
	env := testEnv(sys, fdb)
	_, before := readConfig(t, sys.dir)
	cur, _ := readConfig(t, sys.dir)

	res, err := ApplyOptions(context.Background(), env, optionsInput(OptionsSpec{Debug: ptr(cur.Debug)}), false)
	if err != nil {
		t.Fatalf("ApplyOptions: %v", err)
	}
	if _, after := readConfig(t, sys.dir); !bytes.Equal(before, after) {
		t.Error("config.yaml was rewritten for a no-op")
	}
	if !res.Restarted || res.Proof == nil {
		t.Errorf("a stale daemon was not restarted onto the file: restarted %v", res.Restarted)
	}
}

// Review finding: a no-op on a stopped service must not start it — nothing
// would be rolled back if the proof failed.
func TestApplyOptions_NoOpLeavesAStoppedServiceStopped(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	sys.svc.State = "stopped"
	cur, _ := readConfig(t, sys.dir)
	env := testEnv(sys, fdb)

	chk, err := ApplyOptions(context.Background(), env, optionsInput(OptionsSpec{Debug: ptr(cur.Debug)}), true)
	if err != nil || chk.Check == nil || chk.Check.Restart {
		t.Fatalf("--check = %+v, %v; want no restart for a no-op on a stopped service", chk.Check, err)
	}
	res, err := ApplyOptions(context.Background(), env, optionsInput(OptionsSpec{Debug: ptr(cur.Debug)}), false)
	if err != nil {
		t.Fatalf("ApplyOptions: %v", err)
	}
	if res.Restarted || sys.svc.State != "stopped" || slices.Contains(sys.calls, "start") {
		t.Errorf("a no-op started a stopped service: restarted %v, state %s, calls %v", res.Restarted, sys.svc.State, sys.calls)
	}
}

// Review finding: a failed proof's rollback must leave the DB secret exactly as
// the pre-image had it, even when the login's stored key is older (the GUI
// re-typed the secret and never updates the key).
func TestApplyOptions_RollbackKeepsTheDBSecretInUse(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	cfg, _ := readConfig(t, sys.dir)
	cfg.DB.User = "erp_connector_read"
	writeConfig(t, sys.dir, cfg)
	key := config.DBPasswordKey(cfg.ERP)
	if err := sys.SecretSet(key, []byte("Pw-typed-in-gui-2")); err != nil {
		t.Fatal(err)
	}
	if err := sys.SecretSet(loginKey("erp_connector_read"), []byte("Pw-old-stale-1")); err != nil {
		t.Fatal(err)
	}
	sys.onStart = func(c config.Config) { sys.daemon.set(func(d *fakeDaemon) { d.down = c.Debug }) }

	res, err := ApplyOptions(context.Background(), testEnv(sys, fdb), optionsInput(OptionsSpec{Debug: ptr(!cfg.Debug)}), false)
	wantRefusal(t, err, CodePortProofFailed)
	if !res.RolledBack {
		t.Fatal("not rolled back")
	}
	if got, _ := sys.SecretGet(key); string(got) != "Pw-typed-in-gui-2" {
		t.Errorf("the DB secret after the rollback is %q, want the one in use", got)
	}
}

// Review finding, restore's side: undoing an options change set on the same
// box keeps the DB secret in use, not the login's older stored key.
func TestRestore_KeepsTheDBSecretInUseForTheSameLogin(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	cfg, _ := readConfig(t, sys.dir)
	cfg.DB.User = "erp_connector_read"
	writeConfig(t, sys.dir, cfg)
	key := config.DBPasswordKey(cfg.ERP)
	_ = sys.SecretSet(key, []byte("Pw-typed-in-gui-2"))
	_ = sys.SecretSet(loginKey("erp_connector_read"), []byte("Pw-old-stale-1"))
	env := testEnv(sys, fdb)
	if _, err := ApplyOptions(context.Background(), env, optionsInput(OptionsSpec{QueryMaxRows: ptr(50000)}), false); err != nil {
		t.Fatalf("ApplyOptions: %v", err)
	}
	if _, err := Restore(context.Background(), env, RestoreInput{ChangeSet: "cs-options-0001"}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got, _ := sys.SecretGet(key); string(got) != "Pw-typed-in-gui-2" {
		t.Errorf("the DB secret after restore is %q, want the one in use", got)
	}
}
