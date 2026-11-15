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

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

func TestControlDaemon(t *testing.T) {
	tests := []struct {
		name      string
		in        DaemonInput
		setup     func(*fakeSys)
		calls     []string // the service calls, in order
		state     string   // the service after
		restarted bool
		proof     bool
		code      string // a refusal
	}{
		{name: "restart a running service", in: DaemonInput{Action: DaemonRestart},
			calls: []string{"stop", "start"}, state: "running", restarted: true, proof: true},
		{name: "restart a stopped service starts it", in: DaemonInput{Action: DaemonRestart},
			setup: func(s *fakeSys) { s.svc.State = "stopped" }, calls: []string{"start"}, state: "running", restarted: true, proof: true},
		{name: "stop", in: DaemonInput{Action: DaemonStop}, calls: []string{"stop"}, state: "stopped"},
		{name: "stop a stopped service", in: DaemonInput{Action: DaemonStop},
			setup: func(s *fakeSys) { s.svc.State = "stopped" }, calls: nil, state: "stopped"},
		{name: "start a stopped service", in: DaemonInput{Action: DaemonStart},
			setup: func(s *fakeSys) { s.svc.State = "stopped" }, calls: []string{"start"}, state: "running", restarted: true, proof: true},
		{name: "start a running service only proves it", in: DaemonInput{Action: DaemonStart},
			calls: nil, state: "running", proof: true},
		{name: "orders pending refuse a restart", in: DaemonInput{Action: DaemonRestart},
			setup: func(s *fakeSys) { s.daemon.set(func(d *fakeDaemon) { d.running = 1 }) }, state: "running", code: CodeOrdersPending},
		{name: "orders pending refuse a stop", in: DaemonInput{Action: DaemonStop},
			setup: func(s *fakeSys) { s.daemon.set(func(d *fakeDaemon) { d.queued = 3 }) }, state: "running", code: CodeOrdersPending},
		{name: "an unreadable queue refuses a restart", in: DaemonInput{Action: DaemonRestart},
			setup: func(s *fakeSys) { s.daemon.set(func(d *fakeDaemon) { d.pass = "another" }) }, state: "running", code: CodePendingUnknown},
		{name: "assume-idle gets past it", in: DaemonInput{Action: DaemonRestart, AssumeIdle: true},
			setup: func(s *fakeSys) { s.daemon.set(func(d *fakeDaemon) { d.pass = "another" }) },
			calls: []string{"stop", "start"}, state: "running", restarted: true, proof: true},
		{name: "a service that does not come up", in: DaemonInput{Action: DaemonRestart},
			setup: func(s *fakeSys) {
				s.onStart = func(config.Config) { s.daemon.set(func(d *fakeDaemon) { d.down = true }) }
			},
			calls: []string{"stop", "start"}, state: "running", restarted: true, code: CodePortProofFailed},
		{name: "not installed", in: DaemonInput{Action: DaemonStart},
			setup: func(s *fakeSys) { s.svc.Installed = false }, state: "running", code: CodeServiceMissing},
		{name: "an unknown action", in: DaemonInput{Action: "pause"}, state: "running", code: CodeSpecInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sys, fdb, _ := handInstallBox(t)
			if tc.setup != nil {
				tc.setup(sys)
			}
			_, before := readConfig(t, sys.dir)
			res, err := ControlDaemon(context.Background(), testEnv(sys, fdb), tc.in)
			if tc.code != "" {
				wantRefusal(t, err, tc.code)
			} else if err != nil {
				t.Fatalf("ControlDaemon: %v", err)
			}
			if !slices.Equal(sys.calls, tc.calls) {
				t.Errorf("calls %v, want %v", sys.calls, tc.calls)
			}
			if sys.svc.State != tc.state {
				t.Errorf("the service is %s, want %s", sys.svc.State, tc.state)
			}
			if res.Restarted != tc.restarted || (res.Proof != nil) != tc.proof {
				t.Errorf("restarted %v (want %v), proof %+v (want %v)", res.Restarted, tc.restarted, res.Proof, tc.proof)
			}
			if _, after := readConfig(t, sys.dir); !bytes.Equal(before, after) {
				t.Error("config.yaml was written")
			}
			if list := preimages(sys.dir); len(list) != 0 {
				t.Errorf("pre-images %v; the daemon command writes none", list)
			}
		})
	}
}

// A start or restart never stops a running service onto a config it would
// refuse, and a stop does not need a config at all with --assume-idle.
func TestControlDaemon_ConfigGuards(t *testing.T) {
	t.Run("a config the daemon refuses", func(t *testing.T) {
		sys, fdb, _ := handInstallBox(t)
		cfg, _ := readConfig(t, sys.dir)
		cfg.Auth.Password = "" // the daemon refuses blanks
		writeConfig(t, sys.dir, cfg)
		_, err := ControlDaemon(context.Background(), testEnv(sys, fdb), DaemonInput{Action: DaemonRestart, AssumeIdle: true})
		wantRefusal(t, err, CodeConfigRefused)
		if len(sys.calls) != 0 || sys.svc.State != "running" {
			t.Errorf("the service was touched: %v", sys.calls)
		}
	})
	t.Run("no config", func(t *testing.T) {
		sys, fdb, _ := handInstallBox(t)
		sys.svc.State = "stopped"
		_ = os.Remove(filepath.Join(sys.dir, configFile))
		_, err := ControlDaemon(context.Background(), testEnv(sys, fdb), DaemonInput{Action: DaemonStart})
		wantRefusal(t, err, CodeConfigRefused)
	})
	t.Run("a config that does not parse: stop needs assume-idle", func(t *testing.T) {
		sys, fdb, _ := handInstallBox(t)
		if err := os.WriteFile(filepath.Join(sys.dir, configFile), []byte("auth: [not: yaml"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := ControlDaemon(context.Background(), testEnv(sys, fdb), DaemonInput{Action: DaemonStop})
		wantRefusal(t, err, CodePendingUnknown)
		res, err := ControlDaemon(context.Background(), testEnv(sys, fdb), DaemonInput{Action: DaemonStop, AssumeIdle: true})
		if err != nil || sys.svc.State != "stopped" || len(res.Warnings) == 0 {
			t.Errorf("stop --assume-idle: %v, state %s, warnings %v", err, sys.svc.State, res.Warnings)
		}
	})
	t.Run("another run holds the lock", func(t *testing.T) {
		sys, fdb, _ := handInstallBox(t)
		unlock, err := lockRuns(sys.dir)
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		_, err = ControlDaemon(context.Background(), testEnv(sys, fdb), DaemonInput{Action: DaemonRestart})
		wantRefusal(t, err, CodeBusy)
	})
}

func TestRun_Daemon(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	env := testEnv(sys, fdb)
	out, code := Run(context.Background(), env, []string{"daemon", "restart", "--assume-idle"}, strings.NewReader("{}"))
	var e struct {
		Command string       `json:"command"`
		OK      bool         `json:"ok"`
		Result  *ApplyResult `json:"result"`
	}
	if err := json.Unmarshal(out, &e); err != nil || code != ExitOK || !e.OK || e.Command != "daemon" || !e.Result.Restarted {
		t.Fatalf("code %d, out %s", code, out)
	}
	for _, bad := range [][]string{{"daemon"}, {"daemon", "pause"}, {"daemon", "--assume-idle"}, {"daemon", "stop", "--force"},
		{"daemon", "stop", "--assume-idle", "x"}} {
		if _, code := Run(context.Background(), env, bad, strings.NewReader("")); code != ExitUsage {
			t.Errorf("%v: code %d, want usage", bad, code)
		}
	}
}
