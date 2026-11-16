package link

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// --- plan 088 test helpers ------------------------------------------------------------------------------------------

var everyDay = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

// everyBox and boxList are the two scopes (plan 088 D9).
func everyBox() linkproto.PolicyScope { return linkproto.PolicyScope{Type: linkproto.ScopeAll} }

func boxList(ids ...string) linkproto.PolicyScope {
	return linkproto.PolicyScope{Type: linkproto.ScopeBoxes, Boxes: ids}
}

// targetPolicy moves scope to version (setup digest digest) in a 02:00-05:00 window every day, valid around now.
func targetPolicy(sc linkproto.PolicyScope, seq int64, version, digest string, now time.Time) linkproto.UpgradePolicy {
	return linkproto.UpgradePolicy{Scope: sc, Seq: seq, IssuedAt: now.Add(-time.Hour), ExpiresAt: now.Add(30 * 24 * time.Hour),
		Target: &linkproto.PolicyTarget{Version: version, SHA256: digest},
		Window: linkproto.PolicyWindow{Days: everyDay, From: "02:00", To: "05:00"}, HealthMinutes: 20, By: "ilya"}
}

func holdPolicy(sc linkproto.PolicyScope, seq int64, now time.Time) linkproto.UpgradePolicy {
	return linkproto.UpgradePolicy{Scope: sc, Seq: seq, IssuedAt: now.Add(-time.Hour), ExpiresAt: now.Add(7 * 24 * time.Hour), Hold: true, By: "ilya"}
}

func signed(t *testing.T, priv ed25519.PrivateKey, keyID string, p linkproto.UpgradePolicy) linkproto.SignedPolicy {
	t.Helper()
	s, err := linkproto.SignPolicy(priv, keyID, p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// reports collects the report streams a box opens toward the fake relay.
type reports struct {
	ch chan linkproto.Report
}

// acceptReports serves the box-opened streams of rc: each must be a report stream carrying a report signed by pub.
func acceptReports(t *testing.T, rc *relayConn, pub linkproto.BoxPublic) *reports {
	t.Helper()
	rs := &reports{ch: make(chan linkproto.Report, 16)}
	go func() {
		for {
			st, err := rc.sess.AcceptStream()
			if err != nil {
				return
			}
			_ = st.SetReadDeadline(time.Now().Add(5 * time.Second))
			var kind [1]byte
			if _, err := io.ReadFull(st, kind[:]); err != nil || kind[0] != linkproto.StreamReport {
				t.Errorf("the box opened a stream of type %q", kind[0])
				_ = st.Close()
				continue
			}
			raw, err := linkproto.ReadFrame(st, linkproto.MaxControlFrame)
			_ = st.Close()
			if err != nil {
				t.Errorf("a report stream without its frame: %v", err)
				continue
			}
			var s linkproto.SignedReport
			_ = json.Unmarshal(raw, &s)
			r, err := linkproto.VerifyReport(pub, s, rc.box)
			if err != nil {
				t.Errorf("a report that does not verify with the box's key: %v", err)
				continue
			}
			rs.ch <- r
		}
	}()
	return rs
}

// waitCurrent waits for the link to serve the connection it made (Run names it current just after connect returns).
func waitCurrent(t *testing.T, b *box) *conn {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c := b.link.current(); c != nil {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatal("the link serves no connection")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (rs *reports) next(t *testing.T) linkproto.Report {
	t.Helper()
	select {
	case r := <-rs.ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no report")
	}
	return linkproto.Report{}
}

func (rs *reports) none(t *testing.T) {
	t.Helper()
	select {
	case r := <-rs.ch:
		t.Fatalf("an unexpected report: %+v", r)
	case <-time.After(200 * time.Millisecond):
	}
}

// deliver opens a policy stream and writes d.
func deliver(t *testing.T, rc *relayConn, d any) {
	t.Helper()
	st, err := rc.sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	_, _ = st.Write([]byte{linkproto.StreamPolicy})
	raw, _ := json.Marshal(d)
	if err := linkproto.WriteFrame(st, raw); err != nil {
		t.Fatal(err)
	}
}

func delivery(box, tenant, ring string, ps ...linkproto.SignedPolicy) linkproto.PolicyDelivery {
	if ps == nil {
		ps = []linkproto.SignedPolicy{}
	}
	return linkproto.PolicyDelivery{V: linkproto.Version, Box: box, Tenant: tenant, Ring: ring, Policies: ps}
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) set(t time.Time)     { c.mu.Lock(); c.t = t; c.mu.Unlock() }
func (c *fakeClock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// forTests088: no scheduler (the tests call autoCheck), no report budget and no delivery gate (they deliver and read
// every report at once).
func forTests088(c *Config) { c.AutoEvery, c.ReportBudget, c.DeliveryEvery = -1, 1000, -1 }

func withBuild(build string) func(*Config)   { return func(c *Config) { c.Build = build } }
func withClock(clk *fakeClock) func(*Config) { return func(c *Config) { c.Now = clk.Now } }

func (l *testLog) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

// --- the tests ------------------------------------------------------------------------------------------------------

func TestAPolicyDeliveryIsKeptAndReported(t *testing.T) {
	r := newRelay(t)
	priv, kr := team(t, 1)
	b, rc := pairedBox(t, r, "bfl-read", kr, "127.0.0.1:1", func(c *Config) { forTests088(c); withBuild("v1.0.30")(c) })
	rs := acceptReports(t, rc, b.keys.Public())
	now := time.Now()
	digest := strings.Repeat("ab", 32)
	ps := []linkproto.SignedPolicy{
		signed(t, priv, "team-1", targetPolicy(everyBox(), 3, "1.0.31", digest, now)),
		signed(t, priv, "team-1", targetPolicy(boxList("bfl-read", "tamig-1"), 5, "1.0.32", digest, now)),
	}
	deliver(t, rc, delivery("bfl-read", "bflstore", "canary", ps...))
	rep := rs.next(t)
	if len(rep.Policies) != 2 || len(rep.Refused) != 0 || rep.Tenant != "bflstore" || rep.Ring != "canary" || rep.Build != "v1.0.30" {
		t.Fatalf("report %+v", rep)
	}
	eff := rep.Effective
	if eff.Scope == nil || eff.Scope.Key() != "boxes" || len(eff.Scope.Boxes) != 0 || eff.Seq != 5 || eff.Version != "1.0.32" || eff.Hold ||
		eff.Why != linkproto.WhyScheduled {
		t.Fatalf("effective %+v", eff) // the box list naming it is the most specific; the report names it without the list
	}
	pf, err := b.store.loadPolicies()
	if err != nil || len(pf.Policies) != 2 || pf.Policies[1].Epoch != 1 || len(pf.Policies[1].Scope.Boxes) != 2 {
		t.Fatalf("policy.json %+v %v", pf, err)
	}
	if !b.log.has("policy boxes[2] #5 accepted (key team-1): target 1.0.32") {
		t.Fatalf("not logged: %v", b.log.lines)
	}
	// the relay delivers the same policies again on every connection: nothing changes, nothing is refused
	deliver(t, rc, delivery("bfl-read", "bflstore", "canary", ps...))
	if rep := rs.next(t); len(rep.Refused) != 0 || len(rep.Policies) != 2 {
		t.Fatalf("again: %+v", rep)
	}
	if n := b.log.count("accepted (key"); n != 2 {
		t.Fatalf("%d acceptances logged for 2 policies delivered twice", n)
	}
}

func TestPolicyRefusals(t *testing.T) {
	r := newRelay(t)
	priv, kr := team(t, 1)
	stranger, _ := team(t, 1)
	b, rc := pairedBox(t, r, "bfl-read", kr, "127.0.0.1:1", func(c *Config) { forTests088(c); withBuild("v1.0.30")(c) })
	rs := acceptReports(t, rc, b.keys.Public())
	now := time.Now()
	digest := strings.Repeat("cd", 32)
	all5 := targetPolicy(everyBox(), 5, "1.0.31", digest, now)
	deliver(t, rc, delivery("bfl-read", "bflstore", "canary", signed(t, priv, "team-1", all5)))
	if rep := rs.next(t); len(rep.Policies) != 1 {
		t.Fatalf("%+v", rep)
	}
	expired := targetPolicy(boxList("bfl-read"), 1, "1.0.31", digest, now)
	expired.IssuedAt, expired.ExpiresAt = now.Add(-48*time.Hour), now.Add(-time.Hour)
	other := all5
	other.HealthMinutes = 30 // the same seq, other bytes
	cases := []struct {
		name  string
		p     linkproto.SignedPolicy
		code  string
		named bool // the refusal names the policy's scope (its signature verified)
	}{
		{"a key this release does not trust", signed(t, stranger, "team-9", all5), linkproto.CodeKeyUnknown, false},
		{"another key under our id", signed(t, stranger, "team-1", all5), linkproto.CodeBadSignature, false},
		{"expired", signed(t, priv, "team-1", expired), linkproto.CodeExpired, true},
		{"a list without this box", signed(t, priv, "team-1", targetPolicy(boxList("bfl-write", "tamig-1"), 1, "1.0.31", digest, now)), linkproto.CodeWrongScope, true},
		{"an older seq (a replay)", signed(t, priv, "team-1", targetPolicy(everyBox(), 4, "1.0.30", digest, now)), linkproto.CodeStaleSeq, true},
		{"the same seq, other bytes", signed(t, priv, "team-1", other), linkproto.CodeStaleSeq, true},
	}
	for _, c := range cases {
		deliver(t, rc, delivery("bfl-read", "bflstore", "canary", c.p))
		rep := rs.next(t)
		if len(rep.Refused) != 1 || rep.Refused[0].Code != c.code || (rep.Refused[0].Scope != nil) != c.named {
			t.Errorf("%s: refused %+v", c.name, rep.Refused)
		}
	}
	pf, _ := b.store.loadPolicies()
	if len(pf.Policies) != 1 || pf.Policies[0].Seq != 5 || pf.Policies[0].Scope.Key() != "all" {
		t.Fatalf("policy.json changed: %+v", pf.Policies)
	}
	// the text of what did not verify never reaches the log
	if b.log.has("team-9") {
		t.Fatal("an unverified policy's key id was logged")
	}
}

// A delivery refused as a whole records nothing: for another box, malformed, or with a store that cannot be read.
func TestADeliveryRefusedAsAWhole(t *testing.T) {
	r := newRelay(t)
	priv, kr := team(t, 1)
	b, rc := pairedBox(t, r, "bfl-read", kr, "127.0.0.1:1", forTests088)
	rs := acceptReports(t, rc, b.keys.Public())
	now := time.Now()
	p := signed(t, priv, "team-1", targetPolicy(everyBox(), 1, "1.0.31", strings.Repeat("ef", 32), now))
	for _, c := range []struct {
		name string
		d    any
		code string
	}{
		{"another box's delivery", delivery("bfl-write", "", "", p), linkproto.CodeWrongBox},
		{"version 2", map[string]any{"v": 2, "box": "bfl-read", "policies": []any{}}, linkproto.CodeVersion},
		{"five policies", delivery("bfl-read", "", "", p, p, p, p, p), linkproto.CodeMalformed},
	} {
		deliver(t, rc, c.d)
		if rep := rs.next(t); len(rep.Refused) != 1 || rep.Refused[0].Code != c.code {
			t.Errorf("%s: %+v", c.name, rep.Refused)
		}
	}
	if _, err := os.Stat(policyPath(b.store.Dir)); !os.IsNotExist(err) {
		t.Fatal("a refused delivery wrote policy.json")
	}
	// a store that does not read is never started over: an old policy could be replayed into a fresh one
	_ = os.MkdirAll(linkDir(b.store.Dir), 0o700)
	_ = os.WriteFile(policyPath(b.store.Dir), []byte("{corrupt"), 0o600)
	deliver(t, rc, delivery("bfl-read", "", "", p))
	if rep := rs.next(t); len(rep.Refused) != 1 || rep.Refused[0].Code != codeStateUnreadable || rep.Effective.Why != linkproto.WhyStateUnreadable {
		t.Fatalf("%+v", rep)
	}
	if got, _ := os.ReadFile(policyPath(b.store.Dir)); string(got) != "{corrupt" {
		t.Fatal("the unreadable store was overwritten")
	}
}

// A retired team key's policies are refused once a newer key was seen, and a policy held from it stops counting.
func TestARetiredKeysPolicies(t *testing.T) {
	r := newRelay(t)
	priv1, kr1 := team(t, 1)
	priv2, kr2 := team(t, 2)
	b, rc := pairedBox(t, r, "bfl-read", append(kr1, kr2...), "127.0.0.1:1", func(c *Config) { forTests088(c); withBuild("v1.0.30")(c) })
	rs := acceptReports(t, rc, b.keys.Public())
	now := time.Now()
	digest := strings.Repeat("ab", 32)
	deliver(t, rc, delivery("bfl-read", "", "", signed(t, priv1, "team-1", targetPolicy(everyBox(), 1, "1.0.31", digest, now))))
	if rep := rs.next(t); rep.Effective.Version != "1.0.31" {
		t.Fatalf("%+v", rep.Effective)
	}
	deliver(t, rc, delivery("bfl-read", "", "", signed(t, priv2, "team-2", holdPolicy(boxList("bfl-read"), 1, now))))
	rep := rs.next(t)
	if b.store.MinEpoch(CompiledMinEpoch) != 2 {
		t.Fatal("the epoch floor did not rise")
	}
	// the epoch-1 policy no longer verifies: it is not reported as held
	if len(rep.Policies) != 1 || rep.Policies[0].Scope.Key() != "boxes" {
		t.Fatalf("%+v", rep.Policies)
	}
	deliver(t, rc, delivery("bfl-read", "", "", signed(t, priv1, "team-1", targetPolicy(everyBox(), 2, "1.0.31", digest, now))))
	if rep := rs.next(t); len(rep.Refused) != 1 || rep.Refused[0].Code != linkproto.CodeKeyRevoked {
		t.Fatalf("%+v", rep.Refused)
	}
}

// The box opens a report stream only on a connection whose relay opened a policy stream: an older relay closes a
// connection whose box opens a second stream.
func TestNoReportToARelayThatSentNoPolicy(t *testing.T) {
	r := newRelay(t)
	_, kr := team(t, 1)
	b, rc := pairedBox(t, r, "bfl-read", kr, "127.0.0.1:1", forTests088)
	rs := acceptReports(t, rc, b.keys.Public())
	c := waitCurrent(t, b)
	if b.link.sendReport(c) {
		t.Fatal("a report went to a relay that never opened a policy stream")
	}
	rs.none(t)
	deliver(t, rc, delivery("bfl-read", "", ""))
	rs.next(t)
	if !b.link.sendReport(b.link.current()) {
		t.Fatal("no report after a policy stream")
	}
	rs.next(t)
}

// D9 (review M1): what applies is the signed scope against the box's own id. A relay that states another tenant or
// ring — or none — neither undoes a hold nor spreads a canary-only approval.
func TestTheRelaysStatementsDecideNothing(t *testing.T) {
	r := newRelay(t)
	priv, kr := team(t, 1)
	b, rc := pairedBox(t, r, "bfl-read", kr, "127.0.0.1:1", func(c *Config) { forTests088(c); withBuild("v1.0.30")(c) })
	rs := acceptReports(t, rc, b.keys.Public())
	now := time.Now()
	digest := strings.Repeat("ab", 32)
	ps := []linkproto.SignedPolicy{
		signed(t, priv, "team-1", targetPolicy(everyBox(), 20, "1.0.40", digest, now)),
		signed(t, priv, "team-1", holdPolicy(boxList("bfl-read", "bfl-write"), 3, now)), // BFL stays on its build
	}
	for _, stated := range []struct{ tenant, ring string }{{"bflstore", ""}, {"", ""}, {"tamig", "canary"}, {"bflstore", "stable"}} {
		deliver(t, rc, delivery("bfl-read", stated.tenant, stated.ring, ps...))
		rep := rs.next(t)
		if rep.Effective.Why != linkproto.WhyPolicyHolds || rep.Effective.Scope.Key() != "boxes" || !rep.Effective.Hold {
			t.Fatalf("stated %+v: the hold was undone: %+v", stated, rep.Effective)
		}
		if rep.Tenant != stated.tenant || rep.Ring != stated.ring {
			t.Fatalf("the report does not repeat what the relay stated: %+v", rep)
		}
	}
	// a canary-only approval (the canary box list) never reaches a box it does not name, whatever ring it is stated in
	canary := signed(t, priv, "team-1", targetPolicy(boxList("tamig-1"), 4, "1.0.41", digest, now))
	deliver(t, rc, delivery("bfl-read", "bflstore", "canary", canary))
	if rep := rs.next(t); len(rep.Refused) != 1 || rep.Refused[0].Code != linkproto.CodeWrongScope || rep.Effective.Why != linkproto.WhyPolicyHolds {
		t.Fatalf("%+v", rep)
	}
}

// D10 (review M2): a hold issued after a moving policy wins at any scope — every box's hold is the emergency brake for
// a box list that still targets the bad release.
func TestAnAllHoldBrakesABoxListsTarget(t *testing.T) {
	r := newRelay(t)
	priv, kr := team(t, 1)
	b, rc := pairedBox(t, r, "bfl-read", kr, "127.0.0.1:1", func(c *Config) { forTests088(c); withBuild("v1.0.40")(c) })
	rs := acceptReports(t, rc, b.keys.Public())
	now := time.Now()
	digest := strings.Repeat("ab", 32)
	canary := targetPolicy(boxList("bfl-read", "tamig-1"), 3, "1.0.41", digest, now)
	canary.IssuedAt = now.Add(-3 * time.Hour)
	all := targetPolicy(everyBox(), 5, "1.0.41", digest, now)
	all.IssuedAt = now.Add(-2 * time.Hour)
	deliver(t, rc, delivery("bfl-read", "", "", signed(t, priv, "team-1", canary), signed(t, priv, "team-1", all)))
	if rep := rs.next(t); rep.Effective.Why != linkproto.WhyScheduled || rep.Effective.Version != "1.0.41" {
		t.Fatalf("%+v", rep.Effective)
	}
	brake := holdPolicy(everyBox(), 6, now) // 1.0.41 is found bad: the inverse is every box's hold
	deliver(t, rc, delivery("bfl-read", "", "", signed(t, priv, "team-1", canary), signed(t, priv, "team-1", brake)))
	rep := rs.next(t)
	if rep.Effective.Why != linkproto.WhyPolicyHolds || !rep.Effective.Hold || rep.Effective.Scope.Key() != "all" || rep.Effective.Seq != 6 {
		t.Fatalf("the brake did not hold the box list's target: %+v", rep.Effective)
	}
	st := b.link.autoStatus()
	if st.Braked == nil || st.Braked.Scope.Key() != "boxes" || st.Braked.Seq != 3 {
		t.Fatalf("braked %+v", st.Braked)
	}
	// a box list issued after the brake moves its boxes again
	again := targetPolicy(boxList("bfl-read"), 7, "1.0.42", digest, now)
	again.IssuedAt = now.Add(time.Minute)
	deliver(t, rc, delivery("bfl-read", "", "", signed(t, priv, "team-1", again), signed(t, priv, "team-1", brake)))
	if rep := rs.next(t); rep.Effective.Why != linkproto.WhyScheduled || rep.Effective.Version != "1.0.42" {
		t.Fatalf("%+v", rep.Effective)
	}
}

// Review m1: seq stops at 2^31-1, and a floor set by a key the box has since retired no longer binds — a stolen key
// cannot freeze a scope past its rotation.
func TestARetiredKeysFloorNoLongerBinds(t *testing.T) {
	r := newRelay(t)
	priv1, kr1 := team(t, 1)
	priv2, kr2 := team(t, 2)
	b, rc := pairedBox(t, r, "bfl-read", append(kr1, kr2...), "127.0.0.1:1", func(c *Config) { forTests088(c); withBuild("v1.0.30")(c) })
	rs := acceptReports(t, rc, b.keys.Public())
	now := time.Now()
	digest := strings.Repeat("ab", 32)
	frozen := targetPolicy(everyBox(), linkproto.MaxSeq, "1.0.31", digest, now)
	deliver(t, rc, delivery("bfl-read", "", "", signed(t, priv1, "team-1", frozen)))
	if rep := rs.next(t); len(rep.Refused) != 0 || rep.Policies[0].Seq != linkproto.MaxSeq {
		t.Fatalf("%+v", rep)
	}
	// over the ceiling: refused before it is weighed (signed raw, the way only a forger with the key would)
	over := strings.Replace(string(signed(t, priv1, "team-1", targetPolicy(everyBox(), 9, "1.0.31", digest, now)).Policy), `"seq":9`, `"seq":2147483648`, 1)
	deliver(t, rc, delivery("bfl-read", "", "", rawSigned(priv1, "team-1", []byte(over))))
	if rep := rs.next(t); len(rep.Refused) != 1 || rep.Refused[0].Code != linkproto.CodeMalformed {
		t.Fatalf("seq 2^31: %+v", rep.Refused)
	}
	// with team-1, nothing lower gets through
	deliver(t, rc, delivery("bfl-read", "", "", signed(t, priv1, "team-1", targetPolicy(everyBox(), 5, "1.0.31", digest, now))))
	if rep := rs.next(t); len(rep.Refused) != 1 || rep.Refused[0].Code != linkproto.CodeStaleSeq {
		t.Fatalf("%+v", rep.Refused)
	}
	// rotated: team-2's next policy is taken at its own seq, and team-1's floor is gone
	deliver(t, rc, delivery("bfl-read", "", "", signed(t, priv2, "team-2", targetPolicy(everyBox(), 5, "1.0.31", digest, now))))
	rep := rs.next(t)
	if len(rep.Refused) != 0 || len(rep.Policies) != 1 || rep.Policies[0].Seq != 5 {
		t.Fatalf("after the rotation: %+v", rep)
	}
	pf, _ := b.store.loadPolicies()
	if len(pf.Policies) != 1 || pf.Policies[0].Seq != 5 || pf.Policies[0].Epoch != 2 {
		t.Fatalf("policy.json %+v", pf.Policies)
	}
}

// rawSigned signs raw policy bytes as they are, the way a forger holding the key would.
func rawSigned(priv ed25519.PrivateKey, keyID string, raw []byte) linkproto.SignedPolicy {
	sig := ed25519.Sign(priv, append([]byte("digi-link-policy-v1\x00"), raw...))
	return linkproto.SignedPolicy{Policy: raw, KeyID: keyID, Sig: base64.StdEncoding.EncodeToString(sig)}
}

func TestTheAutoUpgradeSwitchIsAKnownKey(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	for y, want := range map[string]struct{ on, unknown bool }{
		"box: bfl-read\n":                      {true, false},
		"box: bfl-read\nautoUpgrade: false\n":  {false, false},
		"box: bfl-read\nautoUpgrade: true\n":   {true, false},
		"box: bfl-read\nautoupgrade: false\n":  {true, true}, // misspelt: every command refused, and no auto-update (state_unreadable)
		"box: bfl-read\nAutoUpgrade: false\n":  {true, true},
		"box: bfl-read\nauto_upgrade: false\n": {true, true},
	} {
		_ = os.WriteFile(s.statePath(), []byte(y), 0o600)
		st, err := s.LoadState()
		if err != nil || st.AutoUpgradeOn() != want.on || st.UnknownKeys() != want.unknown {
			t.Errorf("%q: on %v unknown %v (%v)", y, st.AutoUpgradeOn(), st.UnknownKeys(), err)
		}
	}
	// the link never writes it: a save keeps the person's value
	_ = os.WriteFile(s.statePath(), []byte("pairCode: ABCDE-FGHJK\nautoUpgrade: false\n"), 0o600)
	st, _ := s.LoadState()
	st.Box, st.PairCode = "bfl-read", ""
	_ = s.SaveState(st)
	if back, _ := s.LoadState(); back.AutoUpgradeOn() {
		t.Fatal("pairing turned auto-update back on")
	}
}

// The relay ends a connection whose box opens more than six report streams a minute: the box keeps within its budget,
// and a report over it goes later as the latest view (one, however many were owed).
func TestReportsStayWithinTheRelaysLimit(t *testing.T) {
	old := reportWindow
	reportWindow = 400 * time.Millisecond
	t.Cleanup(func() { reportWindow = old })
	r := newRelay(t)
	_, kr := team(t, 1)
	b, rc := pairedBox(t, r, "bfl-read", kr, "127.0.0.1:1", func(c *Config) { forTests088(c); c.ReportBudget = 2 })
	rs := acceptReports(t, rc, b.keys.Public())
	stated := func(ring string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			// taken, and its slot let go (the relay waits for the report, or 10 s, before its next delivery)
			if b.link.statedNow().ring == ring && len(b.link.policySlots) == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("delivery for ring %s not taken", ring)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	start := time.Now()
	for _, ring := range []string{"r-1", "r-2"} {
		deliver(t, rc, delivery("bfl-read", "", ring))
		if rep := rs.next(t); rep.Ring != ring {
			t.Fatalf("report for %s: %+v", ring, rep)
		}
	}
	for _, ring := range []string{"r-3", "r-4"} { // over the budget: owed
		deliver(t, rc, delivery("bfl-read", "", ring))
		stated(ring)
	}
	rep := rs.next(t)
	if rep.Ring != "r-4" || time.Since(start) < 300*time.Millisecond {
		t.Fatalf("the owed report: ring %q after %s", rep.Ring, time.Since(start))
	}
	rs.none(t)
}

// The link takes one delivery at a time, and the relay sends its next one once it has the report: the slot is free
// before the report leaves, so that next delivery is never closed unread.
func TestTheDeliverySlotIsFreeBeforeTheReport(t *testing.T) {
	r := newRelay(t)
	_, kr := team(t, 1)
	b, rc := pairedBox(t, r, "bfl-read", kr, "127.0.0.1:1", forTests088)
	rs := acceptReports(t, rc, b.keys.Public())
	for i := range 5 {
		deliver(t, rc, delivery("bfl-read", "", ""))
		rs.next(t)
		if n := len(b.link.policySlots); n != 0 {
			t.Fatalf("delivery %d: the slot was still held when the report arrived", i)
		}
	}
}

// Review M1 (with D9): what the relay states about the box is information for its reports — never stored with the
// policies, never a reason to rewrite policy.json, its changes logged at most once a minute.
func TestTheRelaysStatementsAreInformationOnly(t *testing.T) {
	r := newRelay(t)
	priv, kr := team(t, 1)
	b, rc := pairedBox(t, r, "bfl-read", kr, "127.0.0.1:1", func(c *Config) { forTests088(c); withBuild("v1.0.30")(c) })
	rs := acceptReports(t, rc, b.keys.Public())
	p := signed(t, priv, "team-1", targetPolicy(everyBox(), 3, "1.0.31", strings.Repeat("ab", 32), time.Now()))
	deliver(t, rc, delivery("bfl-read", "bflstore", "canary", p))
	if rep := rs.next(t); rep.Tenant != "bflstore" || rep.Ring != "canary" {
		t.Fatalf("%+v", rep)
	}
	before, err := os.ReadFile(policyPath(b.store.Dir))
	if err != nil || strings.Contains(string(before), "bflstore") || strings.Contains(string(before), "canary") {
		t.Fatalf("policy.json holds what the relay stated: %s (%v)", before, err)
	}
	fi, _ := os.Stat(policyPath(b.store.Dir))
	for i, stated := range []string{"tamig", "bflstore", "tamig", "bflstore", "tamig"} {
		deliver(t, rc, delivery("bfl-read", stated, "", p))
		if rep := rs.next(t); rep.Tenant != stated || len(rep.Refused) != 0 || rep.Effective.Why != linkproto.WhyScheduled {
			t.Fatalf("delivery %d: %+v", i, rep)
		}
	}
	after, _ := os.ReadFile(policyPath(b.store.Dir))
	fi2, _ := os.Stat(policyPath(b.store.Dir))
	if string(after) != string(before) || !fi2.ModTime().Equal(fi.ModTime()) {
		t.Fatal("policy.json was rewritten for a statement")
	}
	if n := b.log.count("the relay states tenant"); n != 1 {
		t.Fatalf("the statement's change logged %d times", n)
	}
}

// Review m2: a connection's deliveries are taken at most once per DeliveryEvery; of those that come sooner only the
// newest is taken, once, when the period is over.
func TestDeliveriesAreCoalesced(t *testing.T) {
	r := newRelay(t)
	_, kr := team(t, 1)
	b, rc := pairedBox(t, r, "bfl-read", kr, "127.0.0.1:1", func(c *Config) { forTests088(c); c.DeliveryEvery = 400 * time.Millisecond })
	rs := acceptReports(t, rc, b.keys.Public())
	start := time.Now()
	deliver(t, rc, delivery("bfl-read", "", "r-1"))
	if rep := rs.next(t); rep.Ring != "r-1" {
		t.Fatalf("the first: %+v", rep)
	}
	for _, ring := range []string{"r-2", "r-3", "r-4", "r-5"} {
		deliver(t, rc, delivery("bfl-read", "", ring))
		time.Sleep(10 * time.Millisecond)
	}
	rep := rs.next(t)
	if rep.Ring != "r-5" || time.Since(start) < 350*time.Millisecond {
		t.Fatalf("the coalesced one: ring %q after %s", rep.Ring, time.Since(start))
	}
	rs.none(t)
	if n := len(b.link.policySlots); n != 0 {
		t.Fatal("a waiting delivery holds the slot")
	}
}

// Review m2: only a delivery that changed a policy asks the scheduler for a check, and those checks come at most once
// per autoKickEvery however many deliveries ask.
func TestOnlyAChangeKicksTheScheduler(t *testing.T) {
	old := autoKickEvery
	autoKickEvery = time.Second
	t.Cleanup(func() { autoKickEvery = old })
	r := newRelay(t)
	priv, kr := team(t, 1)
	b, rc := pairedBox(t, r, "bfl-read", kr, "127.0.0.1:1", func(c *Config) {
		forTests088(c)
		c.AutoEvery, c.AutoFirst = time.Hour, time.Hour // only kicks check
	})
	rs := acceptReports(t, rc, b.keys.Public())
	now := time.Now()
	policy := func(seq int64) linkproto.SignedPolicy {
		return signed(t, priv, "team-1", targetPolicy(everyBox(), seq, "1.0.31", strings.Repeat("ab", 32), now))
	}
	waitChecks := func(want int64) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for b.link.autoChecks.Load() < want && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if got := b.link.autoChecks.Load(); got != want {
			t.Fatalf("%d checks, want %d", got, want)
		}
	}
	deliver(t, rc, delivery("bfl-read", "", "", policy(1)))
	rs.next(t)
	waitChecks(1)
	// the same policy again, five times: no check, not even once the period is over
	for range 5 {
		deliver(t, rc, delivery("bfl-read", "", "", policy(1)))
		rs.next(t)
	}
	time.Sleep(autoKickEvery + 200*time.Millisecond)
	waitChecks(1)
	// four new policies in a row: a check at once, then one more when the period is over — not four
	for seq := int64(2); seq <= 5; seq++ {
		deliver(t, rc, delivery("bfl-read", "", "", policy(seq)))
		rs.next(t)
	}
	time.Sleep(200 * time.Millisecond)
	waitChecks(2)
	time.Sleep(autoKickEvery)
	waitChecks(3)
}

// Review m3, m4: the seq floors are kept apart from policy.json (in the secrets store) and never pruned — a policy file
// deleted by hand, or lost, holds the box until the relay delivers again, and never lets an older policy back.
func TestTheSeqFloorsSurviveALostPolicyFile(t *testing.T) {
	r := newRelay(t)
	priv, kr := team(t, 1)
	b, rc := pairedBox(t, r, "bfl-read", kr, "127.0.0.1:1", func(c *Config) { forTests088(c); withBuild("v1.0.30")(c) })
	rs := acceptReports(t, rc, b.keys.Public())
	now := time.Now()
	digest := strings.Repeat("ab", 32)
	all5 := targetPolicy(everyBox(), 5, "1.0.31", digest, now)
	list7 := holdPolicy(boxList("bfl-read", "bfl-write"), 7, now)
	deliver(t, rc, delivery("bfl-read", "", "", signed(t, priv, "team-1", all5), signed(t, priv, "team-1", list7)))
	if rep := rs.next(t); len(rep.Policies) != 2 || rep.Effective.Why != linkproto.WhyPolicyHolds {
		t.Fatalf("%+v", rep)
	}
	if err := os.Remove(policyPath(b.store.Dir)); err != nil {
		t.Fatal(err)
	}
	// older policies of both keys, replayed into the empty file: refused
	all4 := targetPolicy(everyBox(), 4, "1.0.31", digest, now)
	list6 := targetPolicy(boxList("bfl-read"), 6, "1.0.31", digest, now)
	deliver(t, rc, delivery("bfl-read", "", "", signed(t, priv, "team-1", all4), signed(t, priv, "team-1", list6)))
	rep := rs.next(t)
	if len(rep.Refused) != 2 || rep.Refused[0].Code != linkproto.CodeStaleSeq || rep.Refused[1].Code != linkproto.CodeStaleSeq ||
		len(rep.Policies) != 0 || rep.Effective.Why != linkproto.WhyNoPolicy || !rep.Effective.Hold {
		t.Fatalf("a replay into a lost policy file: %+v", rep)
	}
	// the same seq with other bytes: refused too
	other := all5
	other.HealthMinutes = 30
	deliver(t, rc, delivery("bfl-read", "", "", signed(t, priv, "team-1", other)))
	if rep := rs.next(t); len(rep.Refused) != 1 || rep.Refused[0].Code != linkproto.CodeStaleSeq {
		t.Fatalf("other bytes at the floor's seq: %+v", rep.Refused)
	}
	// the very policies again: taken back
	deliver(t, rc, delivery("bfl-read", "", "", signed(t, priv, "team-1", all5), signed(t, priv, "team-1", list7)))
	if rep := rs.next(t); len(rep.Refused) != 0 || len(rep.Policies) != 2 || rep.Effective.Why != linkproto.WhyPolicyHolds {
		t.Fatalf("taken back: %+v", rep)
	}
	pf, err := b.store.loadPolicies()
	if err != nil || len(pf.Policies) != 2 {
		t.Fatalf("policy.json %+v %v", pf, err)
	}
}

// Review m4: an unreadable policy file no longer stops policies for good — the floors kept apart judge every replay —
// but with no floors to judge by, it is never started over.
func TestAnUnreadablePolicyFileStartsOverAgainstTheFloors(t *testing.T) {
	r := newRelay(t)
	priv, kr := team(t, 1)
	b, rc := pairedBox(t, r, "bfl-read", kr, "127.0.0.1:1", func(c *Config) { forTests088(c); withBuild("v1.0.30")(c) })
	rs := acceptReports(t, rc, b.keys.Public())
	now := time.Now()
	digest := strings.Repeat("ab", 32)
	deliver(t, rc, delivery("bfl-read", "", "", signed(t, priv, "team-1", targetPolicy(everyBox(), 5, "1.0.31", digest, now))))
	rs.next(t)
	_ = os.WriteFile(policyPath(b.store.Dir), []byte("{corrupt"), 0o600)
	deliver(t, rc, delivery("bfl-read", "", "", signed(t, priv, "team-1", targetPolicy(everyBox(), 4, "1.0.31", digest, now))))
	rep := rs.next(t)
	if len(rep.Refused) != 1 || rep.Refused[0].Code != linkproto.CodeStaleSeq || rep.Effective.Why != linkproto.WhyStateUnreadable {
		t.Fatalf("a replay against the floors: %+v", rep)
	}
	if got, _ := os.ReadFile(policyPath(b.store.Dir)); string(got) != "{corrupt" {
		t.Fatal("a delivery that changed nothing rewrote the file")
	}
	deliver(t, rc, delivery("bfl-read", "", "", signed(t, priv, "team-1", targetPolicy(everyBox(), 6, "1.0.31", digest, now))))
	if rep := rs.next(t); len(rep.Refused) != 0 || rep.Effective.Why != linkproto.WhyScheduled || rep.Effective.Seq != 6 {
		t.Fatalf("a newer policy: %+v", rep)
	}
	if pf, err := b.store.loadPolicies(); err != nil || len(pf.Policies) != 1 || pf.Policies[0].Seq != 6 {
		t.Fatalf("policy.json %+v %v", pf, err)
	}
	// the floors themselves unreadable: every delivery is refused, nothing is written
	_ = b.store.Secrets.Set(secretPolicyFloors, []byte("{corrupt"))
	deliver(t, rc, delivery("bfl-read", "", "", signed(t, priv, "team-1", targetPolicy(everyBox(), 9, "1.0.31", digest, now))))
	if rep := rs.next(t); len(rep.Refused) != 1 || rep.Refused[0].Code != codeStateUnreadable {
		t.Fatalf("unreadable floors: %+v", rep.Refused)
	}
	if pf, _ := b.store.loadPolicies(); len(pf.Policies) != 1 || pf.Policies[0].Seq != 6 {
		t.Fatalf("written past unreadable floors: %+v", pf.Policies)
	}
}
