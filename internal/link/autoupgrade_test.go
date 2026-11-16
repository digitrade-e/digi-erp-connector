package link

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// The box's local time in these tests: a fixed zone, two hours east of UTC. 2025-03-03 is a Monday; the clock is in
// the past so a ticket the fake runner never finishes stays pending (its job file is newer than "now").
var boxZone = time.FixedZone("IST", 2*3600)

func boxTime(day, hour, minute int) time.Time {
	return time.Date(2025, 3, day, hour, minute, 0, 0, boxZone)
}

type autoRig struct {
	*upgradeRig
	clk *fakeClock
	rs  *reports
}

func newAutoRig(t *testing.T, build string, at time.Time, extra ...func(*Config)) *autoRig {
	t.Helper()
	clk := &fakeClock{t: at}
	g := newUpgradeRig(t, build, append([]func(*Config){forTests088, withClock(clk)}, extra...)...)
	return &autoRig{upgradeRig: g, clk: clk, rs: acceptReports(t, g.rc, g.b.keys.Public())}
}

func (a *autoRig) deliver(t *testing.T, tenant, ring string, ps ...linkproto.UpgradePolicy) linkproto.Report {
	t.Helper()
	var sp []linkproto.SignedPolicy
	for _, p := range ps {
		sp = append(sp, signed(t, ed25519.PrivateKey(a.priv), "team-1", p))
	}
	deliver(t, a.rc, delivery("bfl-read", tenant, ring, sp...))
	return a.rs.next(t)
}

// policy is the rig's release 1.0.30 for every box, in a 02:00-05:00 window on days.
func (a *autoRig) policy(seq int64, days ...string) linkproto.UpgradePolicy {
	p := targetPolicy(everyBox(), seq, "1.0.30", sha(a.setup), a.clk.Now())
	if len(days) > 0 {
		p.Window.Days = days
	}
	return p
}

func (a *autoRig) auto(t *testing.T) autoFile {
	t.Helper()
	af, err := a.b.store.loadAuto()
	if err != nil {
		t.Fatal(err)
	}
	return af
}

func (a *autoRig) spawns() int {
	a.spawned.mu.Lock()
	defer a.spawned.mu.Unlock()
	return len(a.spawned.calls)
}

func (a *autoRig) lastTicket() string {
	a.spawned.mu.Lock()
	defer a.spawned.mu.Unlock()
	c := a.spawned.calls[len(a.spawned.calls)-1]
	return c[len(c)-1]
}

// finish writes the result a runner would write for ticket.
func (a *autoRig) finish(t *testing.T, res UpgradeResult) {
	t.Helper()
	b, _ := json.Marshal(res)
	_ = os.MkdirAll(filepath.Join(a.dir, "results"), 0o700)
	if err := os.WriteFile(resultPath(a.dir, res.Ticket), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTheSchedulerUpgradesInItsWindow(t *testing.T) {
	a := newAutoRig(t, "v1.0.29", boxTime(3, 3, 0))
	rep := a.deliver(t, "bflstore", "", a.policy(1, "mon"))
	if rep.Effective.Why != linkproto.WhyScheduled || rep.Effective.Version != "1.0.30" {
		t.Fatalf("%+v", rep.Effective)
	}
	next := a.b.link.autoCheck()
	if a.spawns() != 1 {
		t.Fatalf("spawned %d", a.spawns())
	}
	ticket := a.lastTicket()
	var job UpgradeJob
	b, _ := os.ReadFile(filepath.Join(a.dir, "upgrade", ticket, jobFile))
	if err := json.Unmarshal(b, &job); err != nil || !job.Auto || job.Policy == nil || job.Policy.Scope.Key() != "all" || job.Policy.Seq != 1 ||
		job.HealthMinutes != 20 || job.From != "1.0.29" || job.To != "1.0.30" || job.SHA256 != sha(a.setup) ||
		job.By != "auto-update: policy all #1 approved by ilya" {
		t.Fatalf("job %+v (%v)", job, err)
	}
	af := a.auto(t)
	if len(af.Attempts) != 1 || af.Attempts[0].Ticket != ticket || af.Attempts[0].Outcome != linkproto.OutcomePending ||
		!af.Attempts[0].WindowStart.Equal(boxTime(3, 2, 0)) {
		t.Fatalf("attempts %+v", af.Attempts)
	}
	if af.View == nil || af.View.Effective.Why != linkproto.WhyUpgrading || !af.View.NextCheck.Equal(next) || next.Sub(a.clk.Now()) != autoPendingEvery {
		t.Fatalf("view %+v, next %s", af.View, next)
	}
	// the relay hears of it before the setup stops the link
	rep = a.rs.next(t)
	if rep.AutoUpgrade == nil || rep.AutoUpgrade.Ticket != ticket || rep.AutoUpgrade.Outcome != linkproto.OutcomePending ||
		rep.AutoUpgrade.From != "1.0.29" || rep.AutoUpgrade.To != "1.0.30" {
		t.Fatalf("report %+v", rep.AutoUpgrade)
	}
	if !a.b.log.has("auto-update 1.0.29 -> 1.0.30 accepted, ticket " + ticket + " (policy all #1 approved by ilya)") {
		t.Fatal("not logged")
	}
	// while it runs, nothing more starts
	a.clk.add(time.Minute)
	a.b.link.autoCheck()
	if a.spawns() != 1 {
		t.Fatal("a second upgrade started while the first runs")
	}
}

func TestTheSchedulerWaitsForItsWindow(t *testing.T) {
	a := newAutoRig(t, "v1.0.29", boxTime(3, 6, 0)) // Monday 06:00, after the window
	a.deliver(t, "", "", a.policy(1, "mon", "tue"))
	next := a.b.link.autoCheck()
	af := a.auto(t)
	if a.spawns() != 0 || af.View.Effective.Why != linkproto.WhyScheduled || !af.View.NextWindow.Equal(boxTime(4, 2, 0)) {
		t.Fatalf("spawned %d, view %+v", a.spawns(), af.View)
	}
	if d := next.Sub(a.clk.Now()); d < 9*time.Minute || d > 11*time.Minute {
		t.Fatalf("the next check in %s", d)
	}
	a.clk.set(boxTime(4, 2, 30))
	a.b.link.autoCheck()
	if a.spawns() != 1 {
		t.Fatal("no upgrade in the window")
	}
}

func TestTheSchedulerHolds(t *testing.T) {
	at := boxTime(3, 3, 0)
	setState := func(edit func(*State)) func(*autoRig) {
		return func(a *autoRig) {
			st, _ := a.b.store.LoadState()
			edit(&st)
			_ = a.b.store.SaveState(st)
		}
	}
	off := false
	cases := []struct {
		name  string
		build string
		tune  func(*autoRig)
		why   string
		hold  bool
	}{
		{"nothing held", "v1.0.29", func(*autoRig) {}, linkproto.WhyNoPolicy, true},
		{"autoUpgrade: false", "v1.0.29", setState(func(s *State) { s.AutoUpgrade = &off }), linkproto.WhyAutoUpgradeOff, true},
		{"admin: false", "v1.0.29", setState(func(s *State) { s.Admin = &off }), linkproto.WhyAdminOff, true},
		{"a misspelt switch", "v1.0.29", func(a *autoRig) {
			st, _ := a.b.store.LoadState()
			_ = os.WriteFile(a.b.store.statePath(), []byte(fmt.Sprintf("relay: %s\nbox: %s\nautoupgrade: false\n", st.Relay, st.Box)), 0o600)
		}, linkproto.WhyStateUnreadable, true},
		{"a box list's hold over every box's target", "v1.0.29", func(a *autoRig) {
			a.deliver(t, "", "", holdPolicy(boxList("bfl-read", "bfl-write"), 1, at))
		}, linkproto.WhyPolicyHolds, true},
		{"every box's later hold over a box list's target (D10)", "v1.0.29", func(a *autoRig) {
			list := a.policy(1)
			list.Scope, list.IssuedAt = boxList("bfl-read"), at.Add(-2*time.Hour)
			a.deliver(t, "", "", list, holdPolicy(everyBox(), 2, at))
		}, linkproto.WhyPolicyHolds, true},
		// review m9: an expired policy holds for its scope, said so
		{"the policy expired", "v1.0.29", func(a *autoRig) { a.clk.add(31 * 24 * time.Hour) }, linkproto.WhyPolicyExpired, true},
		{"an expired box list's hold does not lapse into every box's target", "v1.0.29", func(a *autoRig) {
			h := holdPolicy(boxList("bfl-read"), 1, at)
			h.ExpiresAt = at.Add(time.Hour)
			a.deliver(t, "", "", a.policy(2), h)
			a.clk.add(2 * time.Hour)
		}, linkproto.WhyPolicyExpired, true},
		{"already on the target", "v1.0.30", func(*autoRig) {}, linkproto.WhyUpToDate, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := newAutoRig(t, c.build, at)
			if c.name != "nothing held" {
				a.deliver(t, "", "", a.policy(1))
			}
			c.tune(a)
			a.b.link.autoCheck()
			af := a.auto(t)
			if a.spawns() != 0 || af.View.Effective.Why != c.why || af.View.Effective.Hold != c.hold {
				t.Fatalf("spawned %d, effective %+v", a.spawns(), af.View.Effective)
			}
		})
	}
}

// D4: an upgrade waits for an empty order queue, and tries again at the next check, in the same window.
func TestTheSchedulerWaitsForAnIdleQueue(t *testing.T) {
	a := newAutoRig(t, "v1.0.29", boxTime(3, 2, 10))
	a.deliver(t, "", "", a.policy(1))
	a.runner.mu.Lock()
	a.runner.queue = `{"queued":2,"running":0}`
	a.runner.mu.Unlock()
	a.b.link.autoCheck()
	af := a.auto(t)
	if a.spawns() != 0 || len(af.Attempts) != 0 || af.Waiting == nil || af.Waiting.Code != "orders_pending" || af.View.Waiting == nil {
		t.Fatalf("spawned %d, %+v", a.spawns(), af)
	}
	a.runner.mu.Lock()
	a.runner.queue = `{"queued":0,"running":0}`
	a.runner.mu.Unlock()
	a.clk.add(10 * time.Minute)
	a.b.link.autoCheck()
	if a.spawns() != 1 || a.auto(t).Waiting != nil {
		t.Fatal("no upgrade once the queue emptied")
	}
	// another command running is "not now" too
	b := newAutoRig(t, "v1.0.29", boxTime(3, 2, 10))
	b.deliver(t, "", "", b.policy(1))
	b.b.link.cmdSlots <- struct{}{}
	b.b.link.autoCheck()
	<-b.b.link.cmdSlots
	if b.spawns() != 0 || b.auto(t).Waiting.Code != busyCode {
		t.Fatalf("beside a command: %d, %+v", b.spawns(), b.auto(t).Waiting)
	}
}

// A refused attempt waits for the next window: never more than one attempt per window per target.
func TestOneAttemptPerWindow(t *testing.T) {
	a := newAutoRig(t, "v1.0.29", boxTime(3, 2, 10))
	a.deliver(t, "", "", a.policy(1))
	setup := a.rel.files["v1.0.30/"+setupFile("1.0.30")]
	a.rel.mu.Lock()
	delete(a.rel.files, "v1.0.30/"+setupFile("1.0.30"))
	a.rel.mu.Unlock()
	a.b.link.autoCheck()
	af := a.auto(t)
	if a.spawns() != 0 || len(af.Attempts) != 1 || af.Attempts[0].Outcome != linkproto.OutcomeRefused || af.Attempts[0].Refusal.Code != "download_failed" {
		t.Fatalf("attempts %+v", af.Attempts)
	}
	if !a.b.log.has("refused: download_failed") || a.rs.next(t).AutoUpgrade.Outcome != linkproto.OutcomeRefused {
		t.Fatal("not logged or not reported")
	}
	sums := a.rel.hit("v1.0.30/SHA256SUMS")
	a.clk.add(30 * time.Minute)
	a.b.link.autoCheck()
	if af := a.auto(t); len(af.Attempts) != 1 || a.rel.hit("v1.0.30/SHA256SUMS") != sums || !af.View.NextWindow.Equal(boxTime(4, 2, 0)) {
		t.Fatalf("tried again in the same window: %+v", af)
	}
	a.rel.mu.Lock()
	a.rel.files["v1.0.30/"+setupFile("1.0.30")] = setup
	a.rel.mu.Unlock()
	a.clk.set(boxTime(4, 2, 5))
	a.b.link.autoCheck()
	if a.spawns() != 1 {
		t.Fatal("no attempt in the next window")
	}
}

// D5: a target that was rolled back is not tried again under the same policy; a newer policy re-arms it.
func TestARolledBackTargetIsNotTriedAgain(t *testing.T) {
	a := newAutoRig(t, "v1.0.29", boxTime(3, 2, 10))
	a.deliver(t, "", "", a.policy(1))
	a.b.link.autoCheck()
	a.rs.next(t) // the start's report
	first := a.lastTicket()
	a.finish(t, UpgradeResult{Ticket: first, From: "1.0.29", To: "1.0.30", Error: "not healthy within 20 min after the setup",
		Health:   &linkproto.UpgradeHealth{Minutes: 20, Checks: []linkproto.HealthCheck{{Name: "link", Detail: "not connected to the relay"}}},
		Rollback: &linkproto.UpgradeRollback{To: "1.0.29", OK: true, SHA256: strings.Repeat("0", 64)}})
	a.clk.set(boxTime(4, 2, 10))
	a.b.link.autoCheck()
	af := a.auto(t)
	if a.spawns() != 1 || af.Attempts[0].Outcome != linkproto.OutcomeRolledBack || af.View.Effective.Why != linkproto.WhyStopped || !af.View.Effective.Hold {
		t.Fatalf("spawned %d, %+v", a.spawns(), af)
	}
	rep := a.rs.next(t) // the outcome is reported as soon as the link sees it
	if rep.AutoUpgrade.Outcome != linkproto.OutcomeRolledBack || rep.AutoUpgrade.Rollback == nil || !rep.AutoUpgrade.Rollback.OK || rep.Effective.Why != linkproto.WhyStopped {
		t.Fatalf("report %+v", rep.AutoUpgrade)
	}
	a.deliver(t, "", "", a.policy(2))
	a.b.link.autoCheck()
	if a.spawns() != 2 || a.lastTicket() == first {
		t.Fatal("a newer policy did not re-arm the target")
	}
}

func TestThreeFailedTicketsStop(t *testing.T) {
	a := newAutoRig(t, "v1.0.29", boxTime(3, 2, 10))
	a.deliver(t, "", "", a.policy(1))
	for day := 3; day <= 5; day++ {
		a.clk.set(boxTime(day, 2, 10))
		a.b.link.autoCheck()
		if a.spawns() != day-2 {
			t.Fatalf("day %d: spawned %d", day, a.spawns())
		}
		a.finish(t, UpgradeResult{Ticket: a.lastTicket(), From: "1.0.29", To: "1.0.30", ExitCode: 7, Error: "the setup ended with 7",
			Health: &linkproto.UpgradeHealth{OK: true, Minutes: 20}})
	}
	a.clk.set(boxTime(6, 2, 10))
	a.b.link.autoCheck()
	if af := a.auto(t); a.spawns() != 3 || af.View.Effective.Why != linkproto.WhyStopped {
		t.Fatalf("spawned %d, %+v", a.spawns(), af.View.Effective)
	}
}

// link-status shows what the box holds, what decides, the next check and the last automatic upgrade.
func TestLinkStatusShowsAutoUpgrade(t *testing.T) {
	a := newAutoRig(t, "v1.0.29", boxTime(3, 2, 10))
	a.deliver(t, "bflstore", "canary", a.policy(1))
	a.b.link.autoCheck()
	raw, _ := envelope(ed25519.PrivateKey(a.priv), "team-1", "bfl-read", CommandLinkStatus, nil, nil, a.clk.Now())
	out, sa := send(t, a.rc, raw, nil)
	if answerOf(sa).Refusal != "" {
		t.Fatalf("%+v", answerOf(sa))
	}
	var st LinkStatus
	if err := json.Unmarshal(out, &st); err != nil {
		t.Fatal(err)
	}
	au := st.AutoUpgrade
	if au == nil || !au.Enabled || au.Tenant != "bflstore" || au.Ring != "canary" || len(au.Policies) != 1 || au.Policies[0].Target.Version != "1.0.30" ||
		!au.Policies[0].Applies || au.Effective.Version != "1.0.30" || au.NextCheck.IsZero() || au.Last == nil ||
		au.Last.Ticket != a.lastTicket() || au.Last.Outcome != linkproto.OutcomePending {
		t.Fatalf("%s", out)
	}
	for _, field := range []string{`"autoUpgrade":{"enabled":true`, `"window":{"days":["mon","tue","wed","thu","fri","sat","sun"],"from":"02:00","to":"05:00"}`, `"expiresAt":"2025-04-02T00:10:00Z"`} {
		if !strings.Contains(string(out), field) {
			t.Errorf("link-status lacks %s", field)
		}
	}
}

// The job records the setup of the build it replaces when the box knows it (plan 088 §3): the rollback requires it.
func TestTheJobRecordsTheSetupOfTheBuildReplaced(t *testing.T) {
	known := strings.Repeat("9", 64)
	a := newAutoRig(t, "v1.0.29", boxTime(3, 2, 10))
	// a ticket installed 1.0.29 earlier
	a.finish(t, UpgradeResult{Ticket: strings.Repeat("1", 32), From: "1.0.28", To: "1.0.29", SHA256: known})
	a.finish(t, UpgradeResult{Ticket: strings.Repeat("2", 32), From: "1.0.28", To: "1.0.29", SHA256: strings.Repeat("8", 64), Error: "the setup ended with 7"})
	a.deliver(t, "", "", a.policy(1))
	a.b.link.autoCheck()
	var job UpgradeJob
	b, _ := os.ReadFile(filepath.Join(a.dir, "upgrade", a.lastTicket(), jobFile))
	if err := json.Unmarshal(b, &job); err != nil || job.FromSHA256 != known {
		t.Fatalf("job %+v (%v)", job, err)
	}
	// a policy this box holds that targets 1.0.29, when no ticket installed it
	c := newAutoRig(t, "v1.0.29", boxTime(3, 2, 10))
	old := targetPolicy(boxList("bfl-read"), 1, "1.0.29", strings.Repeat("7", 64), c.clk.Now())
	old.Window.Days = []string{"sun"} // keeps it from being the one acted on: it is up to date anyway
	c.deliver(t, "bflstore", "", c.policy(1), old)
	raw, e := envelope(ed25519.PrivateKey(c.priv), "team-1", "bfl-read", CommandUpgrade, nil, []byte(c.spec("1.0.30", sha(c.setup))), c.clk.Now())
	if _, sa := send(t, c.rc, raw, []byte(c.spec("1.0.30", sha(c.setup)))); answerOf(sa).Exit != 0 {
		t.Fatalf("%+v", answerOf(sa))
	}
	b, _ = os.ReadFile(filepath.Join(c.dir, "upgrade", TicketFor(e.Nonce), jobFile))
	job = UpgradeJob{}
	if err := json.Unmarshal(b, &job); err != nil || job.FromSHA256 != strings.Repeat("7", 64) || job.Auto || job.HealthMinutes != defaultHealthMinutes {
		t.Fatalf("job %+v (%v)", job, err)
	}
}

// A command may name its own health check; out of range it is refused before anything is downloaded.
func TestTheUpgradeCommandsHealthMinutes(t *testing.T) {
	g := newUpgradeRig(t, "v1.0.29")
	ans, exit, _ := g.upgrade(t, `{"version":"1.0.30","sha256":"`+sha(g.setup)+`","healthMinutes":3}`)
	if exit != 1 || ans.Refusal == nil || ans.Refusal.Code != "spec_invalid" || g.rel.hit("v1.0.30/SHA256SUMS") != 0 {
		t.Fatalf("%+v", ans)
	}
	ans, _, ticket := g.upgrade(t, `{"version":"1.0.30","sha256":"`+sha(g.setup)+`","healthMinutes":45}`)
	var job UpgradeJob
	b, _ := os.ReadFile(filepath.Join(g.dir, "upgrade", ticket, jobFile))
	if err := json.Unmarshal(b, &job); err != nil || !ans.Accepted || job.HealthMinutes != 45 || job.Auto || job.Policy != nil {
		t.Fatalf("%+v %+v (%v)", ans, job, err)
	}
}
