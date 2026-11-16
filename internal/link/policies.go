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
// that name this box in DataDir\link\policy.json — the highest seq per scope key, never a lower or a different one at
// the same seq — and answers on a report stream signed by its own key. autoupgrade.go decides when to act on them.
//
// What the relay can do: withhold a policy (the box keeps what it holds; an expired one holds for its scope). What it
// cannot: invent or alter a policy, replay an older one for a scope, or choose which policies apply — a policy names
// every box or an explicit signed list of box ids (D9), and the tenant and ring the relay states are only repeated in
// the box's reports.

// policyFile is DataDir\link\policy.json: the accepted policies, one per scope key. What the relay states about the
// box (its tenant and ring) is not in it: that is information for reports only (D9, review M1), kept in memory.
type policyFile struct {
	V        int            `json:"v"`
	Policies []storedPolicy `json:"policies"`
}

// statedPlace is the tenant and ring the relay last stated for this box: repeated in reports and link-status, never
// used to decide anything (D9, review M1).
type statedPlace struct {
	tenant, ring string
	at           time.Time
}

// noteStated records what a delivery states. A change is logged at most once a minute (review m2: a relay alternating
// its statements must not flood link.log and the Event Log).
func (l *Link) noteStated(tenant, ring string, now time.Time) {
	l.statedMu.Lock()
	prev := l.stated
	l.stated = statedPlace{tenant: tenant, ring: ring, at: now.UTC()}
	l.statedMu.Unlock()
	if prev.tenant != tenant || prev.ring != ring {
		l.logEvery("policy-stated", func() {
			l.cfg.Log.Info(fmt.Sprintf("link: the relay states tenant %q, ring %q for this box (it was %q, %q): information only, no policy depends on it",
				tenant, ring, prev.tenant, prev.ring))
		})
	}
}

// statedNow is what the relay last stated.
func (l *Link) statedNow() statedPlace {
	l.statedMu.Lock()
	defer l.statedMu.Unlock()
	return l.stated
}

// storedPolicy is one accepted policy: the signed bytes as delivered, verified again at every use. Epoch is the epoch
// of the team key that signed it: once the box's epoch floor is above it, its seq no longer binds (review m1).
type storedPolicy struct {
	Scope      linkproto.PolicyScope  `json:"scope"`
	Seq        int64                  `json:"seq"`
	Epoch      int                    `json:"epoch,omitempty"`
	AcceptedAt time.Time              `json:"acceptedAt"`
	Signed     linkproto.SignedPolicy `json:"signed"`
}

// loadPolicies reads policy.json; none yet is an empty store. One that cannot be read is an error: the scheduler holds,
// and a delivery starts over only against the seq floors kept apart (loadFloors), never from nothing. Unknown fields
// are ignored, so a build rolled back to reads what a later one wrote.
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

// policyFloor is the highest seq the box accepted for one scope key, the SHA-256 of that policy's signed bytes (the
// same seq comes back only as the same bytes) and the epoch of the key that signed it (once the box's epoch floor is
// above it, the floor no longer binds: review m1).
type policyFloor struct {
	Seq    int64  `json:"seq"`
	SHA256 string `json:"sha256"`
	Epoch  int    `json:"epoch,omitempty"`
}

type floorRecord struct {
	V      int                    `json:"v"`
	Floors map[string]policyFloor `json:"floors"`
}

// loadFloors reads the seq floors (review m3, m4: kept in the secrets store beside the key-epoch floor, never pruned,
// never started over). present is false when none was ever stored; one that cannot be read is an error (fail closed).
func (s *Store) loadFloors() (floors map[string]policyFloor, present bool, err error) {
	if s.Secrets == nil {
		return nil, false, errors.New("no secrets store for the policy seq floors")
	}
	b, err := s.Secrets.Get(secretPolicyFloors)
	if errors.Is(err, ErrNoSecret) {
		return map[string]policyFloor{}, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("the policy seq floors cannot be read: %w", err)
	}
	var rec floorRecord
	if err := json.Unmarshal(b, &rec); err != nil || rec.V != 1 {
		return nil, false, fmt.Errorf("the policy seq floors do not parse (version %d): %v", rec.V, err)
	}
	if rec.Floors == nil {
		rec.Floors = map[string]policyFloor{}
	}
	return rec.Floors, true, nil
}

func (s *Store) saveFloors(floors map[string]policyFloor) error {
	b, _ := json.Marshal(floorRecord{V: 1, Floors: floors})
	return s.Secrets.Set(secretPolicyFloors, b)
}

// floorOf is the floor an accepted policy sets.
func floorOf(sp storedPolicy) policyFloor {
	return policyFloor{Seq: sp.Seq, SHA256: linkproto.SHA256Hex(sp.Signed.Policy), Epoch: sp.Epoch}
}

// mergeFloors raises floors to what policy.json holds (a floor record lost, or older than the file).
func mergeFloors(floors map[string]policyFloor, pf policyFile) {
	for _, sp := range pf.Policies {
		key := sp.Scope.Key()
		if cur, ok := floors[key]; !ok || sp.Seq > cur.Seq {
			floors[key] = floorOf(sp)
		}
	}
}

func (s *Store) savePolicies(pf policyFile) error {
	pf.V = 1
	b, _ := json.MarshalIndent(pf, "", "  ")
	return writeLinkFile(s.Dir, policyPath(s.Dir), append(b, '\n'))
}

// put records an accepted policy for its scope key against the floor kept for that key (review m3, m4: apart from
// policy.json, so a lost file reopens nothing): a higher seq replaces; the same seq is the same policy delivered again
// only with the same bytes (taken back into policy.json when the file lost it); anything else is refused
// (seq_not_newer). A floor whose key is now below the epoch floor (minEpoch) no longer binds (review m1): a stolen
// key's seq does not outlive the key's retirement. changed: policy.json changed.
func (pf *policyFile) put(p linkproto.UpgradePolicy, sp linkproto.SignedPolicy, epoch, minEpoch int, floors map[string]policyFloor,
	now time.Time) (changed bool, code string) {
	key := p.Scope.Key()
	entry := storedPolicy{Scope: p.Scope, Seq: p.Seq, Epoch: epoch, AcceptedAt: now.UTC(), Signed: sp}
	at := slices.IndexFunc(pf.Policies, func(cur storedPolicy) bool { return cur.Scope.Key() == key })
	floor, held := floors[key]
	binds := held && !(floor.Epoch > 0 && floor.Epoch < minEpoch) // a floor of unknown epoch binds (fail closed)
	switch {
	case !binds, p.Seq > floor.Seq:
	case p.Seq == floor.Seq && linkproto.SHA256Hex(sp.Policy) == floor.SHA256:
		if at >= 0 && bytes.Equal(pf.Policies[at].Signed.Policy, sp.Policy) {
			return false, "" // delivered again: nothing changes
		}
	default:
		return false, linkproto.CodeStaleSeq
	}
	if at >= 0 {
		pf.Policies[at] = entry
	} else {
		pf.Policies = append(pf.Policies, entry)
	}
	floors[key] = floorOf(entry)
	return true, ""
}

// policyStream reads one delivery and hands it to the connection's delivery gate. The relay opened a policy stream,
// so it is a relay that takes report streams on this connection. The slot goes as soon as the frame is read: the
// relay waits for the report before its next delivery, and the gate keeps at most one delivery waiting.
func (l *Link) policyStream(c *conn, st net.Conn, release func()) {
	if c != nil {
		c.policies.Store(true)
	}
	_ = st.SetReadDeadline(time.Now().Add(frameWait))
	raw, err := linkproto.ReadFrame(st, linkproto.MaxPolicyFrame)
	_ = st.Close()
	release()
	if err != nil {
		return
	}
	if c == nil {
		l.takeDelivery(nil, raw)
		return
	}
	c.deliveries.offer(raw, l.cfg.DeliveryEvery, func(raw []byte) { l.takeDelivery(c, raw) })
}

// takeDelivery records a delivery, reports, and asks the scheduler for a check only when a policy changed (review m2:
// a relay re-sending the same policies costs a verification and a report, never a write, a check or a log line).
func (l *Link) takeDelivery(c *conn, raw []byte) {
	refused, changed, err := l.acceptDelivery(raw)
	if err != nil {
		code := refusalCode(err)
		l.logEvery("policy-delivery-"+code, func() { l.cfg.Log.Warn("link: a policy delivery was refused: " + code) })
		refused = []linkproto.ReportRefusal{{Code: code}}
	}
	l.refusedMu.Lock()
	l.refused = refused
	l.refusedMu.Unlock()
	l.sendReport(c)
	if changed {
		l.kickAuto()
	}
}

// deliveryGate takes at most one delivery per period on one connection (review m2): one arriving sooner waits, and of
// those waiting only the newest is taken, once, when the period is over.
type deliveryGate struct {
	mu      sync.Mutex
	last    time.Time
	pending []byte
	armed   bool
}

// offer takes raw at once when the gate is open (every <= 0: always), else keeps it — replacing one kept before — for
// one take when it opens.
func (g *deliveryGate) offer(raw []byte, every time.Duration, take func([]byte)) {
	g.mu.Lock()
	now := time.Now()
	if every <= 0 || (!g.armed && !now.Before(g.last.Add(every))) {
		g.last = now
		g.mu.Unlock()
		take(raw)
		return
	}
	g.pending = raw
	if !g.armed {
		g.armed = true
		time.AfterFunc(g.last.Add(every).Sub(now), func() {
			g.mu.Lock()
			raw := g.pending
			g.pending, g.armed, g.last = nil, false, time.Now()
			g.mu.Unlock()
			take(raw)
		})
	}
	g.mu.Unlock()
}

// acceptDelivery verifies a delivery's policies and keeps those that name this box's id. The error refuses the
// delivery as a whole (nothing is recorded); refused lists the policies refused one by one; changed says a policy was
// accepted (and written).
func (l *Link) acceptDelivery(raw []byte) (refused []linkproto.ReportRefusal, changed bool, err error) {
	d, err := linkproto.DecodePolicyDelivery(raw)
	if err != nil {
		return nil, false, err
	}
	st, err := l.cfg.Store.LoadState()
	switch {
	case err != nil:
		return nil, false, &linkproto.Refusal{Code: codeStateUnreadable}
	case st.Box == "":
		return nil, false, &linkproto.Refusal{Code: linkproto.CodeBoxUnknown}
	case d.Box != st.Box:
		return nil, false, &linkproto.Refusal{Code: linkproto.CodeWrongBox}
	}
	minEpoch, err := l.cfg.Store.MinEpochStrict(CompiledMinEpoch)
	if err != nil {
		return nil, false, &linkproto.Refusal{Code: codeStateUnreadable}
	}
	l.cfg.Store.policyMu.Lock()
	defer l.cfg.Store.policyMu.Unlock()
	floors, floorsKept, err := l.cfg.Store.loadFloors()
	if err != nil {
		l.logEvery("policy-floors", func() { l.cfg.Log.Error("link: policies are refused until it is fixed by hand", err) })
		return nil, false, &linkproto.Refusal{Code: codeStateUnreadable}
	}
	pf, err := l.cfg.Store.loadPolicies()
	switch {
	case err != nil && !floorsKept:
		// no floors to judge a replay by: never start over (fail closed)
		l.logEvery("policy-store", func() { l.cfg.Log.Error("link: policies are refused until it is fixed by hand", err) })
		return nil, false, &linkproto.Refusal{Code: codeStateUnreadable}
	case err != nil:
		// review m4: the floors are kept apart, so the box holds and takes the policies again, none below them
		l.logEvery("policy-store", func() {
			l.cfg.Log.Warn("link: " + err.Error() + "; the box holds and takes the relay's policies again, none below the seq floors it keeps")
		})
		pf = policyFile{V: 1, Policies: []storedPolicy{}}
	}
	mergeFloors(floors, pf)
	now := l.cfg.Now()
	l.noteStated(d.Tenant, d.Ring, now)
	refused = []linkproto.ReportRefusal{}
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
		minEpoch = max(minEpoch, key.Epoch)
		code := ""
		if err := p.ActiveAt(now); err != nil {
			code = refusalCode(err)
		} else if !linkproto.Applies(p, st.Box) {
			code = linkproto.CodeWrongScope
		} else {
			var accepted bool
			if accepted, code = pf.put(p, sp, key.Epoch, minEpoch, floors, now); accepted {
				changed = true
				l.cfg.Log.Info(fmt.Sprintf("link: policy %s #%d accepted (key %s): %s; by %q, expires %s",
					p.Scope, p.Seq, key.ID, describePolicy(p), p.By, p.ExpiresAt.UTC().Format(time.RFC3339)))
			}
		}
		if code != "" {
			scope := p.Scope.Ref()
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
	if !changed {
		return refused, false, nil // the same policies again (the relay's hourly delivery): nothing to write
	}
	// the floors first: a crash between the two writes leaves them at or above policy.json, never below
	if err := l.cfg.Store.saveFloors(floors); err != nil {
		l.cfg.Log.Error("link: saving the policy seq floors", err)
		return nil, false, &linkproto.Refusal{Code: codeStateUnreadable}
	}
	if err := l.cfg.Store.savePolicies(pf); err != nil {
		l.cfg.Log.Error("link: saving link\\policy.json", err)
		return nil, false, &linkproto.Refusal{Code: codeStateUnreadable}
	}
	return refused, true, nil
}

// codeStateUnreadable: the box's own state (link.yaml, the epoch floor, policy.json) cannot be read; it never starts
// over from nothing.
const codeStateUnreadable = "state_unreadable"

// describePolicy is a policy in one line for link.log.
func describePolicy(p linkproto.UpgradePolicy) string {
	if p.Holds() {
		return "hold"
	}
	rollbacks := "none"
	if len(p.Rollbacks) > 0 {
		vs := make([]string, len(p.Rollbacks))
		for i, r := range p.Rollbacks {
			vs[i] = r.Version
		}
		rollbacks = strings.Join(vs, ",")
	}
	return fmt.Sprintf("target %s (setup %s…), window %s %s-%s box time, health %d min, signed rollbacks %s", p.Target.Version,
		p.Target.SHA256[:12], strings.Join(p.Window.Days, ","), p.Window.From, p.Window.To, p.HealthMinutes, rollbacks)
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
		rp := linkproto.ReportPolicy{Scope: p.Scope.Ref(), Seq: p.Seq, Hold: p.Hold || p.Target == nil, ExpiresAt: p.ExpiresAt, Applies: p.Applies}
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
