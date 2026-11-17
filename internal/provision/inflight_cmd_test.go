package provision

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/filelock"
)

func inflightIn(f func(*InflightInput)) InflightInput {
	in := InflightInput{Version: 1, ChangeSet: "cs-inflight-01", Kind: "orders", HistoryID: "H-1"}
	if f != nil {
		f(&in)
	}
	return in
}

func expectState(s string) *InflightExpect { return &InflightExpect{State: s} }

// writeOrderEntry saves an order index entry as the daemon would (reserved:
// its outcome never recorded).
func writeOrderEntry(t *testing.T, dataDir, key string, n int64, state string) {
	t.Helper()
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
	dir := hasavshevet.OrderIndexDir(dataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"key": key, "number": n, "state": state, "reservedAt": "2026-11-16T10:00:00Z"})
	if err := os.WriteFile(filepath.Join(dir, sum+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Plan 087 review R6: the inflight command. The check shows the entry
// (state, number, counter, folder, age, the candidate files) and writes
// nothing; a run asks the queue, stops the service, resolves, starts it
// again and proves it; the refusals.
func TestInflight_Command(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	env := testEnv(sys, fdb)
	writeOrderEntry(t, sys.dir, "H-1", 1001, "reserved")

	chk, err := ApplyInflight(context.Background(), env, inflightIn(nil), true)
	if err != nil || chk.Inflight == nil || chk.Inflight.Entry.State != "reserved" || chk.Inflight.Entry.Number != 1001 || len(chk.Inflight.Entry.Candidates) == 0 {
		t.Fatalf("--check %+v %v", chk.Inflight, err)
	}
	if slices.Contains(sys.calls, "stop") {
		t.Fatal("the check stopped the service")
	}

	for name, c := range map[string]struct {
		in   InflightInput
		code string
	}{
		"unknown key": {inflightIn(func(in *InflightInput) {
			in.HistoryID = "nope"
			in.Expect, in.Resolve = expectState("reserved"), "fail"
		}), CodeEntryMissing},
		"moved":               {inflightIn(func(in *InflightInput) { in.Expect, in.Resolve = expectState("retryable"), "fail" }), CodeEntryMoved},
		"done without a file": {inflightIn(func(in *InflightInput) { in.Expect, in.Resolve = expectState("reserved"), "done" }), CodeNotWritten},
		"no resolve":          {inflightIn(func(in *InflightInput) { in.Expect = expectState("reserved") }), CodeSpecInvalid},
		"a receipt by key":    {inflightIn(func(in *InflightInput) { in.Kind = "receipts"; in.Resolve, in.Expect = "fail", expectState("reserved") }), CodeSpecInvalid},
		"expect written":      {inflightIn(func(in *InflightInput) { in.Expect, in.Resolve = expectState("written"), "fail" }), CodeSpecInvalid},
	} {
		_, err := ApplyInflight(context.Background(), env, c.in, false)
		if r := (*Refusal)(nil); !errors.As(err, &r) || r.Code != c.code {
			t.Errorf("%s: %v, want %s", name, err, c.code)
		}
	}

	// The history copy is there: fail is refused, done names it.
	hist := filepath.Join(orders, "history", "1001", "IMOVEIN_1001.doc")
	if err := os.MkdirAll(filepath.Dir(hist), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hist, []byte("doc"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = ApplyInflight(context.Background(), env, inflightIn(func(in *InflightInput) { in.Expect, in.Resolve = expectState("reserved"), "fail" }), true)
	wantRefusal(t, err, CodeMayBeWritten)

	// The running worker writes it: entry_busy, before anything is stopped.
	sys.daemon.set(func(d *fakeDaemon) { d.jobs = map[string]string{"1001": "running"} })
	sys.calls = nil
	_, err = ApplyInflight(context.Background(), env, inflightIn(func(in *InflightInput) { in.Expect, in.Resolve = expectState("reserved"), "done" }), false)
	if r := wantRefusal(t, err, CodeEntryBusy); slices.Contains(sys.calls, "stop") {
		t.Errorf("stopped: %v (%v)", sys.calls, r)
	}
	sys.daemon.set(func(d *fakeDaemon) { d.jobs = nil })

	// Review R6-1: the history copy alone is history_only (model (b) here);
	// acceptHistoryOnly — an order's done only — is named by the check.
	done := func(accept bool) InflightInput {
		return inflightIn(func(in *InflightInput) {
			in.Expect, in.Resolve, in.AcceptHistoryOnly = expectState("reserved"), "done", accept
		})
	}
	_, err = ApplyInflight(context.Background(), env, done(false), true)
	wantRefusal(t, err, CodeHistoryOnly)
	chk, err = ApplyInflight(context.Background(), env, done(true), true)
	if err != nil || !chk.Inflight.AcceptHistoryOnly || chk.Inflight.Entry.Proof != "history-only" {
		t.Fatalf("--check with acceptHistoryOnly: %+v %v", chk.Inflight, err)
	}
	_, err = ApplyInflight(context.Background(), env, inflightIn(func(in *InflightInput) {
		in.Expect, in.Resolve, in.AcceptHistoryOnly = expectState("reserved"), "fail", true
	}), true)
	wantRefusal(t, err, CodeSpecInvalid)

	sys.calls = nil
	res, err := ApplyInflight(context.Background(), env, done(true), false)
	if err != nil || res.Inflight.Resolved == nil || res.Inflight.Resolved.State != "written" || !res.Restarted || res.Proof == nil {
		t.Fatalf("done: %+v %v", res, err)
	}
	if got := strings.Join(sys.calls, " "); !strings.Contains(got, "stop") || strings.Index(got, "stop") > strings.Index(got, "start") {
		t.Errorf("calls %q: stopped, resolved, started", got)
	}
	if b := res.Inflight.B2B; b == nil || !strings.Contains(b.StatusPoll, "status done") || !strings.Contains(b.Resend, "replay true") {
		t.Errorf("b2b %+v", b)
	}
	_, err = ApplyInflight(context.Background(), env, done(true), false)
	wantRefusal(t, err, CodeEntryMoved) // written now
	_, err = ApplyInflight(context.Background(), env, inflightIn(func(in *InflightInput) { in.Expect, in.Resolve = expectState("written"), "done" }), false)
	wantRefusal(t, err, CodeSpecInvalid)

	// The command line and expect as an object.
	out, code := Run(context.Background(), env, []string{"inflight", "--check", "--spec", "-"},
		strings.NewReader(`{"version":1,"changeSet":"cs-inflight-02","kind":"orders","historyId":"H-1","expect":{"state":"written"}}`))
	if code != ExitOK || !strings.Contains(string(out), `"inflight"`) {
		t.Errorf("--check: %d %s", code, out)
	}
}

// Review R6 for receipts: a reserved receipt resolved as failed while no
// process holds the receipt data; one that does is entry_busy.
func TestInflight_CommandReceipt(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	env := testEnv(sys, fdb)
	cfg, _ := readConfig(t, sys.dir)
	cfg.SendReceiptDir = mkdir(t, "kupain")
	writeConfig(t, sys.dir, cfg)
	key := "00000000-0000-4000-8000-000000000001"
	ix := filepath.Join(sys.dir, "receipts", "index")
	if err := os.MkdirAll(ix, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"key": key, "number": 500, "state": "reserved", "request": map[string]any{}, "files": nil, "import": "not_run", "reservedAt": "2026-11-16T10:00:00Z"})
	if err := os.WriteFile(filepath.Join(ix, key+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	in := InflightInput{Version: 1, ChangeSet: "cs-inflight-03", Kind: "receipts", Number: 500, Expect: expectState("reserved"), Resolve: "fail"}

	held, err := filelock.Lock(filepath.Join(sys.dir, "receipts", "writer.lock"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = ApplyInflight(context.Background(), env, in, false)
	wantRefusal(t, err, CodeEntryBusy)
	_ = held.Unlock()

	// A receipt is never resolved on its history copy: Recover delivers it.
	hist := filepath.Join(sys.dir, "history", "receipts", "500", "KUPAIN_500.doc")
	if err := os.MkdirAll(filepath.Dir(hist), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hist, []byte("receipt 500"), 0o600); err != nil {
		t.Fatal(err)
	}
	doneIn := in
	doneIn.Resolve = "done"
	_, err = ApplyInflight(context.Background(), env, doneIn, true)
	if r := wantRefusal(t, err, CodeHistoryOnly); !strings.Contains(r.Fix, "Recover") {
		t.Errorf("fix %q", r.Fix)
	}
	doneIn.AcceptHistoryOnly = true
	_, err = ApplyInflight(context.Background(), env, doneIn, true)
	wantRefusal(t, err, CodeSpecInvalid)
	if err := os.Remove(hist); err != nil {
		t.Fatal(err)
	}

	res, err := ApplyInflight(context.Background(), env, in, false)
	if err != nil || res.Inflight.Resolved == nil || res.Inflight.Resolved.State != "failed" || !strings.Contains(res.Inflight.B2B.Resend, "409 RECEIPT_FAILED") {
		t.Fatalf("fail: %+v %v", res, err)
	}
}

// Re-review MINOR 1: a delivered file with no readable history copy is
// history_missing, its fix a check by hand.
func TestInflight_HistoryMissingCode(t *testing.T) {
	err := inflightRefusal(fmt.Errorf("%w: order 7", hasavshevet.ErrInflightHistoryMissing))
	if r := wantRefusal(t, err, CodeHistoryMissing); !strings.Contains(r.Fix, "by hand") {
		t.Errorf("fix %q", r.Fix)
	}
}

// Re-review MINOR 2: acceptHistoryOnly outside model (b) is history_only,
// its fix the importer's way (not acceptHistoryOnly again).
func TestInflight_DeliveryLeavesFileCode(t *testing.T) {
	err := inflightRefusal(fmt.Errorf("%w (%w): order 7", hasavshevet.ErrInflightHistoryOnly, hasavshevet.ErrDeliveryLeavesFile))
	if r := wantRefusal(t, err, CodeHistoryOnly); !strings.Contains(r.Fix, "model (b) orders only") {
		t.Errorf("fix %q", r.Fix)
	}
}
