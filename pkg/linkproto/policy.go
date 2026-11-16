package linkproto

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
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
//	relay → box   StreamPolicy ('P'): one frame, a PolicyDelivery — every signed policy that applies to the box (at
//	              most MaxDeliveredPolicies: all, its ring, its tenant, its id) and the tenant and ring the relay's
//	              registry gives it. The relay opens one on every new connection of a box (even with no policy: an
//	              empty list) and whenever a policy that applies to the box changes. The box answers nothing on it.
//	box → relay   StreamReport ('R'): one frame, a SignedReport — the box's view, signed by its own key: what it
//	              accepted per scope, what was refused and why, the effective policy, the last auto-upgrade. The box
//	              opens one after each delivery, when an auto-upgrade starts and when its outcome is known.
//
// A relay that opens policy streams must accept report streams. The box opens one only on a connection over which a
// policy stream arrived, so a relay that predates plan 088 (it closes a connection whose box opens anything but its
// control stream) never sees one.
//
// # What a box accepts
//
// A policy verifies with a compiled-in team key at or above the box's epoch floor (Keyring.VerifySignedPolicy), in
// its canonical encoding (the bytes SignPolicy makes), names this box's id, tenant or ring as the relay stated them
// (Applies), is not expired (ActiveAt), and carries a seq above the one the box keeps for that scope (an equal seq is
// the same policy delivered again only when its bytes are identical). The most specific valid policy decides
// (MostSpecific: box, then tenant, then ring, then all); none, an expired one, hold or no target: the box holds.
const (
	StreamPolicy byte = 'P' // relay → box: one PolicyDelivery frame
	StreamReport byte = 'R' // box → relay: one SignedReport frame
)

// Policy constants (plan 088 §3).
const (
	PolicyKindUpgrade = "upgrade-policy"

	ScopeAll    = "all"
	ScopeRing   = "ring"
	ScopeTenant = "tenant"
	ScopeBox    = "box"

	// MinHealthMinutes and MaxHealthMinutes bound how long the upgrade runner waits for the box to be healthy after
	// the setup before it rolls back.
	MinHealthMinutes = 5
	MaxHealthMinutes = 120

	// MaxPolicyLifetime bounds expiresAt − issuedAt.
	MaxPolicyLifetime = 366 * 24 * time.Hour

	// MaxDeliveredPolicies is the most a delivery carries: one per scope type.
	MaxDeliveredPolicies = 4

	maxPolicyBytes = 4 << 10
	maxPolicyBy    = 128
)

// Refusal codes of plan 088 (in addition to refusal.go's).
const (
	CodeWrongScope = "wrong_scope"   // the policy names neither this box nor its tenant or ring
	CodeStaleSeq   = "seq_not_newer" // the box holds this scope at this seq or a higher one
)

const (
	domainPolicy = "digi-link-policy-v1\x00"
	domainReport = "digi-link-report-v1\x00"
)

// PolicyScope is what a policy targets: every box (all, Value ""), a ring, a tenant or one box.
type PolicyScope struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// Key is the scope as one string, the box's key for the seq it holds: "all", "ring:canary", "tenant:bflstore",
// "box:bfl-read".
func (s PolicyScope) Key() string {
	if s.Type == ScopeAll {
		return ScopeAll
	}
	return s.Type + ":" + s.Value
}

func (s PolicyScope) String() string { return s.Key() }

// Rank orders scopes from the least specific (all, 0) to the most (box, 3); -1 for an unknown type.
func (s PolicyScope) Rank() int {
	switch s.Type {
	case ScopeAll:
		return 0
	case ScopeRing:
		return 1
	case ScopeTenant:
		return 2
	case ScopeBox:
		return 3
	}
	return -1
}

func (s PolicyScope) valid() bool {
	switch s.Type {
	case ScopeAll:
		return s.Value == ""
	case ScopeRing:
		return ValidRing(s.Value)
	case ScopeTenant:
		return ValidTenant(s.Value)
	case ScopeBox:
		return ValidBoxID(s.Value)
	}
	return false
}

var nameShape = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

// ValidTenant reports whether s has the shape of a tenant (the relay's: a Hub client's name).
func ValidTenant(s string) bool { return nameShape.MatchString(s) }

// ValidRing reports whether s has the shape of a ring name (canary, early, stable…).
func ValidRing(s string) bool { return nameShape.MatchString(s) }

// PolicyTarget is the release a policy moves boxes to: its version and the SHA-256 of its setup
// (digi-erp-connector-setup-<version>.exe), which the box checks against the release's SHA256SUMS too.
type PolicyTarget struct {
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

// UpgradePolicy is one signed approval (plan 088 §3). Target nil or Hold true: boxes in scope do not upgrade.
type UpgradePolicy struct {
	V             int           `json:"v"`
	Kind          string        `json:"kind"`
	Scope         PolicyScope   `json:"scope"`
	Seq           int64         `json:"seq"`
	IssuedAt      time.Time     `json:"issuedAt"`
	ExpiresAt     time.Time     `json:"expiresAt"`
	Target        *PolicyTarget `json:"target"`
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

// SignPolicy signs p with a team key (digi-mcp's side). It sets v and kind, puts both times in UTC whole seconds and
// refuses a policy a box would refuse for its shape.
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
)

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
		return bad("scope: type is all, ring, tenant or box; value is empty for all, else a name of its type's shape")
	case p.Seq <= 0:
		return bad("seq is a positive integer")
	case p.IssuedAt.IsZero() || p.ExpiresAt.IsZero():
		return bad("issuedAt and expiresAt are both set")
	case !p.ExpiresAt.After(p.IssuedAt):
		return bad("expiresAt is after issuedAt")
	case p.ExpiresAt.Sub(p.IssuedAt) > MaxPolicyLifetime:
		return policyRefusal(CodeLifetime, "expiresAt is at most 366 days after issuedAt")
	case p.Hold && p.Target != nil:
		return bad("a hold names no target")
	case !validBy(p.By):
		return bad("by names who approved it: 1 to 128 bytes of UTF-8, no control character")
	}
	if p.Target != nil && (!policyVersion.MatchString(p.Target.Version) || !sha256Shape.MatchString(p.Target.SHA256)) {
		return bad("target: version is a release number like 1.0.31, sha256 the setup's 64 lowercase hex digits")
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

// Applies reports whether p's scope names this box: every box, its ring, its tenant or its id. tenant and ring are
// what the relay stated ("" for none: then no ring or tenant policy applies).
func Applies(p UpgradePolicy, box, tenant, ring string) bool {
	switch p.Scope.Type {
	case ScopeAll:
		return true
	case ScopeRing:
		return ring != "" && p.Scope.Value == ring
	case ScopeTenant:
		return tenant != "" && p.Scope.Value == tenant
	case ScopeBox:
		return box != "" && p.Scope.Value == box
	}
	return false
}

// MostSpecific is the policy that decides among ps: the most specific scope (box, tenant, ring, all), then the highest
// seq. False when ps is empty. Callers pass only valid policies that apply (Effective does both).
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

// Effective is the policy that decides for a box at now among ps: the most specific of those that apply to it and
// are active. False: none does, and the box holds.
func Effective(ps []UpgradePolicy, box, tenant, ring string, now time.Time) (UpgradePolicy, bool) {
	var live []UpgradePolicy
	for _, p := range ps {
		if Applies(p, box, tenant, ring) && p.ActiveAt(now) == nil {
			live = append(live, p)
		}
	}
	return MostSpecific(live)
}

// PolicyDelivery is a policy stream's one frame (relay → box).
type PolicyDelivery struct {
	V        int            `json:"v"`
	Box      string         `json:"box"`              // the box the relay delivers to; the box refuses another id
	Tenant   string         `json:"tenant,omitempty"` // the box's tenant in the relay's registry
	Ring     string         `json:"ring,omitempty"`   // the box's ring there, "" for none
	Policies []SignedPolicy `json:"policies"`         // every policy that applies, at most MaxDeliveredPolicies
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

// Auto-upgrade outcomes, in reports and link-status.
const (
	OutcomePending    = "pending"     // the runner is at work
	OutcomeOK         = "ok"          // the target is installed and the box was healthy
	OutcomeFailed     = "failed"      // the setup or the services failed; the box was healthy again without a rollback, or nothing changed
	OutcomeUnhealthy  = "unhealthy"   // not healthy after the setup, and the rollback did not run or did not end healthy
	OutcomeRolledBack = "rolled_back" // not healthy after the setup; the build it replaced was installed again
	OutcomeLost       = "lost"        // the runner ended without a result
	OutcomeRefused    = "refused"     // the attempt was refused before a ticket (Error says why)
)

// Why the effective view is what it is (ReportEffective.Why).
const (
	WhyNoPolicy        = "no_policy"        // no valid policy applies: hold
	WhyPolicyHolds     = "policy_hold"      // the deciding policy holds (hold, or no target)
	WhyAutoUpgradeOff  = "auto_upgrade_off" // link.yaml autoUpgrade: false
	WhyAdminOff        = "admin_off"        // link.yaml admin: false
	WhyStateUnreadable = "state_unreadable" // link.yaml names a key the link does not know, or the policy store does not read
	WhyUpToDate        = "up_to_date"       // the target is not newer than the running build
	WhyScheduled       = "scheduled"        // the target is newer: the box upgrades in its window, with its order queue idle
	WhyUpgrading       = "upgrading"        // an upgrade to the target is running
	WhyStopped         = "stopped"          // the box stopped trying this target under this policy (rolled back, or failed three times): a new policy re-arms it
)

// Report is the box's view (box → relay on a report stream, signed: SignReport).
type Report struct {
	V           int             `json:"v"`
	Box         string          `json:"box"`
	At          time.Time       `json:"at"`
	Build       string          `json:"build"`            // the link's build
	Tenant      string          `json:"tenant,omitempty"` // as the relay last stated them
	Ring        string          `json:"ring,omitempty"`
	Policies    []ReportPolicy  `json:"policies"`          // what the box holds, one per scope
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
	Applies   bool        `json:"applies"` // names this box with the tenant and ring stated now
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
