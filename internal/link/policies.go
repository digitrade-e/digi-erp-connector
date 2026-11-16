package link

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// Managed auto-update, the box's side (plan 088). The relay delivers signed upgrade policies on a policy stream; the
// link verifies each one as it verifies a command's envelope (the compiled-in team keys, the epoch floor), keeps those
// that name this box in DataDir\link\policy.json — the highest seq per scope, never a lower or a different one at the
// same seq — and answers on a report stream signed by its own key. autoupgrade.go decides when to act on them.
//
// What the relay can do: withhold a policy (the box keeps what it holds, and an expired one means hold), or state
// another tenant or ring for the box (plan 088 D6: that only exposes it to another signed, approved policy). What it
// cannot: invent or alter a policy, or replay an older one for a scope.

// maxStoredPolicies bounds policy.json: one entry per scope ever accepted (the seq floor is kept after expiry, so an
// older policy cannot come back); beyond it, the oldest entries that do not apply now go first.
const maxStoredPolicies = 64

// policyFile is DataDir\link\policy.json.
type policyFile struct {
	V        int            `json:"v"`
	Tenant   string         `json:"tenant,omitempty"` // as the relay last stated them (plan 088 D6)
	Ring     string         `json:"ring,omitempty"`
	StatedAt time.Time      `json:"statedAt,omitzero"`
	Policies []storedPolicy `json:"policies"`
}

// storedPolicy is one accepted policy: the signed bytes as delivered, verified again at every use.
type storedPolicy struct {
	Scope      linkproto.PolicyScope  `json:"scope"`
	Seq        int64                  `json:"seq"`
	AcceptedAt time.Time              `json:"acceptedAt"`
	Signed     linkproto.SignedPolicy `json:"signed"`
}

// loadPolicies reads policy.json; none yet is an empty store. One that cannot be read is an error, never an empty
// store: starting over would let a relay replay an older policy (fail closed, as the nonces). Unknown fields are
// ignored, so a build rolled back to reads what a later one wrote.
func (s *Store) loadPolicies() (policyFile, error) {
	b, err := readLinkFile(s.Dir, policyPath(s.Dir), 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return policyFile{V: 1, Policies: []storedPolicy{}}, nil
	}
	if err != nil {
		return policyFile{}, fmt.Errorf("link\\policy.json cannot be read: %w", err)
	}
	var pf policyFile
	if err := json.Unmarshal(b, &pf); err != nil {
		return policyFile{}, fmt.Errorf("link\\policy.json does not parse: %w", err)
	}
	if pf.V != 1 {
		return policyFile{}, fmt.Errorf("link\\policy.json is version %d, not 1", pf.V)
	}
	return pf, nil
}

func (s *Store) savePolicies(pf policyFile) error {
	pf.V = 1
	b, _ := json.MarshalIndent(pf, "", "  ")
	return writeLinkFile(s.Dir, policyPath(s.Dir), append(b, '\n'))
}

// put records an accepted policy for its scope: a higher seq replaces, the same bytes at the same seq are a policy
// delivered again (nothing changes), anything else is refused (seq_not_newer).
func (pf *policyFile) put(p linkproto.UpgradePolicy, sp linkproto.SignedPolicy, now time.Time) (accepted bool, code string) {
	key := p.Scope.Key()
	for i := range pf.Policies {
		cur := &pf.Policies[i]
		if cur.Scope.Key() != key {
			continue
		}
		switch {
		case p.Seq > cur.Seq:
			*cur = storedPolicy{Scope: p.Scope, Seq: p.Seq, AcceptedAt: now.UTC(), Signed: sp}
			return true, ""
		case p.Seq == cur.Seq && bytes.Equal(cur.Signed.Policy, sp.Policy):
			return false, ""
		}
		return false, linkproto.CodeStaleSeq
	}
	pf.Policies = append(pf.Policies, storedPolicy{Scope: p.Scope, Seq: p.Seq, AcceptedAt: now.UTC(), Signed: sp})
	return true, ""
}

// prune keeps at most maxStoredPolicies entries, dropping the oldest that do not name box with the stated tenant and
// ring.
func (pf *policyFile) prune(box string) {
	for len(pf.Policies) > maxStoredPolicies {
		oldest := -1
		for i, sp := range pf.Policies {
			if linkproto.Applies(linkproto.UpgradePolicy{Scope: sp.Scope}, box, pf.Tenant, pf.Ring) {
				continue
			}
			if oldest < 0 || sp.AcceptedAt.Before(pf.Policies[oldest].AcceptedAt) {
				oldest = i
			}
		}
		if oldest < 0 {
			return
		}
		pf.Policies = slices.Delete(pf.Policies, oldest, oldest+1)
	}
}

// policyStream reads one delivery, records it and answers with a report. The relay opened a policy stream, so it is a
// relay that takes report streams on this connection. It lets its slot go before it reports: the relay waits for the
// report before its next delivery, and the link takes one delivery at a time (a second one is closed unread).
func (l *Link) policyStream(c *conn, st net.Conn, release func()) {
	if c != nil {
		c.policies.Store(true)
	}
	_ = st.SetReadDeadline(time.Now().Add(frameWait))
	raw, err := linkproto.ReadFrame(st, linkproto.MaxControlFrame)
	_ = st.Close()
	if err != nil {
		return
	}
	refused, err := l.acceptDelivery(raw)
	if err != nil {
		code := refusalCode(err)
		l.logEvery("policy-delivery-"+code, func() { l.cfg.Log.Warn("link: a policy delivery was refused: " + code) })
		refused = []linkproto.ReportRefusal{{Code: code}}
	}
	l.refusedMu.Lock()
	l.refused = refused
	l.refusedMu.Unlock()
	release()
	l.sendReport(c)
	l.kickAuto()
}

// acceptDelivery verifies a delivery's policies and keeps those that name this box. The error refuses the delivery
// as a whole (nothing is recorded); refused lists the policies refused one by one.
func (l *Link) acceptDelivery(raw []byte) ([]linkproto.ReportRefusal, error) {
	d, err := linkproto.DecodePolicyDelivery(raw)
	if err != nil {
		return nil, err
	}
	st, err := l.cfg.Store.LoadState()
	switch {
	case err != nil:
		return nil, &linkproto.Refusal{Code: codeStateUnreadable}
	case st.Box == "":
		return nil, &linkproto.Refusal{Code: linkproto.CodeBoxUnknown}
	case d.Box != st.Box:
		return nil, &linkproto.Refusal{Code: linkproto.CodeWrongBox}
	}
	minEpoch, err := l.cfg.Store.MinEpochStrict(CompiledMinEpoch)
	if err != nil {
		return nil, &linkproto.Refusal{Code: codeStateUnreadable}
	}
	l.cfg.Store.policyMu.Lock()
	defer l.cfg.Store.policyMu.Unlock()
	pf, err := l.cfg.Store.loadPolicies()
	if err != nil {
		l.logEvery("policy-store", func() { l.cfg.Log.Error("link: policies are refused until it is fixed by hand", err) })
		return nil, &linkproto.Refusal{Code: codeStateUnreadable}
	}
	now := l.cfg.Now()
	if pf.Tenant != d.Tenant || pf.Ring != d.Ring {
		l.cfg.Log.Info(fmt.Sprintf("link: the relay places this box in tenant %q, ring %q (it was %q, %q)", d.Tenant, d.Ring, pf.Tenant, pf.Ring))
	}
	pf.Tenant, pf.Ring, pf.StatedAt = d.Tenant, d.Ring, now.UTC()
	refused := []linkproto.ReportRefusal{}
	unverified, lastUnverified := 0, ""
	for _, sp := range d.Policies {
		p, key, err := l.cfg.Team.VerifySignedPolicy(sp, minEpoch)
		if err != nil {
			code := refusalCode(err)
			refused = append(refused, linkproto.ReportRefusal{Code: code})
			unverified, lastUnverified = unverified+1, code
			continue
		}
		if err := l.cfg.Store.RaiseMinEpoch(key.Epoch, CompiledMinEpoch); err != nil {
			l.cfg.Log.Error("link: recording the key epoch", err)
		}
		code := ""
		if err := p.ActiveAt(now); err != nil {
			code = refusalCode(err)
		} else if !linkproto.Applies(p, st.Box, d.Tenant, d.Ring) {
			code = linkproto.CodeWrongScope
		} else {
			var accepted bool
			if accepted, code = pf.put(p, sp, now); accepted {
				l.cfg.Log.Info(fmt.Sprintf("link: policy %s #%d accepted (key %s): %s; by %q, expires %s",
					p.Scope, p.Seq, key.ID, describePolicy(p), p.By, p.ExpiresAt.UTC().Format(time.RFC3339)))
			}
		}
		if code != "" {
			scope := p.Scope
			refused = append(refused, linkproto.ReportRefusal{Scope: &scope, Seq: p.Seq, Code: code})
			l.logEvery("policy-"+p.Scope.Key()+"-"+code, func() {
				l.cfg.Log.Warn(fmt.Sprintf("link: policy %s #%d refused: %s", p.Scope, p.Seq, code))
			})
		}
	}
	if unverified > 0 {
		// what did not verify is not quoted: a relay cannot choose the text written to link.log and the Event Log
		l.logEvery("policy-unverified", func() {
			l.cfg.Log.Warn(fmt.Sprintf("link: %d policy(ies) of a delivery refused before verification (last: %s)", unverified, lastUnverified))
		})
	}
	pf.prune(st.Box)
	if err := l.cfg.Store.savePolicies(pf); err != nil {
		l.cfg.Log.Error("link: saving link\\policy.json", err)
		return nil, &linkproto.Refusal{Code: codeStateUnreadable}
	}
	return refused, nil
}

// codeStateUnreadable: the box's own state (link.yaml, the epoch floor, policy.json) cannot be read; it never starts
// over from nothing.
const codeStateUnreadable = "state_unreadable"

// describePolicy is a policy in one line for link.log.
func describePolicy(p linkproto.UpgradePolicy) string {
	if p.Holds() {
		return "hold"
	}
	return fmt.Sprintf("target %s (setup %s…), window %s %s-%s box time, health %d min", p.Target.Version, p.Target.SHA256[:12],
		strings.Join(p.Window.Days, ","), p.Window.From, p.Window.To, p.HealthMinutes)
}

// kickAuto asks the scheduler for a check now (after a delivery changed what it holds).
func (l *Link) kickAuto() {
	select {
	case l.autoKick <- struct{}{}:
	default:
	}
}

// lastRefused is the last delivery's refusals.
func (l *Link) lastRefused() []linkproto.ReportRefusal {
	l.refusedMu.Lock()
	defer l.refusedMu.Unlock()
	return slices.Clone(l.refused)
}

// buildReport is the box's view for the relay (linkproto.Report): what it holds, the effective policy and the last
// auto-upgrade.
func (l *Link) buildReport() linkproto.Report {
	st, _ := l.cfg.Store.LoadState()
	s := l.autoStatus()
	r := linkproto.Report{Box: st.Box, At: l.cfg.Now().UTC(), Build: l.cfg.Build, Tenant: s.Tenant, Ring: s.Ring,
		Policies: []linkproto.ReportPolicy{}, Refused: s.Refused, Effective: s.Effective, AutoUpgrade: s.Last}
	for _, p := range s.Policies {
		if p.Error != "" {
			continue // it no longer verifies (its key retired): the box does not hold it
		}
		rp := linkproto.ReportPolicy{Scope: p.Scope, Seq: p.Seq, Hold: p.Hold || p.Target == nil, ExpiresAt: p.ExpiresAt, Applies: p.Applies}
		if p.Target != nil && !p.Hold {
			rp.Target = p.Target.Version
		}
		r.Policies = append(r.Policies, rp)
	}
	return r
}

// reportWait bounds a report stream's write: the relay reads it at once.
const reportWait = 10 * time.Second

// reportWindow is the period of the report budget (Config.ReportBudget, default 5): the relay ends a connection whose
// box opens more than six report streams a minute. A variable for the tests only.
var reportWindow = time.Minute

// reportGate keeps one connection's report streams within the budget: at most budget in any reportWindow, counted in
// real time (the relay's). A report over the budget is owed: the view as it is then goes as soon as the budget allows.
type reportGate struct {
	mu   sync.Mutex
	sent []time.Time
	owed bool
}

// take reserves a report at now, or says how long until one may go.
func (g *reportGate) take(now time.Time, budget int) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	keep := g.sent[:0]
	for _, at := range g.sent {
		if now.Sub(at) < reportWindow {
			keep = append(keep, at)
		}
	}
	g.sent = keep
	if len(g.sent) < budget {
		g.sent = append(g.sent, now)
		return 0
	}
	return max(g.sent[0].Add(reportWindow).Sub(now), time.Millisecond)
}

// owe calls send once after wait, unless a call is owed already (it will carry the newest view).
func (g *reportGate) owe(wait time.Duration, send func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.owed {
		return
	}
	g.owed = true
	time.AfterFunc(wait, func() {
		g.mu.Lock()
		g.owed = false
		g.mu.Unlock()
		send()
	})
}

// sendReport opens a report stream on c and writes the signed report. Only on a connection whose relay opened a
// policy stream: an older relay closes a connection whose box opens a second stream. Within the report budget: one
// over it is sent later, as the then-latest view. False when nothing was sent now.
func (l *Link) sendReport(c *conn) bool {
	if c == nil || !c.policies.Load() {
		return false
	}
	if wait := c.reports.take(time.Now(), l.cfg.ReportBudget); wait > 0 {
		c.reports.owe(wait, func() { l.sendReport(c) })
		return false
	}
	r := l.buildReport()
	signed, err := linkproto.SignReport(l.cfg.Keys, r)
	if err != nil {
		return false
	}
	raw, _ := json.Marshal(signed)
	if len(raw) > linkproto.MaxControlFrame {
		r.Policies, r.Refused = r.Policies[:0], nil // never more than a frame: the essentials stay
		if signed, err = linkproto.SignReport(l.cfg.Keys, r); err != nil {
			return false
		}
		raw, _ = json.Marshal(signed)
	}
	st, err := c.sess.OpenStream()
	if err != nil {
		return false
	}
	defer func() { _ = st.Close() }()
	_ = st.SetWriteDeadline(time.Now().Add(reportWait))
	if _, err := st.Write([]byte{linkproto.StreamReport}); err != nil {
		return false
	}
	return linkproto.WriteFrame(st, raw) == nil
}
