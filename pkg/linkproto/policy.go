package linkproto

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Managed auto-update (Digitrade plan 088): a box upgrades itself, in its own maintenance window, to a release
// Digitrade approved once for a group of boxes. The approval is an UpgradePolicy signed by a team key (the key that
// signs commands) — digi-mcp signs it, the relay only stores and delivers it, so a relay can withhold a policy but can
// neither invent nor alter one. The box still downloads and checks the release itself (plan 086 D2).
//
// # The two streams
//
//	relay → box   StreamPolicy ('P'): one frame (at most MaxPolicyFrame), a PolicyDelivery — the signed policies
//	              that name the box (at most MaxDeliveredPolicies: every box's, and the newest box list naming it) and,
//	              for information only, the tenant and ring the relay's registry gives it. The relay opens one on every
//	              new connection of a box (even with no policy: an empty list) and whenever a policy that names the box
//	              changes. The box answers nothing on it.
//	box → relay   StreamReport ('R'): one frame, a SignedReport — the box's view, signed by its own key: what it
//	              accepted per scope, what was refused and why, the effective policy, the last auto-upgrade. The box
//	              opens one after each delivery, when an auto-upgrade starts and when its outcome is known.
//
// A relay that opens policy streams must accept report streams. The box opens one only on a connection over which a
// policy stream arrived, so a relay that predates plan 088 (it closes a connection whose box opens anything but its
// control stream) never sees one.
//
// # What a box accepts (plan 088 §2b, D8–D13)
//
// A policy verifies with a compiled-in team key at or above the box's epoch floor (Keyring.VerifySignedPolicy), in
// its canonical encoding (the bytes SignPolicy makes), names this box — every box, or an explicit signed list holding
// its id (Applies; D9: the tenant and ring a relay states decide nothing) — is not expired (ActiveAt), and carries a
// seq above the one the box keeps for that scope's key (an equal seq is the same policy delivered again only when its
// bytes are identical). What decides is Decide: the most specific policy the box holds (a box list over every box),
// an expired one holding for its scope; a hold issued at or after a moving policy wins at any scope (D10).
const (
	StreamPolicy byte = 'P' // relay → box: one PolicyDelivery frame
	StreamReport byte = 'R' // box → relay: one SignedReport frame
)

// Policy constants (plan 088 §3, §2b).
const (
	PolicyKindUpgrade = "upgrade-policy"

	// ScopeAll names every box; ScopeBoxes an explicit list of box ids (D9: digi-mcp expands a tenant or a ring into
	// box ids when it signs). There is no other scope: a relay's registry (tenant, ring) never decides what applies.
	ScopeAll   = "all"
	ScopeBoxes = "boxes"

	// MaxScopeBoxes bounds a box list.
	MaxScopeBoxes = 256

	// MaxRollbacks bounds a policy's signed rollback digests (D8).
	MaxRollbacks = 16

	// MaxSeq is the highest seq a policy may carry (D13, review m1): a stolen key cannot push a scope out of reach of
	// every later policy, and every party (box, relay, digi-mcp) can hold it in 32 bits.
	MaxSeq = 1<<31 - 1

	// MinHealthMinutes and MaxHealthMinutes bound how long the upgrade runner waits for the box to be healthy after
	// the setup before it rolls back.
	MinHealthMinutes = 5
	MaxHealthMinutes = 120

	// WindowSlackMinutes: a moving policy's window is at least its healthMinutes plus this long (review m17), so a
	// rollback's restart does not land long after the window.
	WindowSlackMinutes = 30

	// MaxPolicyLifetime bounds expiresAt − issuedAt (D13, review m16: what digi-mcp signs, enforced by every
	// verifier).
	MaxPolicyLifetime = 90 * 24 * time.Hour

	// MaxDeliveredPolicies is the most a delivery carries.
	MaxDeliveredPolicies = 4

	// MaxPolicyFrame bounds a policy stream's one frame: MaxDeliveredPolicies policies of a full box list each,
	// base64-encoded.
	MaxPolicyFrame = 256 << 10

	maxPolicyBytes = 24 << 10
	maxPolicyBy    = 128
)

// Refusal codes of plan 088 (in addition to refusal.go's).
const (
	CodeWrongScope = "wrong_scope"   // the policy names neither every box nor this box's id
	CodeStaleSeq   = "seq_not_newer" // the box holds this scope at this seq or a higher one
)

const (
	domainPolicy = "digi-link-policy-v1\x00"
	domainReport = "digi-link-report-v1\x00"
)

// PolicyScope is what a policy targets: every box ({"type":"all"}) or an explicit list of box ids ({"type":"boxes",
// "boxes":[…]}, 1 to MaxScopeBoxes valid ids in ascending order, each once). Reports and records name a box-list
// scope without its list (Ref): {"type":"boxes"} with the seq says which one.
type PolicyScope struct {
	Type  string   `json:"type"`
	Boxes []string `json:"boxes,omitempty"`
}

// Key is the scope's seq key, the box's key for the seq floor it keeps: "all", or "boxes" for every box-list policy.
// A box keeps one floor per key, so box-list policies are numbered from one counter: a signer gives each the highest
// seq it signed for any box list, plus one (D13), and a box takes, of the box lists naming it, only one newer than the
// one it holds.
func (s PolicyScope) Key() string { return s.Type }

// String is the scope for a log line: "all", "boxes[3]" (a list of three), "boxes" (a list named by Ref).
func (s PolicyScope) String() string {
	if s.Type == ScopeBoxes && len(s.Boxes) > 0 {
		return fmt.Sprintf("%s[%d]", s.Type, len(s.Boxes))
	}
	return s.Type
}

// Ref is the scope without its box list: how reports, jobs and the box's records name it.
func (s PolicyScope) Ref() PolicyScope { return PolicyScope{Type: s.Type} }

// Rank orders scopes from the least specific (all, 0) to the most (a box list, 1); -1 for an unknown type.
func (s PolicyScope) Rank() int {
	switch s.Type {
	case ScopeAll:
		return 0
	case ScopeBoxes:
		return 1
	}
	return -1
}

// Names reports whether the scope names box: every box, or a list holding its id.
func (s PolicyScope) Names(box string) bool {
	switch s.Type {
	case ScopeAll:
		return true
	case ScopeBoxes:
		return box != "" && slices.Contains(s.Boxes, box)
	}
	return false
}

func (s PolicyScope) valid() bool {
	switch s.Type {
	case ScopeAll:
		return len(s.Boxes) == 0
	case ScopeBoxes:
		if len(s.Boxes) == 0 || len(s.Boxes) > MaxScopeBoxes {
			return false
		}
		for i, b := range s.Boxes {
			if !ValidBoxID(b) || (i > 0 && s.Boxes[i-1] >= b) { // ascending, each once: one encoding per list
				return false
			}
		}
		return true
	}
	return false
}

var nameShape = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

// ValidTenant reports whether s has the shape of a tenant (the relay's: a Hub client's name).
func ValidTenant(s string) bool { return nameShape.MatchString(s) }

// ValidRing reports whether s has the shape of a ring name (canary, early, stable…): digi-mcp's grouping at the
// relay, expanded into box ids when a policy is signed.
func ValidRing(s string) bool { return nameShape.MatchString(s) }

// PolicyTarget is the release a policy moves boxes to: its version and the SHA-256 of its setup
// (digi-erp-connector-setup-<version>.exe), which the box checks against the release's SHA256SUMS too.
type PolicyTarget struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

// RollbackPin is a signed digest a rollback may install (D8): the setup of release Version
// (digi-erp-connector-setup-<version>.exe) has SHA-256 SHA256. A box's upgrade runner rolls back only to a build it
// replaced whose setup matches such a pin — a policy's Rollbacks for an automatic upgrade, an UpgradeSpec's Rollback
// for a command — and that release's SHA256SUMS. Without one it keeps the new build (OutcomeNoRollback).
type RollbackPin struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

// PolicyWindow is when a box may upgrade, in its own local time: on Days (mon…sun), From to To ("HH:MM", To may be
// "24:00"). To before From crosses midnight: the occurrence belongs to the day it starts on.
type PolicyWindow struct {
	Days []string `json:"days"`
	From string   `json:"from"`
	To   string   `json:"to"`
}

// UpgradePolicy is one signed approval (plan 088 §3, §2b). Target nil or Hold true: boxes in scope do not upgrade.
// Rollbacks are the digests a box covered by it may roll back to (D8: digi-mcp fills them from its pins for every
// build a covered box runs), in ascending version order, each older than the target; a hold carries none.
type UpgradePolicy struct {
	V             int           `json:"v"`
	Kind          string        `json:"kind"`
	Scope         PolicyScope   `json:"scope"`
	Seq           int64         `json:"seq"`
	IssuedAt      time.Time     `json:"issuedAt"`
	ExpiresAt     time.Time     `json:"expiresAt"`
	Target        *PolicyTarget `json:"target"`
	Rollbacks     []RollbackPin `json:"rollbacks,omitempty"`
	Hold          bool          `json:"hold"`
	Window        PolicyWindow  `json:"window"`
	HealthMinutes int           `json:"healthMinutes"`
	By            string        `json:"by"` // who approved it, as digi-mcp knows them
}

// SignedPolicy is a policy as it travels and is stored: Policy is the exact bytes signed (base64 in JSON), Sig the
// team key's Ed25519 signature over them (domain-separated like every signature in this package).
type SignedPolicy struct {
	Policy []byte `json:"policy"`
	KeyID  string `json:"keyId"`
	Sig    string `json:"sig"`
}

// Holds reports whether the policy keeps boxes where they are.
func (p UpgradePolicy) Holds() bool { return p.Hold || p.Target == nil }

// HoldsAt reports whether the policy keeps boxes where they are at now: it holds, or it is not active (D10: an expired
// policy holds for its scope).
func (p UpgradePolicy) HoldsAt(now time.Time) bool { return p.Holds() || p.ActiveAt(now) != nil }

// RollbackFor is the policy's signed rollback digest for release version ("1.0.31" or "v1.0.31"), if it carries one.
func (p UpgradePolicy) RollbackFor(version string) (RollbackPin, bool) {
	want, ok := parseRelease(version)
	if !ok {
		return RollbackPin{}, false
	}
	for _, r := range p.Rollbacks {
		if v, ok := parseRelease(r.Version); ok && v == want {
			return r, true
		}
	}
	return RollbackPin{}, false
}

// ActiveAt checks the policy's times at now: expired, or issued in the future beyond the clock skew allowed.
func (p UpgradePolicy) ActiveAt(now time.Time) error {
	switch {
	case p.IssuedAt.After(now.Add(MaxClockSkew)):
		return refuse(CodeFromFuture)
	case !p.ExpiresAt.After(now):
		return refuse(CodeExpired)
	}
	return nil
}

// SignPolicy signs p with a team key (digi-mcp's side). It sets v and kind, puts both times in UTC whole seconds,
// sorts a box list and the rollbacks into their one encoding, and refuses a policy a box would refuse for its shape (a
// box listed twice, a version pinned twice, included).
func SignPolicy(priv ed25519.PrivateKey, keyID string, p UpgradePolicy) (SignedPolicy, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return SignedPolicy{}, errors.New("linkproto: not an Ed25519 private key")
	}
	p.V = Version
	if p.Kind == "" {
		p.Kind = PolicyKindUpgrade
	}
	p.IssuedAt = p.IssuedAt.UTC().Truncate(time.Second)
	p.ExpiresAt = p.ExpiresAt.UTC().Truncate(time.Second)
	if p.Scope.Boxes != nil {
		p.Scope.Boxes = slices.Clone(p.Scope.Boxes)
		slices.Sort(p.Scope.Boxes)
	}
	if p.Rollbacks != nil {
		p.Rollbacks = slices.Clone(p.Rollbacks)
		slices.SortStableFunc(p.Rollbacks, func(a, b RollbackPin) int {
			va, _ := parseRelease(a.Version)
			vb, _ := parseRelease(b.Version)
			return slices.Compare(va[:], vb[:])
		})
	}
	if err := p.check(); err != nil {
		return SignedPolicy{}, err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return SignedPolicy{}, err
	}
	sig := ed25519.Sign(priv, append([]byte(domainPolicy), b...))
	return SignedPolicy{Policy: b, KeyID: keyID, Sig: base64.StdEncoding.EncodeToString(sig)}, nil
}

// VerifySignedPolicy checks a signed policy: a trusted key at or above minEpoch, the signature over the exact bytes,
// the canonical encoding (the bytes SignPolicy makes of what they decode to: no unknown, duplicate or re-cased key,
// no whitespace), then the shape. It does not judge the times against a clock (ActiveAt) or the scope against a box
// (Applies): those are the box's.
func (kr Keyring) VerifySignedPolicy(s SignedPolicy, minEpoch int) (UpgradePolicy, TeamKey, error) {
	if len(s.Policy) == 0 || len(s.Policy) > maxPolicyBytes {
		return UpgradePolicy{}, TeamKey{}, refuse(CodeMalformed)
	}
	k, err := kr.verify(Signed{Key: s.KeyID, Body: s.Policy, Sig: s.Sig}, domainPolicy, minEpoch)
	if err != nil {
		return UpgradePolicy{}, TeamKey{}, err
	}
	var head struct {
		V int `json:"v"`
	}
	if json.Unmarshal(s.Policy, &head) != nil {
		return UpgradePolicy{}, TeamKey{}, refuse(CodeMalformed)
	}
	if head.V != Version {
		return UpgradePolicy{}, TeamKey{}, refuse(CodeVersion)
	}
	var p UpgradePolicy
	if err := strictUnmarshal(s.Policy, &p); err != nil {
		return UpgradePolicy{}, TeamKey{}, err
	}
	if again, err := json.Marshal(p); err != nil || !bytes.Equal(again, s.Policy) {
		return UpgradePolicy{}, TeamKey{}, policyRefusal(CodeMalformed, "the policy is not in its canonical encoding")
	}
	if err := p.check(); err != nil {
		return UpgradePolicy{}, TeamKey{}, err
	}
	return p, k, nil
}

// VerifyPolicy is VerifySignedPolicy with any key of keys, whatever its epoch (the relay's and digi-mcp's check; a box
// passes its epoch floor to VerifySignedPolicy).
func VerifyPolicy(s SignedPolicy, keys Keyring) (UpgradePolicy, error) {
	p, _, err := keys.VerifySignedPolicy(s, 0)
	return p, err
}

// policyRefusal is a Refusal with what was wrong: the code for a caller to switch on (errors.As), the detail for a
// person. The detail names a field and a rule, never a value.
func policyRefusal(code, detail string) error {
	return fmt.Errorf("%w: %s", &Refusal{Code: code}, detail)
}

var (
	policyVersion = regexp.MustCompile(`^(0|[1-9][0-9]{0,3})\.(0|[1-9][0-9]{0,3})\.(0|[1-9][0-9]{0,5})$`)
	clockShape    = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
	// specVersion is a release number as the upgrade command takes it (plan 086: a leading v allowed)
	specVersion = regexp.MustCompile(`^v?(\d{1,4})\.(\d{1,4})\.(\d{1,6})$`)
	anyCaseSHA  = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
)

// parseRelease reads "1.0.31" or "v1.0.31" as three numbers.
func parseRelease(s string) ([3]int, bool) {
	m := specVersion.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return [3]int{}, false
	}
	var v [3]int
	for i := range v {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return [3]int{}, false
		}
		v[i] = n
	}
	return v, true
}

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// check is the shape of a policy, the same for the signer and every verifier.
func (p UpgradePolicy) check() error {
	bad := func(detail string) error { return policyRefusal(CodeMalformed, detail) }
	switch {
	case p.V != Version:
		return refuse(CodeVersion)
	case p.Kind != PolicyKindUpgrade:
		return bad(`kind is not "` + PolicyKindUpgrade + `"`)
	case !p.Scope.valid():
		return bad(`scope: {"type":"all"}, or {"type":"boxes","boxes":[1 to 256 box ids, ascending, each once]}`)
	case p.Seq <= 0 || p.Seq > MaxSeq:
		return bad("seq is an integer from 1 to 2147483647")
	case p.IssuedAt.IsZero() || p.ExpiresAt.IsZero():
		return bad("issuedAt and expiresAt are both set")
	case !p.ExpiresAt.After(p.IssuedAt):
		return bad("expiresAt is after issuedAt")
	case p.ExpiresAt.Sub(p.IssuedAt) > MaxPolicyLifetime:
		return policyRefusal(CodeLifetime, "expiresAt is at most 90 days after issuedAt")
	case p.Hold && p.Target != nil:
		return bad("a hold names no target")
	case p.Holds() && len(p.Rollbacks) > 0:
		return bad("a hold names no rollbacks: they serve the upgrades a target starts")
	case !validBy(p.By):
		return bad("by names who approved it: 1 to 128 bytes of UTF-8, no control character")
	}
	if p.Target != nil && (!policyVersion.MatchString(p.Target.Version) || !sha256Shape.MatchString(p.Target.SHA256)) {
		return bad("target: version is a release number like 1.0.31, sha256 the setup's 64 lowercase hex digits")
	}
	if err := p.checkRollbacks(); err != nil {
		return err
	}
	moves := !p.Holds()
	if moves || !p.Window.isZero() {
		if err := p.Window.check(); err != nil {
			return err
		}
	}
	if (moves || p.HealthMinutes != 0) && (p.HealthMinutes < MinHealthMinutes || p.HealthMinutes > MaxHealthMinutes) {
		return bad("healthMinutes is 5 to 120")
	}
	if moves && p.Window.minutes() < p.HealthMinutes+WindowSlackMinutes {
		return bad("the window is at least healthMinutes + 30 minutes long: an upgrade, its health check and a rollback fit in it")
	}
	return nil
}

// checkRollbacks: at most MaxRollbacks, each a release number and a lowercase digest, in ascending version order with
// each version once, every one older than the target.
func (p UpgradePolicy) checkRollbacks() error {
	if len(p.Rollbacks) > MaxRollbacks {
		return policyRefusal(CodeMalformed, "rollbacks: at most 16")
	}
	var target [3]int
	if p.Target != nil {
		target, _ = parseRelease(p.Target.Version)
	}
	var prev [3]int
	for i, r := range p.Rollbacks {
		v, ok := parseRelease(r.Version)
		switch {
		case !ok || !policyVersion.MatchString(r.Version) || !sha256Shape.MatchString(r.SHA256):
			return policyRefusal(CodeMalformed, "rollbacks: each version a release number like 1.0.30, each sha256 64 lowercase hex digits")
		case i > 0 && slices.Compare(prev[:], v[:]) >= 0:
			return policyRefusal(CodeMalformed, "rollbacks: in ascending version order, each version once")
		case slices.Compare(v[:], target[:]) >= 0:
			return policyRefusal(CodeMalformed, "rollbacks: each one older than the target")
		}
		prev = v
	}
	return nil
}

func validBy(s string) bool {
	if s == "" || len(s) > maxPolicyBy || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return false
		}
	}
	return true
}

func (w PolicyWindow) isZero() bool { return len(w.Days) == 0 && w.From == "" && w.To == "" }

// parseClock is "HH:MM" as minutes after midnight; "24:00" (1440) only when end is set.
func parseClock(s string, end bool) (int, bool) {
	if end && s == "24:00" {
		return 24 * 60, true
	}
	if !clockShape.MatchString(s) {
		return 0, false
	}
	h, _ := strconv.Atoi(s[:2])
	m, _ := strconv.Atoi(s[3:])
	return h*60 + m, true
}

// parsed is the window's days and minutes, ok when its shape is valid.
func (w PolicyWindow) parsed() (days [7]bool, from, to int, ok bool) {
	if len(w.Days) == 0 || len(w.Days) > 7 {
		return days, 0, 0, false
	}
	for _, d := range w.Days {
		wd, known := weekdays[d]
		if !known || days[wd] {
			return days, 0, 0, false
		}
		days[wd] = true
	}
	from, ok1 := parseClock(w.From, false)
	to, ok2 := parseClock(w.To, true)
	if !ok1 || !ok2 || from == to {
		return days, 0, 0, false
	}
	return days, from, to, true
}

func (w PolicyWindow) check() error {
	if _, _, _, ok := w.parsed(); !ok {
		return policyRefusal(CodeMalformed, `window: days are 1 to 7 distinct of mon…sun; from and to are "HH:MM" (to may be "24:00") and differ`)
	}
	return nil
}

// minutes is how long one occurrence of the window lasts (0 for a window that is not valid).
func (w PolicyWindow) minutes() int {
	_, from, to, ok := w.parsed()
	switch {
	case !ok:
		return 0
	case to < from:
		return 24*60 - from + to
	}
	return to - from
}

// at is minute m of day's date in day's location (24:00 is the next day's midnight).
func at(day time.Time, m int) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), m/60, m%60, 0, 0, day.Location())
}

// Start is the start of the occurrence of the window that contains t (read in t's location: the box passes its local
// time), and whether one does.
func (w PolicyWindow) Start(t time.Time) (time.Time, bool) {
	days, from, to, ok := w.parsed()
	if !ok {
		return time.Time{}, false
	}
	for back := 0; back <= 1; back++ { // today's occurrence, or yesterday's running past midnight
		day := time.Date(t.Year(), t.Month(), t.Day()-back, 0, 0, 0, 0, t.Location())
		if !days[day.Weekday()] {
			continue
		}
		start, end := at(day, from), at(day, to)
		if to < from {
			end = at(time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, day.Location()), to)
		}
		if !t.Before(start) && t.Before(end) {
			return start, true
		}
	}
	return time.Time{}, false
}

// Contains reports whether t is inside the window.
func (w PolicyWindow) Contains(t time.Time) bool {
	_, ok := w.Start(t)
	return ok
}

// NextStart is the start of the first occurrence after t (zero for a window that is not valid).
func (w PolicyWindow) NextStart(t time.Time) time.Time {
	days, from, _, ok := w.parsed()
	if !ok {
		return time.Time{}
	}
	for i := 0; i <= 8; i++ {
		day := time.Date(t.Year(), t.Month(), t.Day()+i, 0, 0, 0, 0, t.Location())
		if s := at(day, from); days[day.Weekday()] && s.After(t) {
			return s
		}
	}
	return time.Time{}
}

// Applies reports whether p names box: every box, or a list holding its id. Nothing a relay states — a tenant, a
// ring — has a say (D9).
func Applies(p UpgradePolicy, box string) bool { return p.Scope.Names(box) }

// MostSpecific is the policy that decides among ps: the most specific scope (a box list, then every box), then the
// highest seq. False when ps is empty. Callers pass policies that apply; Decide does the rest of the rules.
func MostSpecific(ps []UpgradePolicy) (UpgradePolicy, bool) {
	var best UpgradePolicy
	found := false
	for _, p := range ps {
		if r := p.Scope.Rank(); r >= 0 && (!found || r > best.Scope.Rank() || (r == best.Scope.Rank() && p.Seq > best.Seq)) {
			best, found = p, true
		}
	}
	return best, found
}

// Decision is what decides for one box at one time (Decide).
type Decision struct {
	// Policy decides: the most specific policy held, or the hold that brakes it. Zero when Found is false.
	Policy UpgradePolicy
	Found  bool
	// Hold: the box does not upgrade. Why says why (WhyNoPolicy, WhyPolicyHolds, WhyPolicyExpired) and is "" when the
	// policy moves the box.
	Hold bool
	Why  string
	// Braked is the moving policy a later hold overrides (D10), when that is why the box holds.
	Braked *UpgradePolicy
}

// Decide is what the policies ps decide for box at now (plan 088 D3, D9, D10):
//  1. Of ps, those naming box (Applies), the newest (highest seq) per scope key: what the box holds.
//  2. The most specific of them decides — a box list over every box — expired or not: an expired policy holds for its
//     scope and never falls through to a less specific one (review m9: an emergency hold does not lapse into a
//     rollout).
//  3. A deciding policy that moves loses to any hold among them issued at or after it, at any scope (review M2: the
//     newest hold wins; an all hold is the emergency brake). Decide then names that hold (the newest such).
//
// None naming box: the box holds (WhyNoPolicy).
func Decide(ps []UpgradePolicy, box string, now time.Time) Decision {
	byKey := map[string]UpgradePolicy{}
	for _, p := range ps {
		if p.Scope.Rank() < 0 || !Applies(p, box) {
			continue
		}
		if cur, ok := byKey[p.Scope.Key()]; !ok || p.Seq > cur.Seq {
			byKey[p.Scope.Key()] = p
		}
	}
	held := make([]UpgradePolicy, 0, len(byKey))
	for _, key := range []string{ScopeAll, ScopeBoxes} {
		if p, ok := byKey[key]; ok {
			held = append(held, p)
		}
	}
	deciding, ok := MostSpecific(held)
	switch {
	case !ok:
		return Decision{Hold: true, Why: WhyNoPolicy}
	case deciding.ActiveAt(now) != nil:
		return Decision{Policy: deciding, Found: true, Hold: true, Why: WhyPolicyExpired}
	case deciding.Holds():
		return Decision{Policy: deciding, Found: true, Hold: true, Why: WhyPolicyHolds}
	}
	var brake *UpgradePolicy
	for i := range held {
		h := held[i]
		if h.Holds() && !h.IssuedAt.Before(deciding.IssuedAt) && (brake == nil || h.IssuedAt.After(brake.IssuedAt)) {
			brake = &h
		}
	}
	if brake != nil {
		moving := deciding
		return Decision{Policy: *brake, Found: true, Hold: true, Why: WhyPolicyHolds, Braked: &moving}
	}
	return Decision{Policy: deciding, Found: true}
}

// Effective is Decide's policy: the one that decides for box at now among ps, and false when none names it (the box
// holds). The box holds also whenever the policy returned HoldsAt(now).
func Effective(ps []UpgradePolicy, box string, now time.Time) (UpgradePolicy, bool) {
	d := Decide(ps, box, now)
	return d.Policy, d.Found
}

// UpgradeSpec is the `upgrade` command's stdin (plan 086 C8; plan 088): signed with the envelope, whose stdinSha256
// binds these exact bytes. Rollback (D8) is the signed digest of the build the box runs now, the only one its runner
// may install again if the upgrade ends unhealthy; without it there is no automatic rollback. A box before v1.0.31
// refuses a spec naming healthMinutes or rollback (an unknown field): send them only to a box whose build has
// auto-update.
type UpgradeSpec struct {
	Version       string       `json:"version"`
	SHA256        string       `json:"sha256"`
	AssumeIdle    bool         `json:"assumeIdle,omitempty"`
	HealthMinutes int          `json:"healthMinutes,omitempty"`
	Rollback      *RollbackPin `json:"rollback,omitempty"`
}

// Check is the spec's shape as a box takes it: version a release number ("1.0.31", a leading v allowed), sha256 64
// hex digits, healthMinutes 0 (the default, 15) or 5 to 120, and a rollback, when there is one, a release number with
// 64 hex digits older than version. Whether the rollback names the running build is the box's to judge.
func (s UpgradeSpec) Check() error {
	bad := func(detail string) error { return policyRefusal(CodeMalformed, detail) }
	to, ok := parseRelease(s.Version)
	switch {
	case !ok:
		return bad("version is a release number, like 1.0.31")
	case !anyCaseSHA.MatchString(strings.TrimSpace(s.SHA256)):
		return bad("sha256 is the setup's SHA-256, 64 hex digits")
	case s.HealthMinutes != 0 && (s.HealthMinutes < MinHealthMinutes || s.HealthMinutes > MaxHealthMinutes):
		return bad("healthMinutes is 5 to 120, or left out (15)")
	}
	if r := s.Rollback; r != nil {
		v, ok := parseRelease(r.Version)
		switch {
		case !ok || !anyCaseSHA.MatchString(strings.TrimSpace(r.SHA256)):
			return bad("rollback: version is a release number, sha256 its setup's 64 hex digits")
		case slices.Compare(v[:], to[:]) >= 0:
			return bad("rollback: a release older than version (the build the box runs now)")
		}
	}
	return nil
}

// PolicyDelivery is a policy stream's one frame (relay → box).
type PolicyDelivery struct {
	V   int    `json:"v"`
	Box string `json:"box"` // the box the relay delivers to; the box refuses another id
	// Tenant and Ring are the box's tenant and ring in the relay's registry: information for the box's report, never
	// a say in what applies (D9).
	Tenant   string         `json:"tenant,omitempty"`
	Ring     string         `json:"ring,omitempty"`
	Policies []SignedPolicy `json:"policies"` // the policies that name the box, at most MaxDeliveredPolicies
}

// DecodePolicyDelivery reads a delivery frame. Unknown fields are ignored — a newer relay may add some, and an older
// box must not stop taking policies for it; what a box acts on is inside the signed policies.
func DecodePolicyDelivery(b []byte) (PolicyDelivery, error) {
	var d PolicyDelivery
	if err := json.Unmarshal(b, &d); err != nil {
		return PolicyDelivery{}, refuse(CodeMalformed)
	}
	switch {
	case d.V != Version:
		return PolicyDelivery{}, refuse(CodeVersion)
	case !ValidBoxID(d.Box), d.Tenant != "" && !ValidTenant(d.Tenant), d.Ring != "" && !ValidRing(d.Ring),
		len(d.Policies) > MaxDeliveredPolicies:
		return PolicyDelivery{}, refuse(CodeMalformed)
	}
	return d, nil
}

// Auto-upgrade outcomes, in reports, results and link-status.
const (
	OutcomePending    = "pending"     // the runner is at work
	OutcomeOK         = "ok"          // the target is installed and the box was healthy
	OutcomeFailed     = "failed"      // the setup or the services failed; the box was healthy again without a rollback, or nothing changed
	OutcomeUnhealthy  = "unhealthy"   // not healthy after the setup, and the rollback did not end healthy (or its checks refused it)
	OutcomeRolledBack = "rolled_back" // not healthy after the setup; the build it replaced was installed again
	OutcomeLost       = "lost"        // the runner ended without a result
	OutcomeRefused    = "refused"     // the attempt was refused before a ticket (Error says why)
	// Plan 088 §2b: the runner left the new build in place, and a person decides.
	OutcomeNoRollback       = "unhealthy_no_rollback"       // D8: not healthy, and no signed digest names the build replaced
	OutcomeBelowFloor       = "rollback_below_floor"        // D11: not healthy, and the build replaced is below DataDir\format-floor
	OutcomeLinkInconclusive = "unhealthy_link_inconclusive" // D12: only the new link failed, and the relay's /healthz did not answer either
	// OutcomeDeferred (review m6): orders were queued right before the setup, which was not run; nothing changed.
	OutcomeDeferred = "deferred"
)

// Why the effective view is what it is (ReportEffective.Why).
const (
	WhyNoPolicy        = "no_policy"        // no policy names this box: hold
	WhyPolicyHolds     = "policy_hold"      // the deciding policy holds (hold, or no target), or a later hold brakes it (D10)
	WhyPolicyExpired   = "policy_expired"   // the deciding policy expired: it holds for its scope (D10, review m9)
	WhyPolicyRevoked   = "policy_revoked"   // the deciding policy's key is below the box's epoch floor: it holds for its scope
	WhyAutoUpgradeOff  = "auto_upgrade_off" // link.yaml autoUpgrade: false
	WhyAdminOff        = "admin_off"        // link.yaml admin: false
	WhyStateUnreadable = "state_unreadable" // link.yaml names a key the link does not know, or the policy store does not read
	WhyUpToDate        = "up_to_date"       // the target is not newer than the running build
	WhyScheduled       = "scheduled"        // the target is newer: the box upgrades in its window, with its order queue idle
	WhyUpgrading       = "upgrading"        // an upgrade to the target is running
	WhyStopped         = "stopped"          // the box stopped trying this target under this policy (rolled back, unhealthy, or failed three times): a new policy re-arms it
)

// Report is the box's view (box → relay on a report stream, signed: SignReport). Its scopes are named without their
// box lists (PolicyScope.Ref).
type Report struct {
	V      int       `json:"v"`
	Box    string    `json:"box"`
	At     time.Time `json:"at"`
	Build  string    `json:"build"`            // the link's build
	Tenant string    `json:"tenant,omitempty"` // as the relay last stated them: information only (D9)
	Ring   string    `json:"ring,omitempty"`
	// Policies is what the box holds, one per scope key.
	Policies    []ReportPolicy  `json:"policies"`
	Refused     []ReportRefusal `json:"refused,omitempty"` // the last delivery's refusals
	Effective   ReportEffective `json:"effective"`
	AutoUpgrade *ReportAuto     `json:"autoUpgrade,omitempty"` // the last auto-upgrade, nil before the first
}

// ReportPolicy is one accepted policy.
type ReportPolicy struct {
	Scope     PolicyScope `json:"scope"`
	Seq       int64       `json:"seq"`
	Target    string      `json:"target,omitempty"` // its version; "" when it holds
	Hold      bool        `json:"hold,omitempty"`
	ExpiresAt time.Time   `json:"expiresAt"`
	Applies   bool        `json:"applies"` // names this box's id
}

// ReportRefusal is one refused policy of a delivery: its scope and seq when its signature verified, and the code.
type ReportRefusal struct {
	Scope *PolicyScope `json:"scope,omitempty"`
	Seq   int64        `json:"seq,omitempty"`
	Code  string       `json:"code"`
}

// ReportEffective is what decides now: the policy (nil when none is valid), its target, and whether the box holds.
type ReportEffective struct {
	Scope     *PolicyScope  `json:"scope,omitempty"`
	Seq       int64         `json:"seq,omitempty"`
	Version   string        `json:"version,omitempty"` // the target it names
	Hold      bool          `json:"hold"`
	Why       string        `json:"why"` // one of the Why* codes
	Window    *PolicyWindow `json:"window,omitempty"`
	ExpiresAt time.Time     `json:"expiresAt,omitzero"`
}

// ReportAuto is the last auto-upgrade attempt.
type ReportAuto struct {
	Ticket   string           `json:"ticket,omitempty"` // "" when refused before a ticket
	At       time.Time        `json:"at"`
	Scope    *PolicyScope     `json:"scope,omitempty"` // the policy it followed
	Seq      int64            `json:"seq,omitempty"`
	From     string           `json:"from"`
	To       string           `json:"to"`
	Outcome  string           `json:"outcome"` // one of the Outcome* codes
	Health   *UpgradeHealth   `json:"health,omitempty"`
	Rollback *UpgradeRollback `json:"rollback,omitempty"`
	Error    string           `json:"error,omitempty"`
}

// UpgradeHealth is the upgrade runner's health check after a setup (plan 088 D5): the box is healthy when both
// services run as before, the daemon proves itself (it logs in and answers /api/ping with the installed build), and the
// link of the installed build is connected to the relay. It is in the ticket's result file and in reports.
type UpgradeHealth struct {
	OK      bool          `json:"ok"`
	Minutes int           `json:"minutes"` // the budget
	Seconds int64         `json:"seconds"` // until healthy, or the whole budget
	Checks  []HealthCheck `json:"checks"`  // the last round
}

// HealthCheck is one part of a health check: services, daemon, link.
type HealthCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// UpgradeRollback is the runner's one rollback after an unhealthy upgrade: the build it replaced, installed again
// from its official release.
type UpgradeRollback struct {
	To               string            `json:"to"`
	SHA256           string            `json:"sha256,omitempty"` // the setup's, as checked
	ExitCode         int               `json:"exitCode"`         // the setup's; -1 when it did not run
	OK               bool              `json:"ok"`               // installed, services as before, healthy
	InstalledVersion string            `json:"installedVersion,omitempty"`
	Services         map[string]string `json:"services,omitempty"`
	Health           *UpgradeHealth    `json:"health,omitempty"`
	StartedAt        time.Time         `json:"startedAt"`
	FinishedAt       time.Time         `json:"finishedAt"`
	Error            string            `json:"error,omitempty"`
}

// SignedReport is a Report signed by the box's own key: the relay stores it as it came and cannot forge one, and
// digi-mcp checks it against the key it pinned at pairing (VerifyReport).
type SignedReport struct {
	Body json.RawMessage `json:"body"`
	Sig  string          `json:"sig"`
}

// SignReport signs r with the box's key.
func SignReport(k BoxKeys, r Report) (SignedReport, error) {
	r.V = Version
	if r.Policies == nil {
		r.Policies = []ReportPolicy{}
	}
	body, err := json.Marshal(r)
	if err != nil {
		return SignedReport{}, err
	}
	sig := ed25519.Sign(k.Sign, append([]byte(domainReport), body...))
	return SignedReport{Body: body, Sig: base64.StdEncoding.EncodeToString(sig)}, nil
}

// VerifyReport checks a report against the box's pinned key and its id. Unknown fields are ignored (a newer box may
// report more than this build knows).
func VerifyReport(pub BoxPublic, s SignedReport, box string) (Report, error) {
	sig, err := base64.StdEncoding.DecodeString(s.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize || !ed25519.Verify(pub.Sign, append([]byte(domainReport), s.Body...), sig) {
		return Report{}, refuse(CodeBadSignature)
	}
	var r Report
	if err := json.Unmarshal(s.Body, &r); err != nil {
		return Report{}, refuse(CodeMalformed)
	}
	switch {
	case r.V != Version:
		return Report{}, refuse(CodeVersion)
	case r.Box != box:
		return Report{}, refuse(CodeWrongBox)
	}
	return r, nil
}
