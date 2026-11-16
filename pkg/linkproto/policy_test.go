package linkproto

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var policyNow = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

func policyTeamKey(t *testing.T, id string, epoch int) (ed25519.PrivateKey, TeamKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv, TeamKey{ID: id, Epoch: epoch, Pub: pub}
}

func goodPolicy() UpgradePolicy {
	return UpgradePolicy{
		Scope: PolicyScope{Type: ScopeRing, Value: "canary"}, Seq: 12,
		IssuedAt: policyNow, ExpiresAt: policyNow.Add(30 * 24 * time.Hour),
		Target:        &PolicyTarget{Version: "1.0.31", SHA256: strings.Repeat("ab", 32)},
		Window:        PolicyWindow{Days: []string{"mon", "tue", "wed", "thu", "sun"}, From: "02:00", To: "05:00"},
		HealthMinutes: 15, By: "ilya",
	}
}

// resign signs raw policy bytes as they are, the way a forger with the key — or a buggy signer — would.
func signRaw(priv ed25519.PrivateKey, keyID string, raw []byte) SignedPolicy {
	sig := ed25519.Sign(priv, append([]byte(domainPolicy), raw...))
	return SignedPolicy{Policy: raw, KeyID: keyID, Sig: base64.StdEncoding.EncodeToString(sig)}
}

func TestSignAndVerifyAPolicy(t *testing.T) {
	priv, k := policyTeamKey(t, "team-1", 1)
	in := goodPolicy()
	in.IssuedAt = policyNow.Add(1500 * time.Millisecond).In(time.FixedZone("IST", 2*3600))
	s, err := SignPolicy(priv, "team-1", in)
	if err != nil {
		t.Fatal(err)
	}
	p, err := VerifyPolicy(s, Keyring{k})
	if err != nil {
		t.Fatal(err)
	}
	if p.V != 1 || p.Kind != PolicyKindUpgrade || p.Seq != 12 || p.Scope.Key() != "ring:canary" || p.Target.Version != "1.0.31" ||
		!p.IssuedAt.Equal(policyNow.Add(time.Second)) || p.IssuedAt.Location() != time.UTC {
		t.Fatalf("%+v", p)
	}
	// the wire form: policy is base64 of the exact bytes, which carry the contract's field names
	wire, _ := json.Marshal(s)
	var back SignedPolicy
	if err := json.Unmarshal(wire, &back); err != nil || !bytes.Equal(back.Policy, s.Policy) {
		t.Fatalf("round trip: %v", err)
	}
	for _, field := range []string{`"v":1`, `"kind":"upgrade-policy"`, `"scope":{"type":"ring","value":"canary"}`, `"seq":12`,
		`"issuedAt":"2026-11-16T09:00:01Z"`, `"target":{"version":"1.0.31","sha256":"`, `"hold":false`,
		`"window":{"days":["mon","tue","wed","thu","sun"],"from":"02:00","to":"05:00"}`, `"healthMinutes":15`, `"by":"ilya"`} {
		if !strings.Contains(string(s.Policy), field) {
			t.Errorf("the signed bytes lack %s: %s", field, s.Policy)
		}
	}
	if !strings.Contains(string(wire), `"keyId":"team-1"`) || !strings.Contains(string(wire), `"policy":"`+base64.StdEncoding.EncodeToString(s.Policy)+`"`) {
		t.Fatalf("wire %s", wire)
	}
	// a hold needs no target, window or health minutes
	hold := UpgradePolicy{Scope: PolicyScope{Type: ScopeAll}, Seq: 1, IssuedAt: policyNow, ExpiresAt: policyNow.Add(time.Hour), Hold: true, By: "ilya"}
	if s, err := SignPolicy(priv, "team-1", hold); err != nil {
		t.Fatal(err)
	} else if p, err := VerifyPolicy(s, Keyring{k}); err != nil || !p.Holds() {
		t.Fatalf("hold: %+v %v", p, err)
	}
}

func TestVerifyPolicyRefusals(t *testing.T) {
	priv, k := policyTeamKey(t, "team-1", 1)
	other, _ := policyTeamKey(t, "team-1", 1)
	priv2, k2 := policyTeamKey(t, "team-2", 2)
	s, _ := SignPolicy(priv, "team-1", goodPolicy())
	edited := func(edit func(m map[string]any)) []byte {
		var m map[string]any
		_ = json.Unmarshal(s.Policy, &m)
		edit(m)
		b, _ := json.Marshal(m) // map keys sorted: not the canonical field order
		return b
	}
	// the canonical bytes with one field's text replaced, signed again
	swap := func(old, new string) []byte { return []byte(strings.Replace(string(s.Policy), old, new, 1)) }

	cases := []struct {
		name string
		s    SignedPolicy
		kr   Keyring
		min  int
		want string
	}{
		{"an untrusted key id", SignedPolicy{Policy: s.Policy, KeyID: "team-9", Sig: s.Sig}, Keyring{k}, 0, CodeKeyUnknown},
		{"another key under our id", signRaw(other, "team-1", s.Policy), Keyring{k}, 0, CodeBadSignature},
		{"a changed byte", SignedPolicy{Policy: swap(`"seq":12`, `"seq":13`), KeyID: "team-1", Sig: s.Sig}, Keyring{k}, 0, CodeBadSignature},
		{"an envelope's signature domain", func() SignedPolicy {
			sig := ed25519.Sign(priv, append([]byte(domainEnvelope), s.Policy...))
			return SignedPolicy{Policy: s.Policy, KeyID: "team-1", Sig: base64.StdEncoding.EncodeToString(sig)}
		}(), Keyring{k}, 0, CodeBadSignature},
		{"a key below the floor", s, Keyring{k, k2}, 2, CodeKeyRevoked},
		{"empty", SignedPolicy{KeyID: "team-1"}, Keyring{k}, 0, CodeMalformed},
		{"too big", signRaw(priv, "team-1", bytes.Repeat([]byte(" "), maxPolicyBytes+1)), Keyring{k}, 0, CodeMalformed},
		{"not JSON", signRaw(priv, "team-1", []byte("nope")), Keyring{k}, 0, CodeMalformed},
		{"not canonical: other key order", signRaw(priv, "team-1", edited(func(map[string]any) {})), Keyring{k}, 0, CodeMalformed},
		{"not canonical: whitespace", signRaw(priv, "team-1", append(append([]byte{}, s.Policy...), ' ')), Keyring{k}, 0, CodeMalformed},
		{"not canonical: a re-cased key", signRaw(priv, "team-1", swap(`"seq":12`, `"SEQ":12`)), Keyring{k}, 0, CodeMalformed},
		{"a duplicate key", signRaw(priv, "team-1", swap(`"seq":12`, `"seq":1,"seq":12`)), Keyring{k}, 0, CodeMalformed},
		{"an unknown field", signRaw(priv, "team-1", swap(`"hold":false`, `"hold":false,"url":"https://evil"`)), Keyring{k}, 0, CodeMalformed},
		{"version 2", signRaw(priv, "team-1", swap(`"v":1`, `"v":2`)), Keyring{k}, 0, CodeVersion},
		{"another kind", signRaw(priv, "team-1", swap(`"kind":"upgrade-policy"`, `"kind":"command"`)), Keyring{k}, 0, CodeMalformed},
		{"an unknown scope type", signRaw(priv, "team-1", swap(`"type":"ring"`, `"type":"region"`)), Keyring{k}, 0, CodeMalformed},
		{"all with a value", signRaw(priv, "team-1", swap(`"type":"ring"`, `"type":"all"`)), Keyring{k}, 0, CodeMalformed},
		{"a box id that is not one", signRaw(priv, "team-1", swap(`{"type":"ring","value":"canary"}`, `{"type":"box","value":"../x"}`)), Keyring{k}, 0, CodeMalformed},
		{"seq 0", signRaw(priv, "team-1", swap(`"seq":12`, `"seq":0`)), Keyring{k}, 0, CodeMalformed},
		{"seq negative", signRaw(priv, "team-1", swap(`"seq":12`, `"seq":-3`)), Keyring{k}, 0, CodeMalformed},
		{"expires before issued", signRaw(priv, "team-1", swap(`"expiresAt":"2026-12-16T09:00:00Z"`, `"expiresAt":"2026-11-16T08:00:00Z"`)), Keyring{k}, 0, CodeMalformed},
		{"lives too long", signRaw(priv, "team-1", swap(`"expiresAt":"2026-12-16T09:00:00Z"`, `"expiresAt":"2028-11-16T09:00:00Z"`)), Keyring{k}, 0, CodeLifetime},
		{"a hold with a target", signRaw(priv, "team-1", swap(`"hold":false`, `"hold":true`)), Keyring{k}, 0, CodeMalformed},
		{"a version with a v", signRaw(priv, "team-1", swap(`"version":"1.0.31"`, `"version":"v1.0.31"`)), Keyring{k}, 0, CodeMalformed},
		{"a version with a path", signRaw(priv, "team-1", swap(`"version":"1.0.31"`, `"version":"1.0.31/../x"`)), Keyring{k}, 0, CodeMalformed},
		{"an uppercase digest", signRaw(priv, "team-1", swap(strings.Repeat("ab", 32), strings.Repeat("AB", 32))), Keyring{k}, 0, CodeMalformed},
		{"a short digest", signRaw(priv, "team-1", swap(strings.Repeat("ab", 32), "abc")), Keyring{k}, 0, CodeMalformed},
		{"no days", signRaw(priv, "team-1", swap(`["mon","tue","wed","thu","sun"]`, `[]`)), Keyring{k}, 0, CodeMalformed},
		{"a day twice", signRaw(priv, "team-1", swap(`["mon","tue","wed","thu","sun"]`, `["mon","mon"]`)), Keyring{k}, 0, CodeMalformed},
		{"an unknown day", signRaw(priv, "team-1", swap(`["mon","tue","wed","thu","sun"]`, `["monday"]`)), Keyring{k}, 0, CodeMalformed},
		{"25 o'clock", signRaw(priv, "team-1", swap(`"from":"02:00"`, `"from":"25:00"`)), Keyring{k}, 0, CodeMalformed},
		{"from 24:00", signRaw(priv, "team-1", swap(`"from":"02:00"`, `"from":"24:00"`)), Keyring{k}, 0, CodeMalformed},
		{"from equals to", signRaw(priv, "team-1", swap(`"from":"02:00"`, `"from":"05:00"`)), Keyring{k}, 0, CodeMalformed},
		{"health 4 minutes", signRaw(priv, "team-1", swap(`"healthMinutes":15`, `"healthMinutes":4`)), Keyring{k}, 0, CodeMalformed},
		{"health 121 minutes", signRaw(priv, "team-1", swap(`"healthMinutes":15`, `"healthMinutes":121`)), Keyring{k}, 0, CodeMalformed},
		{"nobody approved it", signRaw(priv, "team-1", swap(`"by":"ilya"`, `"by":""`)), Keyring{k}, 0, CodeMalformed},
		{"a line break in by", signRaw(priv, "team-1", swap(`"by":"ilya"`, `"by":"ilya\n[INFO] forged"`)), Keyring{k}, 0, CodeMalformed},
	}
	for _, c := range cases {
		_, _, err := c.kr.VerifySignedPolicy(c.s, c.min)
		if code(err) != c.want {
			t.Errorf("%s: %v, want %s", c.name, err, c.want)
		}
	}
	// the floor admits the newer key's policies
	s2, _ := SignPolicy(priv2, "team-2", goodPolicy())
	if _, key, err := (Keyring{k, k2}).VerifySignedPolicy(s2, 2); err != nil || key.Epoch != 2 {
		t.Fatalf("epoch 2: %v", err)
	}
}

// SignPolicy refuses what a box would refuse, so digi-mcp finds out at signing, not on the box.
func TestSignPolicyRefusesABadShape(t *testing.T) {
	priv, _ := policyTeamKey(t, "team-1", 1)
	cases := map[string]func(*UpgradePolicy){
		"no scope":           func(p *UpgradePolicy) { p.Scope = PolicyScope{} },
		"a tenant uppercase": func(p *UpgradePolicy) { p.Scope = PolicyScope{Type: ScopeTenant, Value: "BFL"} },
		"no seq":             func(p *UpgradePolicy) { p.Seq = 0 },
		"no expiry":          func(p *UpgradePolicy) { p.ExpiresAt = time.Time{} },
		"a target and hold":  func(p *UpgradePolicy) { p.Hold = true },
		"a target, no window": func(p *UpgradePolicy) {
			p.Window = PolicyWindow{}
		},
		"a target, no health minutes": func(p *UpgradePolicy) { p.HealthMinutes = 0 },
		"a hold with a bad window": func(p *UpgradePolicy) {
			p.Target, p.Hold, p.Window = nil, true, PolicyWindow{Days: []string{"mon"}, From: "9:00", To: "10:00"}
		},
		"another kind": func(p *UpgradePolicy) { p.Kind = "x" },
		"no by":        func(p *UpgradePolicy) { p.By = "" },
	}
	for name, edit := range cases {
		p := goodPolicy()
		edit(&p)
		if _, err := SignPolicy(priv, "team-1", p); code(err) == "" {
			t.Errorf("%s: signed", name)
		} else if !strings.Contains(err.Error(), "linkproto:") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := SignPolicy([]byte("short"), "team-1", goodPolicy()); err == nil {
		t.Error("signed with a short key")
	}
}

func TestPolicyTimes(t *testing.T) {
	p := goodPolicy()
	for _, c := range []struct {
		now  time.Time
		want string
	}{
		{policyNow, ""},
		{policyNow.Add(-MaxClockSkew), ""},
		{policyNow.Add(-MaxClockSkew - time.Second), CodeFromFuture},
		{p.ExpiresAt.Add(-time.Second), ""},
		{p.ExpiresAt, CodeExpired},
	} {
		if got := code(p.ActiveAt(c.now)); got != c.want {
			t.Errorf("at %s: %q, want %q", c.now, got, c.want)
		}
	}
}

func TestAppliesAndMostSpecific(t *testing.T) {
	mk := func(typ, value string, seq int64) UpgradePolicy {
		p := goodPolicy()
		p.Scope, p.Seq = PolicyScope{Type: typ, Value: value}, seq
		return p
	}
	all, ring, tenant, box := mk(ScopeAll, "", 3), mk(ScopeRing, "canary", 9), mk(ScopeTenant, "bflstore", 2), mk(ScopeBox, "bfl-read", 1)
	for _, c := range []struct {
		p                 UpgradePolicy
		box, tenant, ring string
		want              bool
	}{
		{all, "", "", "", true},
		{ring, "bfl-read", "bflstore", "canary", true},
		{ring, "bfl-read", "bflstore", "stable", false},
		{ring, "bfl-read", "bflstore", "", false},
		{tenant, "bfl-read", "bflstore", "", true},
		{tenant, "bfl-read", "", "", false},
		{tenant, "bfl-read", "tamig", "", false},
		{box, "bfl-read", "", "", true},
		{box, "bfl-write", "bflstore", "canary", false},
		{mk("region", "x", 1), "bfl-read", "", "", false},
	} {
		if got := Applies(c.p, c.box, c.tenant, c.ring); got != c.want {
			t.Errorf("%s for %s/%s/%s: %v", c.p.Scope, c.box, c.tenant, c.ring, got)
		}
	}
	order := []struct {
		in   []UpgradePolicy
		want string
	}{
		{[]UpgradePolicy{all, ring, tenant, box}, "box:bfl-read"},
		{[]UpgradePolicy{box, tenant, ring, all}, "box:bfl-read"},
		{[]UpgradePolicy{all, ring}, "ring:canary"},
		{[]UpgradePolicy{ring, tenant}, "tenant:bflstore"}, // a tenant's decision outranks a rollout ring
		{[]UpgradePolicy{all}, "all"},
	}
	for _, c := range order {
		if p, ok := MostSpecific(c.in); !ok || p.Scope.Key() != c.want {
			t.Errorf("%v: %s", c.in, p.Scope)
		}
	}
	if _, ok := MostSpecific(nil); ok {
		t.Error("nothing decided something")
	}
	newer := mk(ScopeAll, "", 7)
	if p, _ := MostSpecific([]UpgradePolicy{all, newer}); p.Seq != 7 {
		t.Errorf("a tie goes to the higher seq: %d", p.Seq)
	}
	// Effective: the expired box policy and the other ring's do not count
	expired := box
	expired.ExpiresAt = policyNow.Add(-time.Minute)
	expired.IssuedAt = policyNow.Add(-time.Hour)
	p, ok := Effective([]UpgradePolicy{all, ring, expired}, "bfl-read", "bflstore", "stable", policyNow)
	if !ok || p.Scope.Key() != "all" {
		t.Fatalf("effective %s %v", p.Scope, ok)
	}
	if _, ok := Effective([]UpgradePolicy{expired}, "bfl-read", "", "", policyNow); ok {
		t.Fatal("an expired policy decided")
	}
}

func TestWindows(t *testing.T) {
	ist := time.FixedZone("IST", 2*3600)
	day := func(d, h, m int) time.Time { return time.Date(2026, 11, d, h, m, 0, 0, ist) } // 2026-11-16 is a Monday
	nightly := PolicyWindow{Days: []string{"mon", "tue", "wed", "thu", "sun"}, From: "02:00", To: "05:00"}
	overnight := PolicyWindow{Days: []string{"sat"}, From: "22:00", To: "04:00"}
	wholeDay := PolicyWindow{Days: []string{"fri"}, From: "00:00", To: "24:00"}
	for _, c := range []struct {
		name  string
		w     PolicyWindow
		t     time.Time
		in    bool
		start time.Time
	}{
		{"Monday 03:00", nightly, day(16, 3, 0), true, day(16, 2, 0)},
		{"Monday 02:00 sharp", nightly, day(16, 2, 0), true, day(16, 2, 0)},
		{"Monday 05:00 sharp is out", nightly, day(16, 5, 0), false, time.Time{}},
		{"Monday 01:59", nightly, day(16, 1, 59), false, time.Time{}},
		{"Friday 03:00 is not a listed day", nightly, day(20, 3, 0), false, time.Time{}},
		{"Saturday 23:00", overnight, day(21, 23, 0), true, day(21, 22, 0)},
		{"Sunday 03:59 belongs to Saturday", overnight, day(22, 3, 59), true, day(21, 22, 0)},
		{"Sunday 04:00 is out", overnight, day(22, 4, 0), false, time.Time{}},
		{"Sunday 23:00 is not Saturday's", overnight, day(22, 23, 0), false, time.Time{}},
		{"Saturday 03:00 is not Friday's", overnight, day(21, 3, 0), false, time.Time{}},
		{"Friday 23:59 in a whole day", wholeDay, day(20, 23, 59), true, day(20, 0, 0)},
		{"Saturday 00:00 after a whole Friday", wholeDay, day(21, 0, 0), false, time.Time{}},
	} {
		start, in := c.w.Start(c.t)
		if in != c.in || !start.Equal(c.start) || c.w.Contains(c.t) != c.in {
			t.Errorf("%s: in %v start %s", c.name, in, start)
		}
	}
	// the next start: later today, tomorrow, or after a run of unlisted days
	for _, c := range []struct {
		w    PolicyWindow
		t    time.Time
		want time.Time
	}{
		{nightly, day(16, 1, 0), day(16, 2, 0)},
		{nightly, day(16, 2, 0), day(17, 2, 0)}, // inside one: the next occurrence
		{nightly, day(19, 6, 0), day(22, 2, 0)}, // Thursday after the window → Sunday
		{overnight, day(16, 12, 0), day(21, 22, 0)},
	} {
		if got := c.w.NextStart(c.t); !got.Equal(c.want) {
			t.Errorf("next after %s: %s, want %s", c.t, got, c.want)
		}
	}
	if (PolicyWindow{}).Contains(day(16, 3, 0)) || !(PolicyWindow{}).NextStart(day(16, 3, 0)).IsZero() {
		t.Error("an empty window opens")
	}
}

func TestPolicyDelivery(t *testing.T) {
	priv, _ := policyTeamKey(t, "team-1", 1)
	s, _ := SignPolicy(priv, "team-1", goodPolicy())
	good := PolicyDelivery{V: 1, Box: "bfl-read", Tenant: "bflstore", Ring: "canary", Policies: []SignedPolicy{s}}
	b, _ := json.Marshal(good)
	d, err := DecodePolicyDelivery(b)
	if err != nil || d.Box != "bfl-read" || len(d.Policies) != 1 || !bytes.Equal(d.Policies[0].Policy, s.Policy) {
		t.Fatalf("%+v %v", d, err)
	}
	// a newer relay's extra field does not stop an older box
	if _, err := DecodePolicyDelivery([]byte(`{"v":1,"box":"bfl-read","policies":[],"region":"eu"}`)); err != nil {
		t.Fatalf("an unknown field: %v", err)
	}
	for in, want := range map[string]string{
		`nope`:                                   CodeMalformed,
		`{"v":2,"box":"bfl-read","policies":[]}`: CodeVersion,
		`{"v":1,"box":"../x","policies":[]}`:     CodeMalformed,
		`{"v":1,"box":"bfl-read","tenant":"BFL","policies":[]}`:            CodeMalformed,
		`{"v":1,"box":"bfl-read","ring":"a b","policies":[]}`:              CodeMalformed,
		`{"v":1,"box":"bfl-read","policies":[{"policy":"%%%"}]}`:           CodeMalformed,
		`{"v":1,"box":"bfl-read","policies":[{},{},{},{},{}]}`:             CodeMalformed,
		`{"v":1,"box":"bfl-read","policies":[]} {"v":1,"box":"bfl-write"}`: CodeMalformed,
	} {
		if _, err := DecodePolicyDelivery([]byte(in)); code(err) != want {
			t.Errorf("%s: %v, want %s", in, err, want)
		}
	}
}

func TestReports(t *testing.T) {
	k := mustKeys(t)
	r := Report{Box: "bfl-read", At: policyNow, Build: "v1.0.31",
		Effective:   ReportEffective{Version: "1.0.32", Why: WhyScheduled},
		AutoUpgrade: &ReportAuto{Ticket: strings.Repeat("ab", 16), From: "1.0.31", To: "1.0.32", Outcome: OutcomeRolledBack}}
	s, err := SignReport(k, r)
	if err != nil {
		t.Fatal(err)
	}
	got, err := VerifyReport(k.Public(), s, "bfl-read")
	if err != nil || got.V != 1 || got.AutoUpgrade.Outcome != OutcomeRolledBack || got.Policies == nil {
		t.Fatalf("%+v %v", got, err)
	}
	if !strings.Contains(string(s.Body), `"policies":[]`) || !strings.Contains(string(s.Body), `"effective":{"version":"1.0.32","hold":false,"why":"scheduled"}`) {
		t.Fatalf("body %s", s.Body)
	}
	if _, err := VerifyReport(k.Public(), s, "bfl-write"); code(err) != CodeWrongBox {
		t.Errorf("another box: %v", err)
	}
	if _, err := VerifyReport(mustKeys(t).Public(), s, "bfl-read"); code(err) != CodeBadSignature {
		t.Errorf("another key: %v", err)
	}
	tampered := s
	tampered.Body = []byte(strings.Replace(string(s.Body), "rolled_back", "ok", 1))
	if _, err := VerifyReport(k.Public(), tampered, "bfl-read"); code(err) != CodeBadSignature {
		t.Errorf("a relay's edit: %v", err)
	}
	// a newer box's extra field verifies
	body := []byte(`{"v":1,"box":"bfl-read","at":"2026-11-16T09:00:00Z","build":"v1.0.40","policies":[],"effective":{"hold":true,"why":"no_policy"},"newThing":1}`)
	sig := ed25519.Sign(k.Sign, append([]byte(domainReport), body...))
	if _, err := VerifyReport(k.Public(), SignedReport{Body: body, Sig: base64.StdEncoding.EncodeToString(sig)}, "bfl-read"); err != nil {
		t.Errorf("an unknown field: %v", err)
	}
}

func TestScopeKeysAndShapes(t *testing.T) {
	for s, want := range map[PolicyScope]string{
		{Type: ScopeAll}: "all", {Type: ScopeRing, Value: "canary"}: "ring:canary",
		{Type: ScopeTenant, Value: "bflstore"}: "tenant:bflstore", {Type: ScopeBox, Value: "bfl-read"}: "box:bfl-read",
	} {
		if s.Key() != want {
			t.Errorf("%+v: %s", s, s.Key())
		}
	}
	for _, ok := range []string{"bflstore", "b4l", "canary", "ring-2"} {
		if !ValidTenant(ok) || !ValidRing(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "a", "-x", "Canary", "a b", "a_b", strings.Repeat("a", 64)} {
		if ValidTenant(bad) || ValidRing(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
