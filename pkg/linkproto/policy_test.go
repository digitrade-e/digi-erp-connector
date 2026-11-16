package linkproto

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
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

func boxesScope(ids ...string) PolicyScope { return PolicyScope{Type: ScopeBoxes, Boxes: ids} }

func goodPolicy() UpgradePolicy {
	return UpgradePolicy{
		Scope: boxesScope("bfl-read", "bfl-write"), Seq: 12,
		IssuedAt: policyNow, ExpiresAt: policyNow.Add(30 * 24 * time.Hour),
		Target:        &PolicyTarget{Version: "1.0.31", SHA256: strings.Repeat("ab", 32)},
		Rollbacks:     []RollbackPin{{Version: "1.0.29", SHA256: strings.Repeat("29", 32)}, {Version: "1.0.30", SHA256: strings.Repeat("30", 32)}},
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
	if p.V != 1 || p.Kind != PolicyKindUpgrade || p.Seq != 12 || p.Scope.Key() != "boxes" || p.Target.Version != "1.0.31" ||
		!p.IssuedAt.Equal(policyNow.Add(time.Second)) || p.IssuedAt.Location() != time.UTC || len(p.Rollbacks) != 2 {
		t.Fatalf("%+v", p)
	}
	// the wire form: policy is base64 of the exact bytes, which carry the contract's field names
	wire, _ := json.Marshal(s)
	var back SignedPolicy
	if err := json.Unmarshal(wire, &back); err != nil || !bytes.Equal(back.Policy, s.Policy) {
		t.Fatalf("round trip: %v", err)
	}
	for _, field := range []string{`"v":1`, `"kind":"upgrade-policy"`, `"scope":{"type":"boxes","boxes":["bfl-read","bfl-write"]}`, `"seq":12`,
		`"issuedAt":"2026-11-16T09:00:01Z"`, `"target":{"version":"1.0.31","sha256":"`,
		`"rollbacks":[{"version":"1.0.29","sha256":"` + strings.Repeat("29", 32) + `"},{"version":"1.0.30","sha256":"`, `"hold":false`,
		`"window":{"days":["mon","tue","wed","thu","sun"],"from":"02:00","to":"05:00"}`, `"healthMinutes":15`, `"by":"ilya"`} {
		if !strings.Contains(string(s.Policy), field) {
			t.Errorf("the signed bytes lack %s: %s", field, s.Policy)
		}
	}
	if !strings.Contains(string(wire), `"keyId":"team-1"`) || !strings.Contains(string(wire), `"policy":"`+base64.StdEncoding.EncodeToString(s.Policy)+`"`) {
		t.Fatalf("wire %s", wire)
	}
	// a hold needs no target, window, health minutes or rollbacks; every box's scope is {"type":"all"}
	hold := UpgradePolicy{Scope: PolicyScope{Type: ScopeAll}, Seq: 1, IssuedAt: policyNow, ExpiresAt: policyNow.Add(time.Hour), Hold: true, By: "ilya"}
	if s, err := SignPolicy(priv, "team-1", hold); err != nil {
		t.Fatal(err)
	} else if p, err := VerifyPolicy(s, Keyring{k}); err != nil || !p.Holds() || !strings.Contains(string(s.Policy), `"scope":{"type":"all"},`) ||
		strings.Contains(string(s.Policy), "rollbacks") {
		t.Fatalf("hold: %+v %v %s", p, err, s.Policy)
	}
}

// D9: one encoding per box list — SignPolicy sorts it (and the rollbacks), a verifier refuses any other order.
func TestSignPolicySortsListsIntoTheirOneEncoding(t *testing.T) {
	priv, k := policyTeamKey(t, "team-1", 1)
	in := goodPolicy()
	in.Scope = boxesScope("tamig-1", "bfl-write", "bfl-read")
	in.Rollbacks = []RollbackPin{{Version: "1.0.30", SHA256: strings.Repeat("30", 32)}, {Version: "1.0.9", SHA256: strings.Repeat("09", 32)}}
	s, err := SignPolicy(priv, "team-1", in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(s.Policy), `"boxes":["bfl-read","bfl-write","tamig-1"]`) ||
		!strings.Contains(string(s.Policy), `"rollbacks":[{"version":"1.0.9"`) {
		t.Fatalf("not sorted: %s", s.Policy)
	}
	if in.Scope.Boxes[0] != "tamig-1" || in.Rollbacks[0].Version != "1.0.30" {
		t.Fatal("SignPolicy sorted the caller's slices in place")
	}
	if _, err := VerifyPolicy(s, Keyring{k}); err != nil {
		t.Fatal(err)
	}
	// a box listed twice or a version pinned twice is a signer's mistake: refused, never dropped silently
	twice := goodPolicy()
	twice.Scope = boxesScope("bfl-read", "bfl-read")
	if _, err := SignPolicy(priv, "team-1", twice); code(err) != CodeMalformed {
		t.Fatalf("a box listed twice: %v", err)
	}
	twice = goodPolicy()
	twice.Rollbacks = append(twice.Rollbacks, RollbackPin{Version: "1.0.30", SHA256: strings.Repeat("cd", 32)})
	if _, err := SignPolicy(priv, "team-1", twice); code(err) != CodeMalformed {
		t.Fatalf("a version pinned twice: %v", err)
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
	list := `{"type":"boxes","boxes":["bfl-read","bfl-write"]}`
	manyBoxes := make([]string, MaxScopeBoxes+1)
	for i := range manyBoxes {
		manyBoxes[i] = fmt.Sprintf(`"box-%04d"`, i)
	}
	manyPins := make([]string, MaxRollbacks+1)
	for i := range manyPins {
		manyPins[i] = fmt.Sprintf(`{"version":"0.0.%d","sha256":"%s"}`, i, strings.Repeat("ab", 32))
	}
	pins := `"rollbacks":[{"version":"1.0.29","sha256":"` + strings.Repeat("29", 32) + `"},{"version":"1.0.30","sha256":"` + strings.Repeat("30", 32) + `"}]`

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
		// D9: every box, or an explicit list — a relay's tenant or ring is no scope
		{"an unknown scope type", signRaw(priv, "team-1", swap(list, `{"type":"region","boxes":["bfl-read"]}`)), Keyring{k}, 0, CodeMalformed},
		{"a ring scope", signRaw(priv, "team-1", swap(list, `{"type":"ring","value":"canary"}`)), Keyring{k}, 0, CodeMalformed},
		{"a tenant scope", signRaw(priv, "team-1", swap(list, `{"type":"tenant","value":"bflstore"}`)), Keyring{k}, 0, CodeMalformed},
		{"a one-box scope of the old shape", signRaw(priv, "team-1", swap(list, `{"type":"box","value":"bfl-read"}`)), Keyring{k}, 0, CodeMalformed},
		{"all with a list", signRaw(priv, "team-1", swap(`"type":"boxes"`, `"type":"all"`)), Keyring{k}, 0, CodeMalformed},
		{"all with an empty list", signRaw(priv, "team-1", swap(list, `{"type":"all","boxes":[]}`)), Keyring{k}, 0, CodeMalformed},
		{"an empty list", signRaw(priv, "team-1", swap(list, `{"type":"boxes","boxes":[]}`)), Keyring{k}, 0, CodeMalformed},
		{"a list out of order", signRaw(priv, "team-1", swap(list, `{"type":"boxes","boxes":["bfl-write","bfl-read"]}`)), Keyring{k}, 0, CodeMalformed},
		{"a box listed twice", signRaw(priv, "team-1", swap(list, `{"type":"boxes","boxes":["bfl-read","bfl-read"]}`)), Keyring{k}, 0, CodeMalformed},
		{"a box id that is not one", signRaw(priv, "team-1", swap(list, `{"type":"boxes","boxes":["../x"]}`)), Keyring{k}, 0, CodeMalformed},
		{"257 boxes", signRaw(priv, "team-1", swap(list, `{"type":"boxes","boxes":[`+strings.Join(manyBoxes, ",")+`]}`)), Keyring{k}, 0, CodeMalformed},
		{"seq 0", signRaw(priv, "team-1", swap(`"seq":12`, `"seq":0`)), Keyring{k}, 0, CodeMalformed},
		{"seq negative", signRaw(priv, "team-1", swap(`"seq":12`, `"seq":-3`)), Keyring{k}, 0, CodeMalformed},
		// D13, review m1: a stolen key cannot put a scope out of reach
		{"seq above 2^31-1", signRaw(priv, "team-1", swap(`"seq":12`, `"seq":2147483648`)), Keyring{k}, 0, CodeMalformed},
		{"seq 2^53", signRaw(priv, "team-1", swap(`"seq":12`, `"seq":9007199254740992`)), Keyring{k}, 0, CodeMalformed},
		{"expires before issued", signRaw(priv, "team-1", swap(`"expiresAt":"2026-12-16T09:00:00Z"`, `"expiresAt":"2026-11-16T08:00:00Z"`)), Keyring{k}, 0, CodeMalformed},
		// D13, review m16: 90 days at most, for every verifier (the relay too)
		{"lives 91 days", signRaw(priv, "team-1", swap(`"expiresAt":"2026-12-16T09:00:00Z"`, `"expiresAt":"2027-02-15T09:00:00Z"`)), Keyring{k}, 0, CodeLifetime},
		{"lives two years", signRaw(priv, "team-1", swap(`"expiresAt":"2026-12-16T09:00:00Z"`, `"expiresAt":"2028-11-16T09:00:00Z"`)), Keyring{k}, 0, CodeLifetime},
		{"a hold with a target", signRaw(priv, "team-1", swap(`"hold":false`, `"hold":true`)), Keyring{k}, 0, CodeMalformed},
		{"a version with a v", signRaw(priv, "team-1", swap(`"version":"1.0.31"`, `"version":"v1.0.31"`)), Keyring{k}, 0, CodeMalformed},
		{"a version with a path", signRaw(priv, "team-1", swap(`"version":"1.0.31"`, `"version":"1.0.31/../x"`)), Keyring{k}, 0, CodeMalformed},
		{"an uppercase digest", signRaw(priv, "team-1", swap(strings.Repeat("ab", 32), strings.Repeat("AB", 32))), Keyring{k}, 0, CodeMalformed},
		{"a short digest", signRaw(priv, "team-1", swap(strings.Repeat("ab", 32), "abc")), Keyring{k}, 0, CodeMalformed},
		// D8: the signed rollback digests
		{"17 rollbacks", signRaw(priv, "team-1", swap(pins, `"rollbacks":[`+strings.Join(manyPins, ",")+`]`)), Keyring{k}, 0, CodeMalformed},
		{"rollbacks out of order", signRaw(priv, "team-1", swap(pins, `"rollbacks":[{"version":"1.0.30","sha256":"`+strings.Repeat("30", 32)+`"},{"version":"1.0.29","sha256":"`+strings.Repeat("29", 32)+`"}]`)), Keyring{k}, 0, CodeMalformed},
		{"a version pinned twice", signRaw(priv, "team-1", swap(pins, `"rollbacks":[{"version":"1.0.30","sha256":"`+strings.Repeat("29", 32)+`"},{"version":"1.0.30","sha256":"`+strings.Repeat("30", 32)+`"}]`)), Keyring{k}, 0, CodeMalformed},
		{"a rollback to the target", signRaw(priv, "team-1", swap(pins, `"rollbacks":[{"version":"1.0.31","sha256":"`+strings.Repeat("31", 32)+`"}]`)), Keyring{k}, 0, CodeMalformed},
		{"a rollback newer than the target", signRaw(priv, "team-1", swap(pins, `"rollbacks":[{"version":"1.0.32","sha256":"`+strings.Repeat("32", 32)+`"}]`)), Keyring{k}, 0, CodeMalformed},
		{"a rollback with a v", signRaw(priv, "team-1", swap(pins, `"rollbacks":[{"version":"v1.0.30","sha256":"`+strings.Repeat("30", 32)+`"}]`)), Keyring{k}, 0, CodeMalformed},
		{"a rollback's uppercase digest", signRaw(priv, "team-1", swap(pins, `"rollbacks":[{"version":"1.0.30","sha256":"`+strings.Repeat("AB", 32)+`"}]`)), Keyring{k}, 0, CodeMalformed},
		{"a rollback with an unknown field", signRaw(priv, "team-1", swap(pins, `"rollbacks":[{"version":"1.0.30","sha256":"`+strings.Repeat("30", 32)+`","url":"x"}]`)), Keyring{k}, 0, CodeMalformed},
		{"no rollbacks spelt as an empty list", signRaw(priv, "team-1", swap(pins, `"rollbacks":[]`)), Keyring{k}, 0, CodeMalformed},
		{"no days", signRaw(priv, "team-1", swap(`["mon","tue","wed","thu","sun"]`, `[]`)), Keyring{k}, 0, CodeMalformed},
		{"a day twice", signRaw(priv, "team-1", swap(`["mon","tue","wed","thu","sun"]`, `["mon","mon"]`)), Keyring{k}, 0, CodeMalformed},
		{"an unknown day", signRaw(priv, "team-1", swap(`["mon","tue","wed","thu","sun"]`, `["monday"]`)), Keyring{k}, 0, CodeMalformed},
		{"25 o'clock", signRaw(priv, "team-1", swap(`"from":"02:00"`, `"from":"25:00"`)), Keyring{k}, 0, CodeMalformed},
		{"from 24:00", signRaw(priv, "team-1", swap(`"from":"02:00"`, `"from":"24:00"`)), Keyring{k}, 0, CodeMalformed},
		{"from equals to", signRaw(priv, "team-1", swap(`"from":"02:00"`, `"from":"05:00"`)), Keyring{k}, 0, CodeMalformed},
		// review m17: the window holds the upgrade, its health check and a rollback's start
		{"a window shorter than health + 30 min", signRaw(priv, "team-1", swap(`"from":"02:00","to":"05:00"`, `"from":"02:00","to":"02:44"`)), Keyring{k}, 0, CodeMalformed},
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
	// the edges that are allowed: seq 2^31-1, 90 days, a window of exactly health + 30 min, 256 boxes, 16 rollbacks
	edge := goodPolicy()
	edge.Seq, edge.ExpiresAt = MaxSeq, edge.IssuedAt.Add(MaxPolicyLifetime)
	edge.Window = PolicyWindow{Days: []string{"mon"}, From: "23:30", To: "00:15"} // 45 minutes, past midnight
	edge.Scope = boxesScope()
	for i := range MaxScopeBoxes {
		edge.Scope.Boxes = append(edge.Scope.Boxes, fmt.Sprintf("%s-%03d", strings.Repeat("b", 59), i)) // 63 characters each
	}
	edge.Rollbacks = nil
	for i := range MaxRollbacks {
		edge.Rollbacks = append(edge.Rollbacks, RollbackPin{Version: fmt.Sprintf("1.0.%d", i), SHA256: strings.Repeat("cd", 32)})
	}
	if se, err := SignPolicy(priv, "team-1", edge); err != nil {
		t.Fatalf("the edges: %v", err)
	} else if _, err := VerifyPolicy(se, Keyring{k}); err != nil {
		t.Fatalf("the edges verify: %v", err)
	} else if len(se.Policy) > maxPolicyBytes {
		t.Fatalf("a full policy is %d bytes, over the %d a verifier takes", len(se.Policy), maxPolicyBytes)
	} else if d, _ := json.Marshal(PolicyDelivery{V: 1, Box: "bfl-read", Policies: []SignedPolicy{se, se, se, se}}); len(d) > MaxPolicyFrame {
		t.Fatalf("a delivery of four full policies is %d bytes, over MaxPolicyFrame", len(d))
	}
}

// SignPolicy refuses what a box would refuse, so digi-mcp finds out at signing, not on the box.
func TestSignPolicyRefusesABadShape(t *testing.T) {
	priv, _ := policyTeamKey(t, "team-1", 1)
	cases := map[string]func(*UpgradePolicy){
		"no scope":           func(p *UpgradePolicy) { p.Scope = PolicyScope{} },
		"a ring":             func(p *UpgradePolicy) { p.Scope = PolicyScope{Type: "ring"} },
		"an uppercase box":   func(p *UpgradePolicy) { p.Scope = boxesScope("BFL-READ") },
		"an empty list":      func(p *UpgradePolicy) { p.Scope = boxesScope() },
		"all with a box":     func(p *UpgradePolicy) { p.Scope = PolicyScope{Type: ScopeAll, Boxes: []string{"bfl-read"}} },
		"no seq":             func(p *UpgradePolicy) { p.Seq = 0 },
		"seq over the limit": func(p *UpgradePolicy) { p.Seq = MaxSeq + 1 },
		"no expiry":          func(p *UpgradePolicy) { p.ExpiresAt = time.Time{} },
		"a target and hold":  func(p *UpgradePolicy) { p.Hold = true },
		"a target, no window": func(p *UpgradePolicy) {
			p.Window = PolicyWindow{}
		},
		"a target, no health minutes": func(p *UpgradePolicy) { p.HealthMinutes = 0 },
		"a hold with a bad window": func(p *UpgradePolicy) {
			p.Target, p.Hold, p.Rollbacks, p.Window = nil, true, nil, PolicyWindow{Days: []string{"mon"}, From: "9:00", To: "10:00"}
		},
		"a hold with rollbacks":   func(p *UpgradePolicy) { p.Target, p.Hold = nil, true },
		"a rollback's bad digest": func(p *UpgradePolicy) { p.Rollbacks = []RollbackPin{{Version: "1.0.30", SHA256: "x"}} },
		"a window too short":      func(p *UpgradePolicy) { p.HealthMinutes, p.Window.To = 120, "04:00" },
		"lives 91 days":           func(p *UpgradePolicy) { p.ExpiresAt = p.IssuedAt.Add(91 * 24 * time.Hour) },
		"another kind":            func(p *UpgradePolicy) { p.Kind = "x" },
		"no by":                   func(p *UpgradePolicy) { p.By = "" },
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
		if p.HoldsAt(c.now) != (c.want != "") {
			t.Errorf("at %s: HoldsAt %v", c.now, p.HoldsAt(c.now))
		}
	}
}

// D9 (review M1): what applies is the signed scope against the box's own id — no tenant or ring a relay states.
func TestAppliesIsTheBoxIDOnly(t *testing.T) {
	mk := func(sc PolicyScope) UpgradePolicy { p := goodPolicy(); p.Scope = sc; return p }
	all, list := mk(PolicyScope{Type: ScopeAll}), mk(boxesScope("bfl-read", "bfl-write"))
	for _, c := range []struct {
		p    UpgradePolicy
		box  string
		want bool
	}{
		{all, "bfl-read", true},
		{all, "", true},
		{list, "bfl-read", true},
		{list, "bfl-write", true},
		{list, "tamig-1", false},
		{list, "", false},
		{list, "bfl", false}, // a prefix of a listed id is not it
		{mk(PolicyScope{Type: "ring", Boxes: []string{"bfl-read"}}), "bfl-read", false},
	} {
		if got := Applies(c.p, c.box); got != c.want {
			t.Errorf("%s for %q: %v", c.p.Scope, c.box, got)
		}
	}
	if s := list.Scope; s.Key() != "boxes" || s.String() != "boxes[2]" || s.Ref().String() != "boxes" || len(s.Ref().Boxes) != 0 || s.Rank() != 1 {
		t.Fatalf("%+v", s)
	}
	if s := all.Scope; s.Key() != "all" || s.String() != "all" || s.Rank() != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestRollbackFor(t *testing.T) {
	p := goodPolicy()
	if r, ok := p.RollbackFor("v1.0.30"); !ok || r.SHA256 != strings.Repeat("30", 32) {
		t.Fatalf("1.0.30: %+v %v", r, ok)
	}
	if r, ok := p.RollbackFor("1.0.29"); !ok || r.Version != "1.0.29" {
		t.Fatalf("1.0.29: %+v %v", r, ok)
	}
	for _, v := range []string{"1.0.28", "1.0.31", "dev", ""} {
		if _, ok := p.RollbackFor(v); ok {
			t.Errorf("%q has a rollback", v)
		}
	}
}

// Decide (plan 088 D3, D9, D10; review M2, m9).
func TestDecide(t *testing.T) {
	at := func(h int) time.Time { return policyNow.Add(time.Duration(h) * time.Hour) }
	moves := func(sc PolicyScope, seq int64, version string, issued time.Time) UpgradePolicy {
		p := goodPolicy()
		p.Scope, p.Seq, p.IssuedAt, p.ExpiresAt, p.Rollbacks = sc, seq, issued, issued.Add(30*24*time.Hour), nil
		p.Target = &PolicyTarget{Version: version, SHA256: strings.Repeat("ab", 32)}
		return p
	}
	holds := func(sc PolicyScope, seq int64, issued time.Time) UpgradePolicy {
		return UpgradePolicy{Scope: sc, Seq: seq, IssuedAt: issued, ExpiresAt: issued.Add(7 * 24 * time.Hour), Hold: true, By: "ilya"}
	}
	expired := func(p UpgradePolicy) UpgradePolicy { p.ExpiresAt = at(9); return p }
	all := PolicyScope{Type: ScopeAll}
	bfl := boxesScope("bfl-read", "bfl-write")
	canary := boxesScope("bfl-read", "tamig-1")
	now := at(10)
	cases := []struct {
		name    string
		ps      []UpgradePolicy
		hold    bool
		why     string
		scope   string
		seq     int64
		braked  bool
		version string
	}{
		{"nothing", nil, true, WhyNoPolicy, "", 0, false, ""},
		{"another box's list only", []UpgradePolicy{moves(boxesScope("tamig-1"), 3, "1.0.31", at(1))}, true, WhyNoPolicy, "", 0, false, ""},
		{"every box's target", []UpgradePolicy{moves(all, 5, "1.0.41", at(1))}, false, "", "all", 5, false, "1.0.41"},
		{"the list outranks every box", []UpgradePolicy{moves(all, 5, "1.0.41", at(2)), moves(canary, 7, "1.0.42", at(1))}, false, "", "boxes", 7, false, "1.0.42"},
		{"a list's hold outranks every box's later target", []UpgradePolicy{holds(bfl, 3, at(1)), moves(all, 20, "1.0.40", at(2))}, true, WhyPolicyHolds, "boxes", 3, false, ""},
		// review M2: the inverse of a bad 1.0.41 is all#6 hold; the canary's list still targets it
		{"a later all hold brakes the list's target", []UpgradePolicy{moves(canary, 3, "1.0.41", at(1)), moves(all, 5, "1.0.41", at(2)), holds(all, 6, at(3))},
			true, WhyPolicyHolds, "all", 6, true, ""},
		{"a hold at the same second brakes too", []UpgradePolicy{moves(canary, 3, "1.0.41", at(1)), holds(all, 6, at(1))}, true, WhyPolicyHolds, "all", 6, true, ""},
		{"an older hold does not brake a newer target", []UpgradePolicy{holds(all, 6, at(1)), moves(canary, 7, "1.0.42", at(2))}, false, "", "boxes", 7, false, "1.0.42"},
		// review m9: an expired policy holds for its scope; it never falls through to a less specific one
		{"an expired list hold does not lapse into every box's target", []UpgradePolicy{expired(holds(bfl, 3, at(2))), moves(all, 20, "1.0.40", at(1))},
			true, WhyPolicyExpired, "boxes", 3, false, ""},
		{"an expired list target holds", []UpgradePolicy{expired(moves(canary, 3, "1.0.41", at(1))), moves(all, 20, "1.0.41", at(2))}, true, WhyPolicyExpired, "boxes", 3, false, ""},
		{"an expired all holds", []UpgradePolicy{expired(moves(all, 5, "1.0.41", at(1)))}, true, WhyPolicyExpired, "all", 5, false, ""},
		{"an expired all hold still brakes", []UpgradePolicy{moves(canary, 3, "1.0.41", at(1)), expired(holds(all, 6, at(2)))}, true, WhyPolicyHolds, "all", 6, true, ""},
		{"the newest of a key", []UpgradePolicy{moves(canary, 3, "1.0.41", at(1)), moves(bfl, 4, "1.0.39", at(2))}, false, "", "boxes", 4, false, "1.0.39"},
		{"the newest of a key, a superseded hold", []UpgradePolicy{holds(bfl, 3, at(2)), moves(canary, 4, "1.0.42", at(1))}, false, "", "boxes", 4, false, "1.0.42"},
		{"a policy from the future holds", []UpgradePolicy{moves(all, 5, "1.0.41", now.Add(time.Hour))}, true, WhyPolicyExpired, "all", 5, false, ""},
	}
	for _, c := range cases {
		d := Decide(c.ps, "bfl-read", now)
		got := ""
		if d.Found {
			got = d.Policy.Scope.Key()
		}
		if d.Hold != c.hold || d.Why != c.why || got != c.scope || (d.Found && d.Policy.Seq != c.seq) || (d.Braked != nil) != c.braked {
			t.Errorf("%s: %+v", c.name, d)
			continue
		}
		if !c.hold && d.Policy.Target.Version != c.version {
			t.Errorf("%s: target %s", c.name, d.Policy.Target.Version)
		}
		if p, ok := Effective(c.ps, "bfl-read", now); ok != d.Found || (ok && (p.Seq != d.Policy.Seq || p.HoldsAt(now) != d.Hold)) {
			t.Errorf("%s: Effective %+v %v", c.name, p, ok)
		}
	}
	// MostSpecific alone: a list over every box, then the higher seq
	if p, ok := MostSpecific([]UpgradePolicy{moves(all, 9, "1.0.41", at(1)), moves(canary, 2, "1.0.41", at(1))}); !ok || p.Scope.Key() != "boxes" {
		t.Fatalf("%+v", p)
	}
	if p, _ := MostSpecific([]UpgradePolicy{moves(all, 3, "1.0.41", at(1)), moves(all, 7, "1.0.41", at(1))}); p.Seq != 7 {
		t.Errorf("a tie goes to the higher seq: %d", p.Seq)
	}
	if _, ok := MostSpecific(nil); ok {
		t.Error("nothing decided something")
	}
}

// D8: the upgrade command's spec may carry the signed digest of the build the box runs now.
func TestUpgradeSpec(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	for _, c := range []struct {
		spec UpgradeSpec
		ok   bool
	}{
		{UpgradeSpec{Version: "1.0.31", SHA256: digest}, true},
		{UpgradeSpec{Version: "v1.0.31", SHA256: strings.ToUpper(digest), AssumeIdle: true, HealthMinutes: 45}, true},
		{UpgradeSpec{Version: "1.0.31", SHA256: digest, Rollback: &RollbackPin{Version: "1.0.30", SHA256: digest}}, true},
		{UpgradeSpec{Version: "1.0", SHA256: digest}, false},
		{UpgradeSpec{Version: "1.0.31", SHA256: "abc"}, false},
		{UpgradeSpec{Version: "1.0.31", SHA256: digest, HealthMinutes: 4}, false},
		{UpgradeSpec{Version: "1.0.31", SHA256: digest, Rollback: &RollbackPin{Version: "1.0.31", SHA256: digest}}, false},
		{UpgradeSpec{Version: "1.0.31", SHA256: digest, Rollback: &RollbackPin{Version: "1.0.32", SHA256: digest}}, false},
		{UpgradeSpec{Version: "1.0.31", SHA256: digest, Rollback: &RollbackPin{Version: "1.0.30", SHA256: "x"}}, false},
		{UpgradeSpec{Version: "1.0.31", SHA256: digest, Rollback: &RollbackPin{Version: "../1.0.30", SHA256: digest}}, false},
	} {
		if err := c.spec.Check(); (err == nil) != c.ok {
			t.Errorf("%+v: %v", c.spec, err)
		}
	}
	b, _ := json.Marshal(UpgradeSpec{Version: "1.0.31", SHA256: digest, Rollback: &RollbackPin{Version: "1.0.30", SHA256: digest}})
	if string(b) != `{"version":"1.0.31","sha256":"`+digest+`","rollback":{"version":"1.0.30","sha256":"`+digest+`"}}` {
		t.Fatalf("%s", b)
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
	for w, want := range map[*PolicyWindow]int{&nightly: 180, &overnight: 360, &wholeDay: 1440, {}: 0} {
		if got := w.minutes(); got != want {
			t.Errorf("%+v lasts %d minutes, want %d", *w, got, want)
		}
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
		AutoUpgrade: &ReportAuto{Ticket: strings.Repeat("ab", 16), From: "1.0.31", To: "1.0.32", Outcome: OutcomeNoRollback}}
	s, err := SignReport(k, r)
	if err != nil {
		t.Fatal(err)
	}
	got, err := VerifyReport(k.Public(), s, "bfl-read")
	if err != nil || got.V != 1 || got.AutoUpgrade.Outcome != OutcomeNoRollback || got.Policies == nil {
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
	tampered.Body = []byte(strings.Replace(string(s.Body), "unhealthy_no_rollback", "ok", 1))
	if _, err := VerifyReport(k.Public(), tampered, "bfl-read"); code(err) != CodeBadSignature {
		t.Errorf("a relay's edit: %v", err)
	}
	// a newer box's extra field verifies
	body := []byte(`{"v":1,"box":"bfl-read","at":"2026-11-16T09:00:00Z","build":"v1.0.40","policies":[],"effective":{"hold":true,"why":"no_policy"},"newThing":1}`)
	sig := ed25519.Sign(k.Sign, append([]byte(domainReport), body...))
	if _, err := VerifyReport(k.Public(), SignedReport{Body: body, Sig: base64.StdEncoding.EncodeToString(sig)}, "bfl-read"); err != nil {
		t.Errorf("an unknown field: %v", err)
	}
	// a report names a box list without the list
	ref := boxesScope("bfl-read", "bfl-write").Ref()
	if b, _ := json.Marshal(ReportPolicy{Scope: ref, Seq: 7}); !strings.Contains(string(b), `"scope":{"type":"boxes"}`) {
		t.Fatalf("%s", b)
	}
}

func TestScopeShapes(t *testing.T) {
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
	for sc, want := range map[string]bool{
		`{"type":"all"}`:                           true,
		`{"type":"boxes","boxes":["bfl-read"]}`:    true,
		`{"type":"boxes","boxes":["a-1","b-1"]}`:   true,
		`{"type":"boxes","boxes":["b-1","a-1"]}`:   false,
		`{"type":"boxes","boxes":["ab"]}`:          false, // a box id is 3 characters at least
		`{"type":"boxes"}`:                         false,
		`{"type":"all","boxes":["bfl-read"]}`:      false,
		`{"type":"ring","boxes":["bfl-read"]}`:     false,
		`{"type":"tenant","boxes":["bfl-read"]}`:   false,
		`{"type":"boxes","boxes":["bfl-read",""]}`: false,
	} {
		var s PolicyScope
		_ = json.Unmarshal([]byte(sc), &s)
		if s.valid() != want {
			t.Errorf("%s: valid %v", sc, s.valid())
		}
	}
}
