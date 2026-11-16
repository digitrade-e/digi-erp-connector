package provision

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/filelock"
)

// Plan 087 (DIG-1563): the counters command — the only way a named counter
// is created, given a range, moved forward or retired. It is signed and run
// by digi-mcp over the link (an approval form, an audit, a pre-image); the
// data path only reads counters (GET /api/counters), since the B2B shop holds
// the same credential.
//
// create, range and retire change config.yaml's documents.counters: the flow
// of every config command (runConfigChange) — every check before the first
// write, the change set's pre-image of config.yaml, the order queue asked,
// the restart and the proof — and a counter's store, when create writes one,
// is written while the service is stopped. start-from moves a store forward
// only, after the queue is asked, with the service stopped (no writer can
// race it; for a receipt counter the receipt writer's OS lock is taken too)
// and started again. A store is never part of a pre-image: a restore puts
// config.yaml back and never moves a counter back.

// The counters command's ops.
const (
	CounterOpCreate    = "create"
	CounterOpRange     = "range"
	CounterOpStartFrom = "start-from"
	CounterOpRetire    = "retire"
)

// CountersInput is the counters command's stdin.
type CountersInput struct {
	Version   int    `json:"version"` // 1
	ChangeSet string `json:"changeSet"`
	Op        string `json:"op"`  // create | range | start-from | retire
	Key       string `json:"key"` // [a-z][a-z0-9_]{0,31}
	// Kind, Test and Store are create's: orders or receipts; a test counter
	// (its documents only into a test folder); and the pre-087 store it
	// adopts in place (lastOrderNumber, receiptNumber), or "" for a store of
	// its own (DataDir\counters\<key>.json).
	Kind  string `json:"kind,omitempty"`
	Test  bool   `json:"test,omitempty"`
	Store string `json:"store,omitempty"`
	// Min and Max are create's and range's: 1 ≤ min ≤ max ≤ 999 999 999.
	Min *int64 `json:"min,omitempty"`
	Max *int64 `json:"max,omitempty"`
	// From is the number the next document gets: start-from's (required),
	// create's (optional; default: the lowest number above the range's
	// minimum, the adopted store and every number already issued in the
	// range).
	From *int64 `json:"from,omitempty"`
	// ExpectLast is start-from's compare-and-set: the last number issued as
	// the plan read it (-1: the store is missing). Another value on the box
	// refuses (counter_moved): a number was issued in between.
	ExpectLast *int64  `json:"expectLast,omitempty"`
	AssumeIdle bool    `json:"assumeIdle,omitempty"`
	Expect     *Expect `json:"expect,omitempty"`
}

// CounterCheck is a counter as the command would leave it (--check), or as it
// found it.
type CounterCheck struct {
	Key           string `json:"key"`
	Last          int64  `json:"last"`          // the last number issued now (-1: no store)
	HighestIssued int64  `json:"highestIssued"` // in the (new) range, from the index and history
	SetLast       *int64 `json:"setLast,omitempty"`
	InFlight      int    `json:"inFlight"`
}

// DecodeCounters reads one JSON input, strictly like DecodeSpec.
func DecodeCounters(r io.Reader) (CountersInput, *Refusal) {
	var in CountersInput
	if err := decodeStrict(r, &in); err != nil {
		return CountersInput{}, refuse(CodeSpecInvalid, "send one JSON object with only the documented fields", "the counters input does not decode: %v", err)
	}
	return in, nil
}

// Validate checks the input before anything is read.
func (in CountersInput) Validate() *Refusal {
	if r := validateHeader(in.Version, in.ChangeSet, in.Expect); r != nil {
		return r
	}
	if !config.ValidDocumentKey(in.Key) {
		return refuse(CodeSpecInvalid, "use a lower-case letter, then letters, digits or _ (32 at most)", "key %q is not a counter key", in.Key)
	}
	only := func(allowed ...string) *Refusal {
		set := map[string]bool{}
		for _, a := range allowed {
			set[a] = true
		}
		for name, given := range map[string]bool{"kind": in.Kind != "", "test": in.Test, "store": in.Store != "", "min": in.Min != nil,
			"max": in.Max != nil, "from": in.From != nil, "expectLast": in.ExpectLast != nil} {
			if given && !set[name] {
				return refuse(CodeSpecInvalid, "leave "+name+" out of "+in.Op, "%s is not a field of %s", name, in.Op)
			}
		}
		return nil
	}
	inRange := func(name string, v *int64, lo int64) *Refusal {
		if v != nil && (*v < lo || *v > config.MaxDocumentNumber) {
			return refuse(CodeSpecInvalid, fmt.Sprintf("use %d to %d", lo, config.MaxDocumentNumber), "%s %d is out of range", name, *v)
		}
		return nil
	}
	switch in.Op {
	case CounterOpCreate:
		if r := only("kind", "test", "store", "min", "max", "from"); r != nil {
			return r
		}
		if in.Kind != config.DocumentKindOrders && in.Kind != config.DocumentKindReceipts {
			return refuse(CodeSpecInvalid, `use "orders" or "receipts"`, "kind %q is not a counter kind", in.Kind)
		}
		if in.Min == nil || in.Max == nil {
			return refuse(CodeSpecInvalid, "give the range: min and max", "create needs min and max")
		}
	case CounterOpRange:
		if r := only("min", "max"); r != nil {
			return r
		}
		if in.Min == nil || in.Max == nil {
			return refuse(CodeSpecInvalid, "give the new range: min and max", "range needs min and max")
		}
	case CounterOpStartFrom:
		if r := only("from", "expectLast"); r != nil {
			return r
		}
		if in.From == nil || in.ExpectLast == nil {
			return refuse(CodeSpecInvalid, "give from (the next number) and expectLast (the last number as you read it; -1: no store)", "start-from needs from and expectLast")
		}
		if r := inRange("expectLast", in.ExpectLast, -1); r != nil {
			return r
		}
	case CounterOpRetire:
		if r := only(); r != nil {
			return r
		}
	default:
		return refuse(CodeSpecInvalid, `use "create", "range", "start-from" or "retire"`, "op %q is not a counters op", in.Op)
	}
	for name, v := range map[string]*int64{"min": in.Min, "max": in.Max, "from": in.From} {
		if r := inRange(name, v, 1); r != nil {
			return r
		}
	}
	if in.Min != nil && in.Max != nil && *in.Min > *in.Max {
		return refuse(CodeSpecInvalid, "min ≤ max", "min %d is above max %d", *in.Min, *in.Max)
	}
	return nil
}

// ApplyCounters runs the counters command (check: --check, nothing written).
func ApplyCounters(ctx context.Context, env Env, in CountersInput, check bool) (*ApplyResult, error) {
	res := &ApplyResult{ChangeSet: in.ChangeSet, Steps: []Step{}}
	if r := in.Validate(); r != nil {
		return res, r
	}
	a := &applier{env: env, s: Spec{Version: in.Version, ChangeSet: in.ChangeSet, AssumeIdle: in.AssumeIdle, Expect: in.Expect},
		dir: env.Sys.DataDir(), res: res, check: check}
	unlock, err := lockRuns(a.dir)
	if err != nil {
		return res, err
	}
	defer unlock()
	if in.Op == CounterOpStartFrom {
		err = a.counterStartFrom(ctx, in)
	} else {
		err = a.counterConfigChange(ctx, in)
	}
	a.settle()
	return res, err
}

// counterOf is the counter of key in cfg's set, as the daemon builds it.
func (a *applier) counterOf(cfg config.Config, key string) (*hasavshevet.Counter, *Refusal) {
	c, ok := hasavshevet.NewCounterSet(cfg, a.dir).Get(key)
	if !ok || c.Implicit {
		return nil, refuse(CodeCounterUnknown, "list the counters (state, or GET /api/counters) and name one of them",
			"there is no configured counter %s", key)
	}
	return c, nil
}

// scanCounter reads what the index and history say about a range.
func (a *applier) scanCounter(kind string, cfg config.Config, min, max int64) (hasavshevet.IssuedScan, *Refusal) {
	scan, err := hasavshevet.ScanIssued(kind, a.dir, strings.TrimSpace(cfg.SendOrderDir), min, max)
	if err != nil {
		return scan, refuse(CodeWriteFailed, "run it elevated (as an administrator)", "read the %s index and history: %v", kind, err)
	}
	if scan.Unreadable > 0 {
		a.warn("%d %s index entries could not be read; their numbers are not counted (state lists them)", scan.Unreadable, kind)
	}
	return scan, nil
}

// counterLast is a store's last number: -1 when the store is missing.
func counterLast(c *hasavshevet.Counter) (int64, *Refusal) {
	if !c.StorePresent() {
		return -1, nil
	}
	last, err := c.Last()
	if err != nil {
		return 0, refuse(CodeCounterStore, "repair or restore the store file (docs/troubleshooting.md)", "counter %s: %v", c.Key, err)
	}
	return last, nil
}

// counterConfigChange is create, range and retire.
func (a *applier) counterConfigChange(ctx context.Context, in CountersInput) error {
	var chk *CounterCheck
	set := func(t *config.Config) {
		t.Documents = t.Documents.Clone()
		if t.Documents.Counters == nil {
			t.Documents.Counters = map[string]config.CounterConfig{}
		}
		c := t.Documents.Counters[in.Key]
		switch in.Op {
		case CounterOpCreate:
			c = config.CounterConfig{Kind: in.Kind, Min: *in.Min, Max: *in.Max, Test: in.Test, Store: in.Store}
		case CounterOpRange:
			c.Min, c.Max = *in.Min, *in.Max
		case CounterOpRetire:
			c.Retired = true
		}
		t.Documents.Counters[in.Key] = c
	}
	check := func() error {
		cur, exists := a.cur.Documents.Counters[in.Key]
		switch {
		case in.Op == CounterOpCreate && exists:
			return refuse(CodeCounterExists, "name another key, or change this one with range", "counter %s exists", in.Key)
		case in.Op != CounterOpCreate && !exists:
			return refuse(CodeCounterUnknown, "list the counters (state, or GET /api/counters) and name one of them", "there is no configured counter %s", in.Key)
		case in.Op != CounterOpCreate && cur.Retired:
			return refuse(CodeCounterRetired, "a retired counter keeps its range and issues nothing; create another", "counter %s is retired", in.Key)
		}
		c, r := a.counterOf(a.tgt, in.Key)
		if r != nil {
			return r
		}
		last, r := counterLast(c)
		if r != nil {
			return r
		}
		tc := a.tgt.Documents.Counters[in.Key]
		scan, r := a.scanCounter(tc.Kind, a.tgt, tc.Min, tc.Max)
		if r != nil {
			return r
		}
		chk = &CounterCheck{Key: in.Key, Last: last, HighestIssued: scan.Highest, InFlight: scan.InFlight}
		switch in.Op {
		case CounterOpCreate:
			floor := max(tc.Min, last+1, scan.Highest+1)
			from := floor
			if in.From != nil {
				from = *in.From
			}
			switch {
			case last > tc.Max:
				return refuse(CodeCounterRange, "give a range that holds the store's last number", "the store's last number %d is above max %d", last, tc.Max)
			case scan.Highest >= tc.Max:
				return refuse(CodeCounterRange, "give a range above the numbers already issued", "number %d is already issued and the range ends at %d: it would issue nothing", scan.Highest, tc.Max)
			case from < floor:
				return refuse(CodeCounterLower, fmt.Sprintf("start from %d or above", floor),
					"from %d is at or below a number already issued, or below the range (the store's last %d, the highest issued %d, min %d)", from, last, scan.Highest, tc.Min)
			case from > tc.Max:
				return refuse(CodeCounterRange, "start from a number inside the range", "from %d is above max %d", from, tc.Max)
			}
			if from-1 != last {
				setLast := from - 1
				chk.SetLast = &setLast
				// Written while the service is stopped, after config.yaml: no
				// writer races it, and the restarted daemon reads both.
				a.whileStopped = append(a.whileStopped, func() error { return a.raiseCounter(c, setLast, last) })
			}
		case CounterOpRange:
			old, _ := a.counterOf(a.cur, in.Key)
			oldScan, r := a.scanCounter(tc.Kind, a.cur, old.Min, old.Max)
			if r != nil {
				return r
			}
			switch {
			case last >= 0 && (last < tc.Min-1 || last > tc.Max):
				return refuse(CodeCounterRange, fmt.Sprintf("keep min ≤ %d and max ≥ %d (move the counter with start-from first)", last+1, last),
					"the range %d-%d would exclude the counter's last number %d", tc.Min, tc.Max, last)
			case oldScan.Highest > tc.Max:
				return refuse(CodeCounterRange, fmt.Sprintf("keep max ≥ %d", oldScan.Highest),
					"number %d is already issued and the new max %d is below it", oldScan.Highest, tc.Max)
			}
		case CounterOpRetire:
			if scan.InFlight > 0 {
				return refuse(CodeCounterInFlight, "wait until they are written (or resolve them, docs/troubleshooting.md), then retire it",
					"%d %s of counter %s are not finished (reserved or waiting for a resend): a resend after the retirement would still write under its numbers", scan.InFlight, tc.Kind, in.Key)
			}
		}
		return nil
	}
	err := a.runConfigChange(ctx, set, check)
	if a.check && a.res.Check != nil {
		a.res.Check.Counter = chk
	}
	return err
}

// raiseCounter sets a counter's store to last while the service is stopped,
// having read it again: it must still be what the check read (before, -1 for
// no store), or another writer moved it.
func (a *applier) raiseCounter(c *hasavshevet.Counter, last, before int64) error {
	release, r := a.lockCounterWriter(c)
	if r != nil {
		return r
	}
	defer release()
	now, r := counterLast(c)
	if r != nil {
		return r
	}
	if now != before {
		return refuse(CodeCounterMoved, "read the counter again and make a new plan", "counter %s moved from %d to %d during the run", c.Key, before, now)
	}
	if err := c.RaiseTo(last); err != nil {
		if errors.Is(err, hasavshevet.ErrCounterLower) {
			return refuse(CodeCounterLower, "a counter never moves back", "%v", err)
		}
		return refuse(CodeWriteFailed, "run it elevated (as an administrator)", "set counter %s: %v", c.Key, err)
	}
	a.step("counter", true, fmt.Sprintf("%s: last issued set to %d (the next is %d)", c.Key, last, last+1))
	return nil
}

// lockCounterWriter takes the receipt writer's OS lock for a receipt
// counter: only a writer holding it may touch the receipt numbers, so a
// connector process still alive (a stop that did not finish) refuses the
// change instead of racing it. An order counter has one writer, the
// service's process, which is stopped.
func (a *applier) lockCounterWriter(c *hasavshevet.Counter) (func(), *Refusal) {
	if c.Kind != config.DocumentKindReceipts {
		return func() {}, nil
	}
	path := filepath.Join(a.dir, "receipts", "writer.lock")
	held, err := filelock.Lock(path)
	if errors.Is(err, filelock.ErrLocked) {
		return nil, refuse(CodeServiceFailed, "stop the connector (daemon-stop) and run it again", "another process holds the receipt data (%s): the connector is still running", path)
	}
	if err != nil {
		return nil, refuse(CodeWriteFailed, "run it elevated (as an administrator)", "lock the receipt data: %v", err)
	}
	return func() { _ = held.Unlock() }, nil
}

// counterStartFrom moves a counter's store forward: from is the next number.
func (a *applier) counterStartFrom(ctx context.Context, in CountersInput) error {
	if err := a.readCurrent(); err != nil {
		return err
	}
	if a.curBytes == nil {
		return refuse(CodeConfigRefused, "provision the connector with apply first", "there is no config.yaml: this command changes a configured connector")
	}
	cc, ok := a.cur.Documents.Counters[in.Key]
	if !ok {
		return refuse(CodeCounterUnknown, "list the counters (state, or GET /api/counters) and name one of them", "there is no configured counter %s", in.Key)
	}
	if cc.Retired {
		return refuse(CodeCounterRetired, "a retired counter issues nothing; create another", "counter %s is retired", in.Key)
	}
	c, r := a.counterOf(a.cur, in.Key)
	if r != nil {
		return r
	}
	last, r := counterLast(c)
	if r != nil {
		return r
	}
	scan, r := a.scanCounter(c.Kind, a.cur, c.Min, c.Max)
	if r != nil {
		return r
	}
	from := *in.From
	setLast := from - 1
	a.res.Check = nil
	chk := &CounterCheck{Key: in.Key, Last: last, HighestIssued: scan.Highest, InFlight: scan.InFlight, SetLast: &setLast}
	switch {
	case last != *in.ExpectLast:
		return refuse(CodeCounterMoved, "read the counter again (GET /api/counters) and make a new plan",
			"counter %s's last number is %d, the plan expected %d: a number was issued in between", in.Key, last, *in.ExpectLast)
	case from < c.Min || from > c.Max:
		return refuse(CodeCounterRange, fmt.Sprintf("start from a number in %d-%d (widen it with range first)", c.Min, c.Max),
			"from %d is outside counter %s's range %d-%d", from, in.Key, c.Min, c.Max)
	case from <= last || from <= scan.Highest:
		return refuse(CodeCounterLower, fmt.Sprintf("start from %d or above: a counter never moves back", max(last, scan.Highest)+1),
			"from %d is at or below a number already issued (last %d, highest issued %d)", from, last, scan.Highest)
	}
	if a.check {
		a.res.Check = &CheckResult{Config: "unchanged", Counter: chk, Restart: a.running}
		return nil
	}
	// A store of its own lives in DataDir\counters: only under the
	// protected DACL (as receipt-seed's store).
	if err := a.env.Sys.ProtectDataDir(); err != nil {
		return refuse(CodeWriteFailed, `see "The service will not start" in docs/troubleshooting.md`,
			"the data directory could not be protected, so nothing was written: %v", err)
	}
	if !a.running {
		return a.raiseCounter(c, setLast, last)
	}
	// The service runs: it is the writer. Ask its queue, stop it, set the
	// store, start it again and prove it.
	if err := a.idle(ctx, "before the counter is set"); err != nil {
		return err
	}
	if err := a.env.Sys.StopService(ServiceName); err != nil {
		return refuse(CodeServiceFailed, "stop it in services.msc and run it again", "stop the service: %v", err)
	}
	a.touched = true
	a.step("stop", true, "")
	raiseErr := a.raiseCounter(c, setLast, last)
	if err := a.env.Sys.StartService(ServiceName); err != nil {
		if raiseErr != nil {
			return raiseErr
		}
		return refuse(CodeServiceFailed, "read server.log (the logs command) for why it stopped", "the counter was set, but the service did not start: %v", err)
	}
	a.res.Restarted = true
	a.step("start", true, "")
	if raiseErr != nil {
		_ = a.proveAfter(ctx)
		return raiseErr
	}
	return a.proveAfter(ctx)
}

// proveAfter proves the service with config.yaml's credential.
func (a *applier) proveAfter(ctx context.Context) error {
	proof, err := a.prove(ctx, a.cur)
	if err != nil {
		return refuse(CodePortProofFailed, "read server.log (the logs command)", "%v", err)
	}
	a.res.Proof = proof
	if proof.Health != "ok" && proof.Health != HealthNoDatabase {
		a.warn("the connector answers but its database does not (/api/health: %s)", proof.Health)
	}
	return nil
}
