package provision

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

// boxWithOldRules is a box moved onto the link that still has the inbound
// rules of its WinRM days, and a client's rule of its own.
func boxWithOldRules(t *testing.T) (*fakeSys, *fakeDB) {
	sys, fdb, _ := handInstallBox(t)
	linkBox(t, sys)
	sys.rules["digi-erp-connector"] = FirewallRule{Name: "digi-erp-connector", Enabled: true, Port: 8082,
		RemoteAddress: []string{"198.51.100.241"}, Ours: true}
	sys.rules["digi-erp-connector 8083"] = FirewallRule{Name: "digi-erp-connector 8083", Enabled: true, Port: 8083,
		RemoteAddress: []string{"198.51.100.241", "203.0.113.0/24"}, Ours: true}
	sys.rules["Client SQL"] = FirewallRule{Name: "Client SQL", Enabled: true, Port: 1433, Ours: false}
	return sys, fdb
}

func fwInput(names ...string) FirewallRemoveInput {
	return FirewallRemoveInput{Version: 1, ChangeSet: "cs-fw-000001", Names: names}
}

func TestRemoveFirewallRules_RemovesOursAndRestoreBringsThemBack(t *testing.T) {
	sys, fdb := boxWithOldRules(t)
	env := testEnv(sys, fdb)
	old8082, old8083 := sys.rules["digi-erp-connector"], sys.rules["digi-erp-connector 8083"]

	chk, err := RemoveFirewallRules(context.Background(), env, fwInput("digi-erp-connector", "digi-erp-connector 8083", "never-existed"), true)
	if err != nil || chk.Check == nil || chk.Check.Firewall != "remove" ||
		!slices.Equal(chk.Check.Rules, []string{"digi-erp-connector", "digi-erp-connector 8083"}) {
		t.Fatalf("--check = %+v, %v", chk.Check, err)
	}
	if len(sys.rules) != 3 || len(sys.calls) != 0 {
		t.Fatalf("--check removed something: %v", sys.calls)
	}

	res, err := RemoveFirewallRules(context.Background(), env, fwInput("digi-erp-connector", "digi-erp-connector 8083", "never-existed"), false)
	if err != nil {
		t.Fatalf("RemoveFirewallRules: %v", err)
	}
	if _, ok := sys.rules["digi-erp-connector"]; ok {
		t.Error("the 8082 rule is still there")
	}
	if _, ok := sys.rules["digi-erp-connector 8083"]; ok {
		t.Error("the 8083 rule is still there")
	}
	if _, ok := sys.rules["Client SQL"]; !ok {
		t.Error("the client's rule was removed")
	}
	if res.Restarted || slices.Contains(sys.calls, "stop") || !res.PreimageCaptured {
		t.Errorf("restarted %v, pre-image %v, calls %v", res.Restarted, res.PreimageCaptured, sys.calls)
	}

	// The same again removes nothing and keeps no pre-image of its own.
	again, err := RemoveFirewallRules(context.Background(), env, FirewallRemoveInput{Version: 1, ChangeSet: "cs-fw-000002",
		Names: []string{"digi-erp-connector"}}, false)
	if err != nil || again.PreimageCaptured {
		t.Errorf("a no-op: %v, pre-image %v", err, again.PreimageCaptured)
	}

	// restore re-creates both, as they were, without a restart or a question to the queue.
	sys.calls = nil
	sys.daemon.set(func(d *fakeDaemon) { d.queued = 5 })
	r, err := Restore(context.Background(), env, RestoreInput{ChangeSet: "cs-fw-000001"})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := sys.rules["digi-erp-connector"]; !got.Same(old8082) {
		t.Errorf("8082 rule after restore = %+v", got)
	}
	if got := sys.rules["digi-erp-connector 8083"]; !got.Same(old8083) {
		t.Errorf("8083 rule after restore = %+v", got)
	}
	if r.Restarted || slices.Contains(sys.calls, "stop") {
		t.Errorf("restore restarted: %v", sys.calls)
	}
}

func TestRemoveFirewallRules_Refusals(t *testing.T) {
	tests := []struct {
		name string
		in   FirewallRemoveInput
		code string
	}{
		{"the client's rule", fwInput("digi-erp-connector", "Client SQL"), CodeFirewallForeign},
		{"no names", fwInput(), CodeSpecInvalid},
		{"a name twice", fwInput("digi-erp-connector", "DIGI-ERP-CONNECTOR"), CodeSpecInvalid},
		{"not a rule name", fwInput(`x"; Remove-NetFirewallRule *`), CodeSpecInvalid},
		{"too many", fwInput(slices.Repeat([]string{"a"}, MaxFirewallNames+1)...), CodeSpecInvalid},
		{"version", FirewallRemoveInput{Version: 2, ChangeSet: "cs-fw-000001", Names: []string{"a"}}, CodeSpecInvalid},
		{"a bad change set", FirewallRemoveInput{Version: 1, ChangeSet: "cs", Names: []string{"a"}}, CodeChangeSetID},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sys, fdb := boxWithOldRules(t)
			res, err := RemoveFirewallRules(context.Background(), testEnv(sys, fdb), tc.in, false)
			wantRefusal(t, err, tc.code)
			if len(sys.rules) != 3 || len(sys.calls) != 0 || res.PreimageCaptured {
				t.Errorf("work was done before the refusal: rules %d, calls %v", len(sys.rules), sys.calls)
			}
		})
	}
}

// failingRemove fails the second removal.
type failingRemove struct {
	*fakeSys
	n int
}

func (f *failingRemove) RemoveFirewallRule(name string) error {
	f.n++
	if f.n == 2 {
		return errors.New("access denied")
	}
	return f.fakeSys.RemoveFirewallRule(name)
}

// A removal that fails half way puts back the rules this run removed.
func TestRemoveFirewallRules_FailureHalfWayPutsBack(t *testing.T) {
	sys, fdb := boxWithOldRules(t)
	env := testEnv(sys, fdb)
	env.Sys = &failingRemove{fakeSys: sys}
	res, err := RemoveFirewallRules(context.Background(), env, fwInput("digi-erp-connector", "digi-erp-connector 8083"), false)
	wantRefusal(t, err, CodeFirewallFailed)
	if !res.RolledBack {
		t.Error("not rolled back")
	}
	if _, ok := sys.rules["digi-erp-connector"]; !ok {
		t.Error("the first rule was not put back")
	}
}

func TestRun_FirewallRemove(t *testing.T) {
	sys, fdb := boxWithOldRules(t)
	env := testEnv(sys, fdb)
	out, code := Run(context.Background(), env, []string{"firewall-remove", "--spec", "-"},
		strings.NewReader(`{"version":1,"changeSet":"cs-fw-000003","names":["digi-erp-connector 8083"]}`))
	var e Envelope
	if err := json.Unmarshal(out, &e); err != nil || code != ExitOK || !e.OK {
		t.Fatalf("code %d, out %s", code, out)
	}
	if _, ok := sys.rules["digi-erp-connector 8083"]; ok {
		t.Error("not removed")
	}
	if _, code := Run(context.Background(), env, []string{"firewall-remove", "digi-erp-connector"}, strings.NewReader("")); code != ExitUsage {
		t.Errorf("a name on the command line: code %d", code)
	}
}
