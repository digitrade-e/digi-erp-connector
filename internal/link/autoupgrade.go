package link

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"slices"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// The auto-update scheduler (plan 088 §3, D2, D4). Every AutoEvery (10 min ± 1) the link asks what the policies it
// holds decide (decideAuto). An upgrade is due when a valid policy names a release newer than this build, the box's
// local time is inside that policy's window, no upgrade is running and link.yaml lets it (autoUpgrade and admin not
// false, no unknown key). Then it runs the very path a signed `upgrade` command runs (startUpgrade): the official
// release only, newer, its setup's SHA-256 equal to the policy's and to the release's SHA256SUMS line, the order queue
// idle — every check before the runner starts. The scheduler decides only when.
//
// It tries a target at most once per window occurrence: orders in the queue, another command or a provisioning run
// are "not now" (tried again at the next check, same window); any other refusal waits for the next window. A ticket
// that ended unhealthy or rolled back stops that target under that policy, and so do three failed tickets: a newer
// policy (a higher seq, even for the same target) re-arms it.

const (
	maxAutoTickets   = 3
	maxAutoAttempts  = 20
	autoPendingEvery = time.Minute // checks while an automatic ticket runs, so its outcome is reported soon
)

// autoWaitCodes are refusals that mean "not now": the next check tries again, in the same window.
var autoWaitCodes = map[string]bool{"orders_pending": true, "orders_unknown": true, busyCode: true, CodeUpgradeRunning: true}

// PolicyRef names the policy an automatic upgrade follows (in its job and result).
type PolicyRef struct {
	Scope linkproto.PolicyScope `json:"scope"`
	Seq   int64                 `json:"seq"`
}

// autoAttempt is one automatic attempt: a ticket started, or refused before one.
type autoAttempt struct {
	At          time.Time             `json:"at"`
	Scope       linkproto.PolicyScope `json:"scope"`
	Seq         int64                 `json:"seq"`
	From        string                `json:"from"`
	To          string                `json:"to"`
	WindowStart time.Time             `json:"windowStart"`
	Ticket      string                `json:"ticket,omitempty"`
	Outcome     string                `json:"outcome"` // linkproto.Outcome*
	Refusal     *UpgradeRefusal       `json:"refusal,omitempty"`
}

// AutoWait is the last check that found an upgrade due but not now (Code: orders_pending, orders_unknown, busy,
// upgrade_running).
type AutoWait struct {
	At      time.Time `json:"at"`
	Code    string    `json:"code"`
	Message string    `json:"message"`
}

// autoFile is DataDir\link\autoupgrade.json: the scheduler's memory, and its last view for provision's state.
type autoFile struct {
	V        int                `json:"v"`
	Attempts []autoAttempt      `json:"attempts"`
	Waiting  *AutoWait          `json:"waiting,omitempty"`
	View     *AutoUpgradeStatus `json:"view,omitempty"`
	// Tally is what stopped or failed under each policy and target (review m10): kept apart from the newest
	// maxAutoAttempts attempts, so a rolled-back target, or two failed tickets, never age out of the box's memory.
	Tally []autoTally `json:"tally,omitempty"`
}

// autoTally is one (policy, target)'s record: failed tickets, and whether a ticket stopped it.
type autoTally struct {
	Scope   linkproto.PolicyScope `json:"scope"`
	Seq     int64                 `json:"seq"`
	To      string                `json:"to"`
	Failed  int                   `json:"failed,omitempty"`
	Stopped string                `json:"stopped,omitempty"` // the outcome that stopped it
	At      time.Time             `json:"at"`
}

// maxAutoTally bounds the tally: one entry per signed (policy, target) a ticket ended under — a relay cannot add any.
const maxAutoTally = 64

// stops reports whether an outcome stops its target under its policy.
func stops(outcome string) bool {
	switch outcome {
	case linkproto.OutcomeUnhealthy, linkproto.OutcomeRolledBack, linkproto.OutcomeNoRollback, linkproto.OutcomeBelowFloor,
		linkproto.OutcomeLinkInconclusive:
		return true
	}
	return false
}

// tallyOutcome records a ticket's final outcome under its policy and target.
func (af *autoFile) tallyOutcome(a autoAttempt, now time.Time) {
	failed := a.Outcome == linkproto.OutcomeFailed || a.Outcome == linkproto.OutcomeLost
	if !failed && !stops(a.Outcome) {
		return
	}
	at := slices.IndexFunc(af.Tally, func(t autoTally) bool { return t.Scope.Key() == a.Scope.Key() && t.Seq == a.Seq && t.To == a.To })
	if at < 0 {
		af.Tally = append(af.Tally, autoTally{Scope: a.Scope.Ref(), Seq: a.Seq, To: a.To})
		at = len(af.Tally) - 1
	}
	t := &af.Tally[at]
	t.At = now.UTC()
	if failed {
		t.Failed++
	} else if t.Stopped == "" {
		t.Stopped = a.Outcome
	}
	if len(af.Tally) > maxAutoTally {
		af.Tally = af.Tally[len(af.Tally)-maxAutoTally:]
	}
}

// AutoUpgradeStatus is the auto-update part of link-status and of provision's state (plan 088).
type AutoUpgradeStatus struct {
	Enabled   bool                      `json:"enabled"`          // link.yaml autoUpgrade (default true)
	Tenant    string                    `json:"tenant,omitempty"` // as the relay last stated them: information only (D9)
	Ring      string                    `json:"ring,omitempty"`
	StatedAt  time.Time                 `json:"statedAt,omitzero"`
	Policies  []PolicyStatus            `json:"policies"`
	Refused   []linkproto.ReportRefusal `json:"refused,omitempty"` // the last delivery's refusals
	Effective linkproto.ReportEffective `json:"effective"`
	// Braked is the moving policy a later hold overrides (plan 088 D10), when that is why the box holds.
	Braked     *PolicyRef            `json:"braked,omitempty"`
	NextWindow time.Time             `json:"nextWindow,omitzero"` // the effective policy's next window, box time
	LastCheck  time.Time             `json:"lastCheck,omitzero"`
	NextCheck  time.Time             `json:"nextCheck,omitzero"`
	Waiting    *AutoWait             `json:"waiting,omitempty"`
	Last       *linkproto.ReportAuto `json:"last,omitempty"` // the last automatic attempt
	Error      string                `json:"error,omitempty"`
}

// PolicyStatus is one policy the box holds, as it verifies now.
type PolicyStatus struct {
	Scope         linkproto.PolicyScope   `json:"scope"`
	Seq           int64                   `json:"seq"`
	Target        *linkproto.PolicyTarget `json:"target"`
	Hold          bool                    `json:"hold"`
	Window        linkproto.PolicyWindow  `json:"window"`
	HealthMinutes int                     `json:"healthMinutes,omitempty"`
	Rollbacks     []linkproto.RollbackPin `json:"rollbacks,omitempty"` // the signed digests a rollback may install (D8)
	IssuedAt      time.Time               `json:"issuedAt,omitzero"`
	ExpiresAt     time.Time               `json:"expiresAt,omitzero"`
	By            string                  `json:"by,omitempty"`
	KeyID         string                  `json:"keyId"`
	AcceptedAt    time.Time               `json:"acceptedAt"`
	Applies       bool                    `json:"applies"` // names this box's id
	Expired       bool                    `json:"expired"`
	Error         string                  `json:"error,omitempty"` // it no longer verifies (its key retired): not used
}

func (s *Store) loadAuto() (autoFile, error) {
	s.autoMu.Lock()
	defer s.autoMu.Unlock()
	b, err := readLinkFile(s.Dir, autoStatePath(s.Dir), 4<<20)
	if errors.Is(err, os.ErrNotExist) {
		return autoFile{V: 1}, nil
	}
	if err != nil {
		return autoFile{}, fmt.Errorf("link\\autoupgrade.json cannot be read: %w", err)
	}
	var af autoFile
	if err := json.Unmarshal(b, &af); err != nil {
		return autoFile{}, fmt.Errorf("link\\autoupgrade.json does not parse (delete it to start over): %w", err)
	}
	return af, nil
}

func (s *Store) saveAuto(af autoFile) error {
	s.autoMu.Lock()
	defer s.autoMu.Unlock()
	af.V = 1
	if len(af.Attempts) > maxAutoAttempts {
		af.Attempts = af.Attempts[len(af.Attempts)-maxAutoAttempts:]
	}
	b, _ := json.MarshalIndent(af, "", "  ")
	return writeLinkFile(s.Dir, autoStatePath(s.Dir), append(b, '\n'))
}

// autoInputs is everything decideAuto reads.
type autoInputs struct {
	state    State
	stateErr error
	pf       policyFile
	pfErr    error
	afErr    error
	team     linkproto.Keyring
	minEpoch int
	epochErr error
	build    string
	now      time.Time // the box's local time
	attempts []autoAttempt
	tally    []autoTally
	pending  bool        // an upgrade ticket (a command's or the scheduler's) is running
	stated   statedPlace // what the relay last stated: shown, never weighed
}

// autoView is decideAuto's answer.
type autoView struct {
	status      AutoUpgradeStatus
	policy      *linkproto.UpgradePolicy // the deciding policy, when one is valid
	target      version
	due         bool
	windowStart time.Time
}

// decideAuto is what the box's policies decide at in.now: the status, and whether an upgrade is due now.
func decideAuto(in autoInputs) autoView {
	var v autoView
	s := &v.status
	s.Enabled = in.state.AutoUpgradeOn()
	s.Tenant, s.Ring, s.StatedAt = in.stated.tenant, in.stated.ring, in.stated.at
	s.Policies = []PolicyStatus{}
	hold := func(why string) autoView {
		s.Effective.Hold, s.Effective.Why = true, why
		return v
	}
	switch {
	case in.stateErr != nil:
		s.Error = "link.yaml: " + in.stateErr.Error()
		return hold(linkproto.WhyStateUnreadable)
	case in.state.UnknownKeys():
		s.Error = "link.yaml names a key the link does not know (a misspelt autoUpgrade or admin?)"
		return hold(linkproto.WhyStateUnreadable)
	case in.pfErr != nil:
		s.Error = in.pfErr.Error()
		return hold(linkproto.WhyStateUnreadable)
	case in.epochErr != nil:
		s.Error = "the team-key epoch floor: " + in.epochErr.Error()
		return hold(linkproto.WhyStateUnreadable)
	case in.afErr != nil:
		s.Error = in.afErr.Error()
		return hold(linkproto.WhyStateUnreadable)
	}
	// What the box holds, as Decide weighs it: a policy whose key is now below the epoch floor no longer verifies and
	// is not followed, but it still holds for its scope — a less specific policy does not take over (review m9).
	var held []linkproto.UpgradePolicy
	retired := map[string]int64{} // scope key → the seq of a held policy that no longer verifies
	for _, sp := range in.pf.Policies {
		ps := PolicyStatus{Scope: sp.Scope, Seq: sp.Seq, AcceptedAt: sp.AcceptedAt, KeyID: sp.Signed.KeyID,
			Applies: sp.Scope.Names(in.state.Box)}
		p, _, err := in.team.VerifySignedPolicy(sp.Signed, in.minEpoch)
		if err != nil {
			ps.Error = "no longer verifies: " + refusalCode(err)
			s.Policies = append(s.Policies, ps)
			if ps.Applies {
				held = append(held, linkproto.UpgradePolicy{Scope: sp.Scope, Seq: sp.Seq, Hold: true})
				retired[sp.Scope.Key()] = sp.Seq
			}
			continue
		}
		ps.Target, ps.Hold, ps.Window, ps.HealthMinutes = p.Target, p.Hold, p.Window, p.HealthMinutes
		ps.IssuedAt, ps.ExpiresAt, ps.By, ps.Rollbacks = p.IssuedAt, p.ExpiresAt, p.By, p.Rollbacks
		ps.Applies = linkproto.Applies(p, in.state.Box)
		ps.Expired = p.ActiveAt(in.now) != nil
		if ps.Applies {
			held = append(held, p)
		}
		s.Policies = append(s.Policies, ps)
	}
	d := linkproto.Decide(held, in.state.Box, in.now)
	if !d.Found {
		return hold(linkproto.WhyNoPolicy)
	}
	eff := d.Policy
	v.policy = &eff
	scope := eff.Scope.Ref()
	s.Effective.Scope, s.Effective.Seq, s.Effective.ExpiresAt = &scope, eff.Seq, eff.ExpiresAt
	if d.Hold {
		why := d.Why
		if seq, ok := retired[eff.Scope.Key()]; ok && seq == eff.Seq {
			why = linkproto.WhyPolicyRevoked
		}
		if d.Braked != nil {
			s.Braked = &PolicyRef{Scope: d.Braked.Scope.Ref(), Seq: d.Braked.Seq}
		}
		return hold(why)
	}
	window := eff.Window
	s.Effective.Version, s.Effective.Window = eff.Target.Version, &window
	target, _ := parseVersion(versionRe, eff.Target.Version) // the policy's shape was checked
	v.target = target
	from, ok := parseVersion(buildRe, in.build)
	switch {
	case !ok:
		s.Error = fmt.Sprintf("this link's build %q is not a release, so newer cannot be judged", in.build)
		return hold(linkproto.WhyStopped)
	case !from.less(target):
		s.Effective.Why = linkproto.WhyUpToDate
		return v
	case !in.state.AdminOn():
		return hold(linkproto.WhyAdminOff)
	case !s.Enabled:
		return hold(linkproto.WhyAutoUpgradeOff)
	case in.pending:
		s.Effective.Why = linkproto.WhyUpgrading
		return v
	}
	failed, stopped := 0, false
	var last *autoAttempt
	for i := range in.attempts {
		a := &in.attempts[i]
		if a.Scope.Key() != eff.Scope.Key() || a.Seq != eff.Seq || a.To != target.String() {
			continue
		}
		if a.Outcome != linkproto.OutcomeDeferred { // a setup deferred for orders is "not now": the window stays open
			last = a
		}
		switch {
		case stops(a.Outcome):
			stopped = true
		case a.Outcome == linkproto.OutcomeFailed, a.Outcome == linkproto.OutcomeLost:
			failed++
		}
	}
	for _, t := range in.tally { // review m10: what aged out of the attempts is still here
		if t.Scope.Key() == eff.Scope.Key() && t.Seq == eff.Seq && t.To == target.String() {
			stopped = stopped || t.Stopped != ""
			failed = max(failed, t.Failed)
		}
	}
	if stopped || failed >= maxAutoTickets {
		return hold(linkproto.WhyStopped)
	}
	s.Effective.Why = linkproto.WhyScheduled
	start, inWindow := eff.Window.Start(in.now)
	if !inWindow || (last != nil && last.WindowStart.Equal(start)) { // one attempt per window occurrence
		s.NextWindow = eff.Window.NextStart(in.now)
		return v
	}
	v.due, v.windowStart = true, start
	return v
}

func (l *Link) autoInputs(now time.Time, af autoFile, afErr error) autoInputs {
	in := autoInputs{team: l.cfg.Team, build: l.cfg.Build, now: now, attempts: af.Attempts, tally: af.Tally, afErr: afErr,
		stated: l.statedNow()}
	in.state, in.stateErr = l.cfg.Store.LoadState()
	in.minEpoch, in.epochErr = l.cfg.Store.MinEpochStrict(CompiledMinEpoch)
	l.cfg.Store.policyMu.Lock()
	in.pf, in.pfErr = l.cfg.Store.loadPolicies()
	l.cfg.Store.policyMu.Unlock()
	in.pending, _ = runningTicket(l.upgradeDataDir(), now)
	return in
}

// autoKickEvery is the least time between a check and one a delivery asks for (review m2); a kick sooner is a check
// then, however many came. A variable for the tests only.
var autoKickEvery = 10 * time.Second

// runAuto runs the checks until ctx ends: the first a minute or three after the start (the relay delivers on
// connect), then every AutoEvery, every minute while an automatic ticket runs, and after a delivery that changed a
// policy — at most one such check per autoKickEvery.
func (l *Link) runAuto(ctx context.Context) {
	if l.cfg.AutoEvery < 0 {
		return
	}
	first := l.cfg.AutoFirst
	if first <= 0 {
		first = time.Minute + time.Duration(rand.Int64N(int64(2*time.Minute)))
	}
	t := time.NewTimer(first)
	defer t.Stop()
	due := time.Now().Add(first)
	var lastCheck time.Time
	stop := func() {
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-l.autoKick:
			if soon := lastCheck.Add(autoKickEvery); time.Now().Before(soon) {
				if soon.Before(due) {
					stop()
					t.Reset(time.Until(soon))
					due = soon
				}
				continue
			}
			stop()
		}
		next := l.autoCheck()
		lastCheck = time.Now()
		wait := max(next.Sub(l.cfg.Now()), time.Second)
		t.Reset(wait)
		due = time.Now().Add(wait)
	}
}

// autoCheck is one check: it refreshes the outcome of the automatic tickets, decides, starts an upgrade when one is
// due, writes autoupgrade.json and reports what changed. It returns when to check next.
func (l *Link) autoCheck() time.Time {
	l.autoMu.Lock()
	defer l.autoMu.Unlock()
	l.autoChecks.Add(1)
	now := l.cfg.Now()
	af, afErr := l.cfg.Store.loadAuto()
	if afErr != nil {
		l.logEvery("auto-state", func() { l.cfg.Log.Error("link: auto-update holds", afErr) })
	}
	changed := afErr == nil && l.refreshOutcomes(&af, now, true)
	v := decideAuto(l.autoInputs(now, af, afErr))
	attempted := false
	if v.due {
		attempted = l.autoAttempt(v, &af, now)
		v = decideAuto(l.autoInputs(now, af, afErr))
	}
	next := now.Add(l.autoInterval(af))
	s := v.status
	s.Refused, s.LastCheck, s.NextCheck, s.Waiting = l.lastRefused(), now.UTC(), next.UTC(), af.Waiting
	s.Last = l.lastAuto(af, now)
	if afErr == nil {
		af.View = &s
		if err := l.cfg.Store.saveAuto(af); err != nil {
			l.logEvery("auto-save", func() { l.cfg.Log.Error("link: writing link\\autoupgrade.json", err) })
		}
	}
	if attempted || changed {
		l.sendReport(l.current())
	}
	return next
}

// autoInterval is the wait to the next check: a minute while an automatic ticket runs, else AutoEvery ± 10 %.
func (l *Link) autoInterval(af autoFile) time.Duration {
	for _, a := range af.Attempts {
		if a.Ticket != "" && a.Outcome == linkproto.OutcomePending {
			return autoPendingEvery
		}
	}
	every := l.cfg.AutoEvery
	if every <= 0 {
		every = 10 * time.Minute
	}
	spread := every / 10
	if spread <= 0 {
		return every
	}
	return every - spread + time.Duration(rand.Int64N(int64(2*spread)))
}

// autoAttempt starts the upgrade v found due (startUpgrade, the command's own path) and records the attempt — before
// the setup stops this link. True when an attempt was recorded (a ticket started, or a refusal that waits for the next
// window); false for "not now".
func (l *Link) autoAttempt(v autoView, af *autoFile, now time.Time) bool {
	p := v.policy
	from := l.cfg.Build
	if f, ok := parseVersion(buildRe, l.cfg.Build); ok {
		from = f.String()
	}
	req := upgradeRequest{To: v.target, SHA256: p.Target.SHA256, Ticket: TicketFor("auto/" + linkproto.RandomNonce(16)),
		By: fmt.Sprintf("auto-update: policy %s #%d approved by %s", p.Scope, p.Seq, p.By), HealthMinutes: p.HealthMinutes,
		Policy: &PolicyRef{Scope: p.Scope.Ref(), Seq: p.Seq}}
	if pin, ok := p.RollbackFor(from); ok { // D8: the policy's signed digest of the build this box runs, if it has one
		req.Rollback = &pin
	}
	upgradeMu.Lock()
	ans, err := l.startUpgrade(req)
	upgradeMu.Unlock()
	a := autoAttempt{At: now.UTC(), Scope: p.Scope.Ref(), Seq: p.Seq, From: from, To: v.target.String(), WindowStart: v.windowStart}
	if err != nil {
		var r *UpgradeRefusal
		if !errors.As(err, &r) {
			r = upgradeRefuse("internal", "read link.log", "%v", err)
		}
		r.Message = withoutCredentials(r.Message)
		if autoWaitCodes[r.Code] {
			af.Waiting = &AutoWait{At: now.UTC(), Code: r.Code, Message: r.Message}
			l.logEvery("auto-wait-"+r.Code, func() {
				l.cfg.Log.Info(fmt.Sprintf("link: auto-update %s -> %s due but not now (%s); tried again at the next check", from, a.To, r.Error()))
			})
			return false
		}
		a.Outcome, a.Refusal = linkproto.OutcomeRefused, r
		l.cfg.Log.Warn(fmt.Sprintf("link: auto-update %s -> %s (policy %s #%d) refused: %s; tried again in the next window", from, a.To, p.Scope, p.Seq, r.Error()))
	} else {
		a.Ticket, a.Outcome, af.Waiting = ans.Ticket, linkproto.OutcomePending, nil
		l.cfg.Log.Info(fmt.Sprintf("link: auto-update %s -> %s accepted, ticket %s (policy %s #%d approved by %s); the runner takes it from here",
			ans.From, ans.To, ans.Ticket, p.Scope, p.Seq, p.By))
	}
	af.Attempts = append(af.Attempts, a)
	if serr := l.cfg.Store.saveAuto(*af); serr != nil {
		l.cfg.Log.Error("link: writing link\\autoupgrade.json", serr)
	}
	return true
}

// refreshOutcomes updates the outcome of the automatic tickets still pending; true when one changed.
func (l *Link) refreshOutcomes(af *autoFile, now time.Time, log bool) bool {
	changed := false
	for i := range af.Attempts {
		a := &af.Attempts[i]
		if a.Ticket == "" || a.Outcome != linkproto.OutcomePending {
			continue
		}
		if out, _ := ticketOutcome(l.upgradeDataDir(), a.Ticket, now); out != a.Outcome {
			a.Outcome, changed = out, true
			af.tallyOutcome(*a, now)
			if log {
				l.cfg.Log.Info(fmt.Sprintf("link: auto-update ticket %s (%s -> %s) ended: %s", a.Ticket, a.From, a.To, out))
			}
		}
	}
	return changed
}

// lastAuto is the newest automatic attempt as reports and link-status show it, its health and rollback from the
// ticket's result.
func (l *Link) lastAuto(af autoFile, now time.Time) *linkproto.ReportAuto {
	return lastAutoIn(l.upgradeDataDir(), af, now)
}

func lastAutoIn(dataDir string, af autoFile, now time.Time) *linkproto.ReportAuto {
	if len(af.Attempts) == 0 {
		return nil
	}
	a := af.Attempts[len(af.Attempts)-1]
	scope := a.Scope
	r := &linkproto.ReportAuto{Ticket: a.Ticket, At: a.At, Scope: &scope, Seq: a.Seq, From: a.From, To: a.To, Outcome: a.Outcome}
	if a.Refusal != nil {
		r.Error = a.Refusal.Error()
	}
	if a.Ticket != "" {
		out, res := ticketOutcome(dataDir, a.Ticket, now)
		r.Outcome = out
		if res != nil {
			r.Health, r.Rollback, r.Error = res.Health, res.Rollback, res.Error
		}
	}
	return r
}

// autoStatus is the status now, for link-status and reports: decided afresh, with the last check's times.
func (l *Link) autoStatus() AutoUpgradeStatus {
	now := l.cfg.Now()
	af, afErr := l.cfg.Store.loadAuto()
	if afErr == nil {
		l.refreshOutcomes(&af, now, false)
	}
	s := decideAuto(l.autoInputs(now, af, afErr)).status
	s.Refused, s.Waiting, s.Last = l.lastRefused(), af.Waiting, l.lastAuto(af, now)
	if af.View != nil {
		s.LastCheck, s.NextCheck = af.View.LastCheck, af.View.NextCheck
	}
	return s
}

// ReadAutoUpgradeStatus is the link's last view (link\autoupgrade.json), with the last ticket's outcome read afresh:
// provision's state shows it. Nil before the link's first check.
func ReadAutoUpgradeStatus(dataDir string, now time.Time) (*AutoUpgradeStatus, error) {
	af, err := (&Store{Dir: dataDir}).loadAuto()
	if err != nil {
		return nil, err
	}
	if af.View == nil {
		return nil, nil
	}
	s := *af.View
	s.Last = lastAutoIn(dataDir, af, now)
	return &s, nil
}

// readResult reads a ticket's result file by handle.
func readResult(dataDir, ticket string) (UpgradeResult, error) {
	f, err := dataacl.OpenRead(dataDir, resultPath(dataDir, ticket))
	if err != nil {
		return UpgradeResult{}, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, 256<<10))
	if err != nil {
		return UpgradeResult{}, err
	}
	var res UpgradeResult
	if err := json.Unmarshal(b, &res); err != nil {
		return UpgradeResult{}, err
	}
	return res, nil
}

// ticketOutcome is a ticket's outcome: from its result, else pending or lost.
func ticketOutcome(dataDir, ticket string, now time.Time) (string, *UpgradeResult) {
	res, err := readResult(dataDir, ticket)
	switch {
	case err == nil:
		return resultOutcome(res), &res
	case !errors.Is(err, os.ErrNotExist):
		return linkproto.OutcomeFailed, nil // a result that cannot be read is no success
	}
	if pending, _ := ticketPending(dataDir, ticket, now); pending {
		return linkproto.OutcomePending, nil
	}
	return linkproto.OutcomeLost, nil
}

// resultOutcome is what a result says, in linkproto's words: the runner's own outcome, else (a result written before
// it named one) what its fields say.
func resultOutcome(res UpgradeResult) string {
	switch {
	case res.Outcome != "":
		return res.Outcome
	case res.Rollback != nil && res.Rollback.OK:
		return linkproto.OutcomeRolledBack
	case res.Rollback != nil, res.Health != nil && !res.Health.OK:
		return linkproto.OutcomeUnhealthy
	case res.Error != "":
		return linkproto.OutcomeFailed
	}
	return linkproto.OutcomeOK
}
