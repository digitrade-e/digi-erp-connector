package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/filelock"
)

// Plan 087 review R6 (connector v1.0.32): the inflight command — resolve ONE
// unfinished order or receipt index entry from afar, instead of a person
// editing the index on the box. Signed (inflight, inflight-check over the
// link; WinRM too), not sealed. digi-mcp's feature: inflight.
//
//   - Input {version, changeSet, kind: orders|receipts, historyId (orders) or
//     number (receipts), expect: the entry's state as the check read it,
//     resolve: fail|done}. The check needs only kind and the key; with
//     resolve it refuses as the run would.
//   - fail: a reserved or retryable entry never written — the box proves no
//     file of it exists (its frozen name in its folder, the pickup file, the
//     history copy), otherwise may_be_written. done (review R6-1): only on
//     proof of delivery — the delivered file or the importer's
//     *.MASOFON2.BAK rename of it, with the history copy's bytes where both
//     exist; the history copy alone is history_only (an order: unless
//     acceptHistoryOnly); nothing at all is not_written.
//   - Compare-and-set on expect (entry_moved); a written or failed entry is
//     final (entry_final); an order the running worker has queued or is
//     writing is entry_busy (a receipt: another process holds the receipt
//     data).
//   - A number is never reused and no counter moves (hasavshevet.Resolve*).
//     The entry records the change set and the time.
//   - The run asks the queue, stops the service, reads the entry again,
//     resolves it (a receipt under DataDir\receipts\writer.lock), starts the
//     service again and proves it — as counters start-from.

// The inflight refusals.
const (
	CodeEntryMissing = "entry_missing" // no entry for that historyId / receipt number
	CodeEntryMoved   = "entry_moved"   // the entry's state is not the one expected
	CodeEntryBusy    = "entry_busy"    // the running worker holds it (or another process the receipt data)
	CodeEntryFinal   = "entry_final"   // written or failed already
	CodeMayBeWritten = "may_be_written"
	CodeNotWritten   = "not_written"
	CodeHistoryOnly  = "history_only" // done on the history copy alone (review R6-1)
	// CodeHistoryMissing: done on a delivered file with no readable history copy (re-review MINOR 1)
	CodeHistoryMissing = "history_missing"
)

// InflightInput is the inflight command's stdin.
type InflightInput struct {
	Version   int    `json:"version"` // 1
	ChangeSet string `json:"changeSet"`
	Kind      string `json:"kind"` // orders | receipts
	// HistoryID names an order (its idempotency key); Number a receipt.
	HistoryID string `json:"historyId,omitempty"`
	Number    int64  `json:"number,omitempty"`
	// Expect is the entry's state as the check read it (compare-and-set):
	// "reserved", "retryable" — or {"state": …, "configSha256": …}.
	Expect *InflightExpect `json:"expect,omitempty"`
	// Resolve is fail or done; the check may leave it out.
	Resolve string `json:"resolve,omitempty"`
	// AcceptHistoryOnly (an order's done only; review R6-1): resolve it as
	// written on its history copy alone — a person confirmed Hashavshevet has
	// it (model (b) leaves nothing else after a real import). The check
	// answer names it, for the approval form.
	AcceptHistoryOnly bool `json:"acceptHistoryOnly,omitempty"`
	AssumeIdle        bool `json:"assumeIdle,omitempty"`
}

// InflightExpect is expect: the entry's state, and optionally config.yaml's
// sha256 as the plan read it (as every config command's expect).
type InflightExpect struct {
	State        string
	ConfigSHA256 *string
}

// UnmarshalJSON takes the state as a string, or {"state", "configSha256"}.
func (e *InflightExpect) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		e.State = s
		return nil
	}
	var o struct {
		State        string  `json:"state"`
		ConfigSHA256 *string `json:"configSha256,omitempty"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		return fmt.Errorf("expect is neither the entry's state nor {state, configSha256}: %w", err)
	}
	e.State, e.ConfigSHA256 = o.State, o.ConfigSHA256
	return nil
}

// MarshalJSON writes it back as it reads best: the state alone when that is
// all there is.
func (e InflightExpect) MarshalJSON() ([]byte, error) {
	if e.ConfigSHA256 == nil {
		return json.Marshal(e.State)
	}
	return json.Marshal(struct {
		State        string  `json:"state"`
		ConfigSHA256 *string `json:"configSha256"`
	}{e.State, e.ConfigSHA256})
}

// InflightResult is the inflight command's answer.
type InflightResult struct {
	// Entry is the entry as read: state, number, counter, folder, frozen
	// rule, age, and which candidate files exist.
	Entry   *hasavshevet.InflightEntry `json:"entry"`
	Resolve string                     `json:"resolve,omitempty"`
	// AcceptHistoryOnly: done stands on the history copy alone, as a person
	// confirmed (review R6-1) — the approval form shows it.
	AcceptHistoryOnly bool `json:"acceptHistoryOnly,omitempty"`
	// Resolved is the entry as saved (a run that resolved it).
	Resolved *hasavshevet.InflightEntry `json:"resolved,omitempty"`
	// B2B is what the B2B shop sees once it is resolved.
	B2B *InflightB2B `json:"b2b,omitempty"`
}

// InflightB2B is what the B2B sees afterwards: on a status poll, and on a
// resend with the same historyId (orders) or idempotency key (receipts).
type InflightB2B struct {
	StatusPoll string `json:"statusPoll"`
	Resend     string `json:"resend"`
}

// DecodeInflight reads one JSON input, strictly like DecodeSpec.
func DecodeInflight(r io.Reader) (InflightInput, *Refusal) {
	var in InflightInput
	if err := decodeStrict(r, &in); err != nil {
		return InflightInput{}, refuse(CodeSpecInvalid, "send one JSON object with only the documented fields", "the inflight input does not decode: %v", err)
	}
	return in, nil
}

// maxHistoryID bounds an order's key on input (the routes' own bound is the
// B2B's; the index takes any, hashed).
const maxHistoryID = 256

// Validate checks the input before anything is read; a run (check false)
// needs expect and resolve.
func (in InflightInput) Validate(check bool) *Refusal {
	var e *Expect
	if in.Expect != nil && in.Expect.ConfigSHA256 != nil {
		e = &Expect{ConfigSHA256: in.Expect.ConfigSHA256}
	}
	if r := validateHeader(in.Version, in.ChangeSet, e); r != nil {
		return r
	}
	switch in.Kind {
	case hasavshevet.InflightOrders:
		if in.Number != 0 {
			return refuse(CodeSpecInvalid, "name an order by its historyId only", "number is a receipt's; an order is named by historyId")
		}
		if strings.TrimSpace(in.HistoryID) == "" || len(in.HistoryID) > maxHistoryID || hasControl(in.HistoryID) {
			return refuse(CodeSpecInvalid, fmt.Sprintf("give the order's historyId (at most %d characters)", maxHistoryID), "historyId %q is not an order's key", in.HistoryID)
		}
	case hasavshevet.InflightReceipts:
		if in.HistoryID != "" {
			return refuse(CodeSpecInvalid, "name a receipt by its number only", "historyId is an order's; a receipt is named by number")
		}
		if in.Number < 1 || in.Number > 999_999_999 {
			return refuse(CodeSpecInvalid, "give the receipt's number (1-999999999)", "number %d is not a receipt number", in.Number)
		}
	default:
		return refuse(CodeSpecInvalid, `use "orders" or "receipts"`, "kind %q is neither", in.Kind)
	}
	switch in.Resolve {
	case hasavshevet.ResolveFail, hasavshevet.ResolveDone:
	case "":
		if !check {
			return refuse(CodeSpecInvalid, `say what to do: "resolve": "fail" or "done"`, "resolve is missing")
		}
	default:
		return refuse(CodeSpecInvalid, `use "fail" or "done"`, "resolve %q is neither", in.Resolve)
	}
	if in.AcceptHistoryOnly && (in.Kind != hasavshevet.InflightOrders || in.Resolve != hasavshevet.ResolveDone) {
		return refuse(CodeSpecInvalid, "leave acceptHistoryOnly out: it is an order's done only — a receipt with only its history copy is delivered by Recover",
			"acceptHistoryOnly with kind %s, resolve %q", in.Kind, in.Resolve)
	}
	if in.Resolve != "" {
		if in.Expect == nil {
			return refuse(CodeSpecInvalid, "send expect: the entry's state as the check read it", "expect is missing")
		}
		ok := in.Expect.State == "reserved" || (in.Kind == hasavshevet.InflightOrders && in.Expect.State == "retryable")
		if !ok {
			return refuse(CodeSpecInvalid, "expect the state the check read: reserved, or retryable (an order)",
				"expect %q is not an unfinished %s's state", in.Expect.State, strings.TrimSuffix(in.Kind, "s"))
		}
	}
	return nil
}

// ApplyInflight runs the inflight command (check: --check, nothing written).
func ApplyInflight(ctx context.Context, env Env, in InflightInput, check bool) (*ApplyResult, error) {
	res := &ApplyResult{ChangeSet: in.ChangeSet, Steps: []Step{}}
	if r := in.Validate(check); r != nil {
		return res, r
	}
	var e *Expect
	if in.Expect != nil && in.Expect.ConfigSHA256 != nil {
		e = &Expect{ConfigSHA256: in.Expect.ConfigSHA256}
	}
	a := &applier{env: env, s: Spec{Version: in.Version, ChangeSet: in.ChangeSet, AssumeIdle: in.AssumeIdle, Expect: e},
		dir: env.Sys.DataDir(), res: res, check: check}
	unlock, err := lockRuns(a.dir)
	if err != nil {
		return res, err
	}
	defer unlock()
	if err := a.readCurrent(); err != nil {
		return res, err
	}
	if a.curBytes == nil {
		return res, refuse(CodeConfigRefused, "provision the connector with apply first", "there is no config.yaml: there is no order or receipt to resolve")
	}
	a.tgt = a.cur
	err = a.inflight(ctx, in)
	a.settle()
	return res, err
}

// inflightRefusal maps hasavshevet's inflight errors to the command's codes.
func inflightRefusal(err error) error {
	var r *Refusal
	switch {
	case err == nil:
		return nil
	case errors.As(err, &r):
		return r
	case errors.Is(err, hasavshevet.ErrInflightMissing):
		return refuse(CodeEntryMissing, "read the historyId or the number again (state, the B2B's log)", "%v", err)
	case errors.Is(err, hasavshevet.ErrInflightMoved):
		return refuse(CodeEntryMoved, "check the entry again (inflight-check) and make a new plan", "%v", err)
	case errors.Is(err, hasavshevet.ErrInflightFinal):
		return refuse(CodeEntryFinal, "nothing to do: a written or failed entry no longer holds anything back", "%v", err)
	case errors.Is(err, hasavshevet.ErrInflightMayBeWritten):
		return refuse(CodeMayBeWritten, "if Hashavshevet has the document, resolve it as done; else move the file aside after checking it, then run it again", "%v", err)
	case errors.Is(err, hasavshevet.ErrInflightNotWritten):
		return refuse(CodeNotWritten, "resolve it as failed, or let it be written (a repeat of its key)", "%v", err)
	case errors.Is(err, hasavshevet.ErrInflightHistoryMissing):
		return refuse(CodeHistoryMissing, "check Hashavshevet (the document's number) and the folder by hand: without its history copy nothing shows the file is the connector's — never resolve on it from afar", "%v", err)
	case errors.Is(err, hasavshevet.ErrRecoverDelivers):
		return refuse(CodeHistoryOnly, "let Recover deliver it: once its folder is allowed and its counter's range holds it, the next start (or a repeat of its key) "+
			"writes the receipt from its stored request — a receipt is never marked written on its history copy", "%v", err)
	case errors.Is(err, hasavshevet.ErrInflightHistoryOnly):
		return refuse(CodeHistoryOnly, "check Hashavshevet (Asmahta n): if it has the document, resolve done with acceptHistoryOnly: true (the form shows it); "+
			"if not, it was never delivered — leave it, or move the history copy aside and resolve it as failed", "%v", err)
	case errors.Is(err, hasavshevet.ErrOrderIndexUnreadable), errors.Is(err, hasavshevet.ErrReceiptIndexUnreadable):
		return refuse(CodeIndexUnreadable, "repair or move the entry by hand (docs/troubleshooting.md § The order index); the logs name it", "%v", err)
	}
	return refuse(CodeWriteFailed, "run it elevated (as an administrator)", "%v", err)
}

func (a *applier) inflightRead(in InflightInput, now time.Time) (*hasavshevet.InflightEntry, error) {
	if in.Kind == hasavshevet.InflightOrders {
		return hasavshevet.InspectOrder(a.dir, a.cur, in.HistoryID, now)
	}
	return hasavshevet.InspectReceipt(a.dir, a.cur, in.Number, now)
}

func (a *applier) inflight(ctx context.Context, in InflightInput) error {
	now := a.env.Now()
	v, err := a.inflightRead(in, now)
	if err != nil {
		return inflightRefusal(err)
	}
	out := &InflightResult{Entry: v, Resolve: in.Resolve, AcceptHistoryOnly: in.AcceptHistoryOnly}
	a.res.Inflight = out
	if in.Resolve != "" {
		out.B2B = inflightB2B(v, in.Resolve)
	}
	// An order the running worker has queued or is writing.
	if in.Kind == hasavshevet.InflightOrders && a.running {
		switch status, asked := a.jobStatus(ctx, v.Number); {
		case status == "queued" || status == "running":
			return refuse(CodeEntryBusy, "wait until the worker has written it (a status poll), then check it again",
				"order %d is %s in the running connector", v.Number, status)
		case !asked && in.Resolve != "":
			a.warn("the running connector's order %d could not be asked about; the queue is asked before anything is written", v.Number)
		}
	}
	if in.Resolve == "" {
		if a.check {
			a.res.Check = &CheckResult{Config: "unchanged", Restart: false}
		}
		return nil
	}
	if v.State != in.Expect.State {
		return refuse(CodeEntryMoved, "check the entry again (inflight-check) and make a new plan",
			"%s %d is %s, the plan expected %s", strings.TrimSuffix(in.Kind, "s"), v.Number, v.State, in.Expect.State)
	}
	if err := hasavshevet.CheckResolution(v, in.Resolve, in.AcceptHistoryOnly); err != nil {
		return inflightRefusal(err)
	}
	if a.check {
		a.res.Check = &CheckResult{Config: "unchanged", Restart: a.running}
		return nil
	}

	// The service runs: ask its queue, stop it (no worker holds the entry
	// then), resolve, start it again and prove it.
	wasRunning := a.running
	if wasRunning {
		if err := a.idle(ctx, "before the entry is resolved"); err != nil {
			return err
		}
		if err := a.env.Sys.StopService(ServiceName); err != nil {
			return refuse(CodeServiceFailed, "stop it in services.msc and run it again", "stop the service: %v", err)
		}
		a.touched = true
		a.step("stop", true, "")
	}
	resolved, resolveErr := a.inflightResolve(in, now)
	if wasRunning {
		if err := a.env.Sys.StartService(ServiceName); err != nil {
			if resolveErr != nil {
				return resolveErr
			}
			return refuse(CodeServiceFailed, "read server.log (the logs command) for why it stopped", "the entry was resolved, but the service did not start: %v", err)
		}
		a.res.Restarted = true
		a.step("start", true, "")
	}
	if resolveErr != nil {
		if wasRunning {
			_ = a.proveAfter(ctx)
		}
		return resolveErr
	}
	out.Resolved = resolved
	out.B2B = inflightB2B(resolved, in.Resolve)
	a.wrote = true
	a.step("inflight", true, fmt.Sprintf("%s %d (key %s) resolved as %s", strings.TrimSuffix(in.Kind, "s"), resolved.Number, resolved.Key, resolved.State))
	if wasRunning {
		return a.proveAfter(ctx)
	}
	return nil
}

// inflightResolve resolves the entry, a receipt under the receipt writer's
// lock (a process still holding it is entry_busy).
func (a *applier) inflightResolve(in InflightInput, now time.Time) (*hasavshevet.InflightEntry, error) {
	if in.Kind == hasavshevet.InflightOrders {
		v, err := hasavshevet.ResolveOrder(a.dir, a.cur, in.HistoryID, in.Expect.State, in.Resolve, in.ChangeSet, in.AcceptHistoryOnly, now)
		return v, inflightRefusal(err)
	}
	path := filepath.Join(a.dir, "receipts", "writer.lock")
	held, err := filelock.Lock(path)
	if errors.Is(err, filelock.ErrLocked) {
		return nil, refuse(CodeEntryBusy, "stop the connector (daemon-stop) and run it again", "another process holds the receipt data (%s): the connector is still running", path)
	}
	if err != nil {
		return nil, refuse(CodeWriteFailed, "run it elevated (as an administrator)", "lock the receipt data: %v", err)
	}
	defer func() { _ = held.Unlock() }()
	v, err := hasavshevet.ResolveReceipt(a.dir, a.cur, in.Number, in.Expect.State, in.Resolve, in.ChangeSet, now)
	return v, inflightRefusal(err)
}

// jobStatus asks the running connector about order n (GET
// /api/sendOrder/{n}): its status, and whether it could be asked.
func (a *applier) jobStatus(ctx context.Context, n int64) (string, bool) {
	d := a.env.daemonFor(a.cur)
	tok, err := d.login(ctx, a.cur.Auth.Username, a.cur.Auth.Password)
	if err != nil {
		return "", false
	}
	var job struct {
		Status string `json:"status"`
	}
	status, err := d.get(ctx, "/api/sendOrder/"+strconv.FormatInt(n, 10), tok, &job)
	switch {
	case err != nil:
		return "", false
	case status == http.StatusNotFound:
		return "", true
	case status != http.StatusOK:
		return "", false
	}
	return job.Status, true
}

// inflightB2B says what the B2B sees once e is resolved as resolve.
func inflightB2B(e *hasavshevet.InflightEntry, resolve string) *InflightB2B {
	n := strconv.FormatInt(e.Number, 10)
	switch {
	case e.Kind == hasavshevet.InflightOrders && resolve == hasavshevet.ResolveFail:
		return &InflightB2B{
			StatusPoll: "GET /api/sendOrder/" + n + ": 200, status failed (error \"order processing failed\")",
			Resend: "POST /api/sendOrder or /api/v2/sendOrder with historyId " + strconv.Quote(e.Key) + ": 202, status failed, replay true — never written again; " +
				"a new order needs a new historyId",
		}
	case e.Kind == hasavshevet.InflightOrders:
		return &InflightB2B{
			StatusPoll: "GET /api/sendOrder/" + n + ": 200, status done, writtenFiles the files found",
			Resend:     "POST /api/sendOrder or /api/v2/sendOrder with historyId " + strconv.Quote(e.Key) + ": 202, status done, replay true — nothing written again",
		}
	case resolve == hasavshevet.ResolveFail:
		return &InflightB2B{
			StatusPoll: "receipts have no status route",
			Resend:     "POST /api/sendReceipt with idempotencyKey " + e.Key + ": 409 RECEIPT_FAILED — receipt " + n + " is void, never written; a new receipt needs a new idempotencyKey",
		}
	}
	return &InflightB2B{
		StatusPoll: "receipts have no status route",
		Resend: "POST /api/sendReceipt with idempotencyKey " + e.Key + ": 200, replayed, receipt " + n + ", import not_run, the resolution among its warnings — " +
			"nothing written again",
	}
}
