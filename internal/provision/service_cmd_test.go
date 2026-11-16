package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/autostart"
)

// bflBox is the box of plan 083 §19: the service depends on MSSQLSERVER while
// the database is the named instance WIZSOFT2017.
func bflBox(t *testing.T) (*fakeSys, *fakeDB) {
	sys, fdb, _ := handInstallBox(t)
	sys.svc.Dependencies = []string{"MSSQLSERVER"}
	sys.instances = []SQLInstance{
		{Name: "MSSQLSERVER", Service: "MSSQLSERVER", TCPEnabled: true, StaticPort: 1433},
		{Name: "WIZSOFT2017", Service: "MSSQL$WIZSOFT2017", TCPEnabled: true, DynamicPort: 63501},
	}
	return sys, fdb
}

func serviceInput(deps []string, recovery bool) ServiceInput {
	in := ServiceInput{Version: 1, ChangeSet: "cs-service-001", Recovery: recovery}
	if deps != nil {
		in.DependOn = &deps
	}
	return in
}

func TestApplyService_FixesTheDependencyWithoutARestart(t *testing.T) {
	sys, fdb := bflBox(t)
	env := testEnv(sys, fdb)
	_, before := readConfig(t, sys.dir)

	chk, err := ApplyService(context.Background(), env, serviceInput([]string{"MSSQL$WIZSOFT2017"}, true), true)
	if err != nil || chk.Check == nil || !chk.Check.Dependencies || !chk.Check.Recovery || chk.Check.Restart {
		t.Fatalf("--check = %+v, %v", chk.Check, err)
	}
	if len(sys.calls) != 0 {
		t.Fatalf("--check did something: %v", sys.calls)
	}
	res, err := ApplyService(context.Background(), env, serviceInput([]string{"MSSQL$wizsoft2017"}, true), false)
	if err != nil {
		t.Fatalf("ApplyService: %v", err)
	}
	if !slices.Equal(sys.svc.Dependencies, []string{"MSSQL$WIZSOFT2017"}) || !sys.svc.RecoveryPlanned {
		t.Errorf("dependencies %v, recovery %v", sys.svc.Dependencies, sys.svc.RecoveryPlanned)
	}
	if res.Restarted || slices.Contains(sys.calls, "stop") || slices.Contains(sys.calls, "start") {
		t.Errorf("the service was restarted: %v", sys.calls)
	}
	if _, after := readConfig(t, sys.dir); !bytes.Equal(before, after) {
		t.Error("config.yaml was written")
	}
	if !res.PreimageCaptured {
		t.Error("no pre-image")
	}

	// The same again changes nothing and keeps no pre-image of its own.
	again, err := ApplyService(context.Background(), env, ServiceInput{Version: 1, ChangeSet: "cs-service-002",
		DependOn: &[]string{"MSSQL$WIZSOFT2017"}, Recovery: true}, false)
	if err != nil || again.PreimageCaptured {
		t.Errorf("a no-op: %v, pre-image %v", err, again.PreimageCaptured)
	}

	// restore puts MSSQLSERVER back without a restart and without asking
	// the queue (nothing the service reads changed).
	sys.calls = nil
	sys.daemon.set(func(d *fakeDaemon) { d.queued = 4 })
	r, err := Restore(context.Background(), env, RestoreInput{ChangeSet: "cs-service-001"})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !slices.Equal(sys.svc.Dependencies, []string{"MSSQLSERVER"}) {
		t.Errorf("restored dependencies %v", sys.svc.Dependencies)
	}
	if r.Restarted || slices.Contains(sys.calls, "stop") || slices.Contains(sys.calls, "start") {
		t.Errorf("restore restarted the service: %v", sys.calls)
	}
}

func TestApplyService_RecoveryOnly(t *testing.T) {
	sys, fdb := bflBox(t)
	res, err := ApplyService(context.Background(), testEnv(sys, fdb), serviceInput(nil, true), false)
	if err != nil || !sys.svc.RecoveryPlanned {
		t.Fatalf("recovery: %v, planned %v", err, sys.svc.RecoveryPlanned)
	}
	if !slices.Equal(sys.svc.Dependencies, []string{"MSSQLSERVER"}) {
		t.Errorf("dependencies changed to %v", sys.svc.Dependencies)
	}
	p, err := loadPreimage(preimageDir(sys.dir, "cs-service-001"))
	if err != nil || p.m.Service != nil || len(p.m.Files) != 0 {
		t.Errorf("pre-image %+v, %v; want one with nothing to put back", p.m, err)
	}
	_ = res
}

func TestApplyService_NoDependencies(t *testing.T) {
	sys, fdb := bflBox(t)
	if _, err := ApplyService(context.Background(), testEnv(sys, fdb), serviceInput([]string{}, false), false); err != nil {
		t.Fatalf("ApplyService: %v", err)
	}
	if len(sys.svc.Dependencies) != 0 {
		t.Errorf("dependencies %v, want none", sys.svc.Dependencies)
	}
}

func TestApplyService_Refusals(t *testing.T) {
	tests := []struct {
		name  string
		in    ServiceInput
		setup func(*fakeSys)
		code  string
	}{
		{"nothing to change", serviceInput(nil, false), nil, CodeSpecInvalid},
		{"not a SQL Server service", serviceInput([]string{"Spooler"}, false), nil, CodeSpecInvalid},
		{"too many", serviceInput(slices.Repeat([]string{"MSSQLSERVER"}, MaxDependencies+1), false), nil, CodeSpecInvalid},
		{"a bad change set", ServiceInput{Version: 1, ChangeSet: "x", Recovery: true}, nil, CodeChangeSetID},
		{"version", ServiceInput{Version: 3, ChangeSet: "cs-service-001", Recovery: true}, nil, CodeSpecInvalid},
		{"an instance not on this machine", serviceInput([]string{"MSSQL$WIZSOFT"}, true), nil, CodeDependencyMissing},
		{"service missing", serviceInput([]string{"MSSQL$WIZSOFT2017"}, false), func(s *fakeSys) { s.svc.Installed = false }, CodeServiceMissing},
		// review m1: a Disabled dependency would stop the connector at the next start or reboot, unseen by this command
		{"a disabled dependency", serviceInput([]string{"MSSQL$WIZSOFT2017"}, true), func(s *fakeSys) {
			s.depState = map[string]autostart.ServiceInfo{"MSSQL$WIZSOFT2017": {Installed: true, State: "stopped", StartType: "disabled"}}
		}, CodeDependencyDisabled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sys, fdb := bflBox(t)
			if tc.setup != nil {
				tc.setup(sys)
			}
			res, err := ApplyService(context.Background(), testEnv(sys, fdb), tc.in, false)
			wantRefusal(t, err, tc.code)
			if len(sys.calls) != 0 || res.PreimageCaptured || len(res.Steps) != 0 {
				t.Errorf("work was done before the refusal: calls %v, %+v", sys.calls, res)
			}
		})
	}
}

// A dependency that is installed but not running (Manual, stopped) is accepted with a warning: the service manager
// starts it with the connector.
func TestApplyService_WarnsOfAStoppedDependency(t *testing.T) {
	sys, fdb := bflBox(t)
	sys.depState = map[string]autostart.ServiceInfo{"MSSQL$WIZSOFT2017": {Installed: true, State: "stopped", StartType: "manual"}}
	for _, check := range []bool{true, false} {
		res, err := ApplyService(context.Background(), testEnv(sys, fdb), serviceInput([]string{"MSSQL$WIZSOFT2017"}, false), check)
		if err != nil {
			t.Fatalf("check %v: %v", check, err)
		}
		if !slices.ContainsFunc(res.Warnings, func(w string) bool { return strings.Contains(w, "MSSQL$WIZSOFT2017 is stopped") }) {
			t.Errorf("check %v: warnings %v", check, res.Warnings)
		}
	}
}

func TestRun_Service(t *testing.T) {
	sys, fdb := bflBox(t)
	env := testEnv(sys, fdb)
	out, code := Run(context.Background(), env, []string{"service", "--spec", "-"},
		strings.NewReader(`{"version":1,"changeSet":"cs-service-003","dependOn":["MSSQL$WIZSOFT2017"],"recovery":true}`))
	var e Envelope
	if err := json.Unmarshal(out, &e); err != nil || code != ExitOK || !e.OK || e.Command != "service" {
		t.Fatalf("code %d, out %s", code, out)
	}
	out, code = Run(context.Background(), env, []string{"service", "--check", "--spec", "-"},
		strings.NewReader(`{"version":1,"changeSet":"cs-service-004","dependOn":["MSSQLSERVER"],"startType":"auto"}`))
	if code != ExitRefused || !bytes.Contains(out, []byte(CodeSpecInvalid)) {
		t.Errorf("an unknown field: code %d, out %s", code, out)
	}
	if _, code := Run(context.Background(), env, []string{"service"}, strings.NewReader("{}")); code != ExitUsage {
		t.Errorf("service without --spec -: code %d", code)
	}
}
