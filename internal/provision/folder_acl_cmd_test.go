package provision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
)

// aclBox is the counters box with a typed folder herp that Users can write to.
func aclBox(t *testing.T) (*fakeSys, Env, string, string) {
	t.Helper()
	sys, env, orders := countersBox(t)
	herp := mkdir(t, "herp")
	cfg, _ := readConfig(t, sys.dir)
	cfg.Documents.Folders = map[string]config.FolderConfig{"herp": {Kind: "orders", Path: herp, NamePattern: "IMOVEIN_{n}.doc", Layout: "legacy"}}
	writeConfig(t, sys.dir, cfg)
	sys.writers = map[string][]string{herp: {usersWrite}}
	sys.parentWriters = map[string][]dataacl.Writer{herp: {{SID: "S-1-5-32-545", Name: usersWrite}}}
	return sys, env, orders, herp
}

func aclIn(op string, f func(*FolderACLInput)) FolderACLInput {
	in := FolderACLInput{Version: 1, ChangeSet: "cs-acl-0001", Op: op}
	if f != nil {
		f(&in)
	}
	return in
}

// Plan 087 review R1c: --check shows every principal before and after and
// writes nothing; a lock-down records the pre-image first (a protected file
// named by its sha256, answered), writes, restarts and proves; a restore
// takes only that pre-image, for that folder, and puts it back.
func TestFolderACL_LockdownAndRestore(t *testing.T) {
	sys, env, _, herp := aclBox(t)
	lock := aclIn("lockdown", func(in *FolderACLInput) { in.Folders = []string{"herp", "sendOrderDir"} })

	chk, err := ApplyFolderACL(context.Background(), env, lock, true)
	if err != nil || chk.FolderACL == nil || len(chk.FolderACL.Folders) != 2 {
		t.Fatalf("--check: %+v %v", chk, err)
	}
	h := chk.FolderACL.Folders[0]
	if h.Name != "herp" || h.Path != herp || h.Entries != 2 || h.EntriesChanged != 2 || len(h.Principals) == 0 || h.Principals[0].After != "read" ||
		h.Principals[0].Entries != 2 || len(h.Owners) != 1 || !slices.Equal(h.ParentWritable, []string{usersWrite}) || h.Preimage != "" {
		t.Errorf("--check herp %+v", h)
	}
	if !warned(chk, "parent_writable: herp") {
		t.Errorf("warnings %v", chk.Warnings)
	}
	if slices.ContainsFunc(sys.calls, func(c string) bool { return strings.HasPrefix(c, "lockdown:") }) {
		t.Fatalf("--check wrote: %v", sys.calls)
	}
	if _, err := os.Stat(folderACLDir(sys.dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("--check recorded a pre-image")
	}

	sys.calls = nil
	res, err := ApplyFolderACL(context.Background(), env, lock, false)
	if err != nil || !res.Restarted || res.Proof == nil {
		t.Fatalf("lockdown: %+v %v", res, err)
	}
	sha := res.FolderACL.Folders[0].Preimage
	b, err := os.ReadFile(filepath.Join(folderACLDir(sys.dir), sha+".json"))
	if err != nil {
		t.Fatalf("the pre-image %s: %v", sha, err)
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != sha || !strings.Contains(string(b), "O:S-1-5-21-0-0-0-1001D:AI") {
		t.Fatalf("the pre-image is not addressed by its sha256, or lacks the previous descriptor: %s", b)
	}
	at := func(c string) int { return slices.Index(sys.calls, c) }
	if at("lockdown:herp") < 0 || at("lockdown:digiorders") < 0 || at("protect") > at("lockdown:herp") || at("lockdown:digiorders") > at("stop") || at("stop") > at("start") {
		t.Errorf("calls %v: protect, record, lock down, then restart", sys.calls)
	}
	if st, _ := State(context.Background(), env, StateInput{}); slices.ContainsFunc(st.WritableFolders, func(w StateWritableFolder) bool { return w.Path == herp }) {
		t.Error("herp is still writable after the lock-down")
	}

	// Restore: only a pre-image recorded for this folder.
	other := res.FolderACL.Folders[1].Preimage
	for name, in := range map[string]FolderACLInput{
		"another folder's":   aclIn("restore", func(in *FolderACLInput) { in.Folder, in.Preimage = "herp", other }),
		"one never recorded": aclIn("restore", func(in *FolderACLInput) { in.Folder, in.Preimage = "herp", strings.Repeat("a", 64) }),
	} {
		_, err := ApplyFolderACL(context.Background(), env, in, false)
		if r := (*Refusal)(nil); !errors.As(err, &r) || r.Code != CodePreimageMissing {
			t.Errorf("%s pre-image: %v", name, err)
		}
	}
	sys.calls = nil
	res, err = ApplyFolderACL(context.Background(), env, aclIn("restore", func(in *FolderACLInput) { in.Folder, in.Preimage = "herp", sha }), false)
	if err != nil || res.FolderACL.Folders[0].Restored != 2 || !slices.Contains(sys.calls, "restore-acl:herp") || !res.Restarted {
		t.Fatalf("restore: %+v %v (calls %v)", res, err, sys.calls)
	}
	if sys.folderSD[herp] != "O:S-1-5-21-0-0-0-1001D:AI(A;OICIID;0x1301bf;;;BU)" {
		t.Errorf("restored to %q", sys.folderSD[herp])
	}
	// A pre-image changed on disk no longer matches its sha256.
	if err := os.WriteFile(filepath.Join(folderACLDir(sys.dir), sha+".json"), append(b, ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = ApplyFolderACL(context.Background(), env, aclIn("restore", func(in *FolderACLInput) { in.Folder, in.Preimage = "herp", sha }), false)
	if r := wantRefusal(t, err, CodePreimageMissing); !strings.Contains(r.Message, "sha256") {
		t.Errorf("a changed pre-image: %+v", r)
	}
}

func TestFolderACL_Refusals(t *testing.T) {
	sys, env, _, herp := aclBox(t)
	lock := func(names ...string) FolderACLInput {
		return aclIn("lockdown", func(in *FolderACLInput) { in.Folders = names })
	}
	for name, tc := range map[string]struct {
		in   FolderACLInput
		code string
	}{
		"a path":        {lock(`C:\xampp\htdocs\herp`), CodeSpecInvalid},
		"twice":         {lock("herp", "herp"), CodeSpecInvalid},
		"nine":          {lock("a", "b", "c", "d", "e", "f", "g", "h", "i"), CodeSpecInvalid},
		"none":          {lock(), CodeSpecInvalid},
		"an unknown op": {aclIn("open", nil), CodeSpecInvalid},
		"restore with folders": {aclIn("restore", func(in *FolderACLInput) {
			in.Folders, in.Folder, in.Preimage = []string{"herp"}, "herp", strings.Repeat("a", 64)
		}), CodeSpecInvalid},
		"a bad sha":         {aclIn("restore", func(in *FolderACLInput) { in.Folder, in.Preimage = "herp", "abc" }), CodeSpecInvalid},
		"an unknown folder": {lock("nope"), CodeFolderUnknown},
		"no pickup folder":  {lock("pickup"), CodeFolderUnknown},
		"no sendReceiptDir": {lock("sendReceiptDir"), CodeFolderUnknown},
	} {
		_, err := ApplyFolderACL(context.Background(), env, tc.in, false)
		if r := (*Refusal)(nil); !errors.As(err, &r) || r.Code != tc.code {
			t.Errorf("%s: %v, want %s", name, err, tc.code)
		}
	}

	sys.lockdownErr = map[string]error{herp: fmt.Errorf("%w: %s\\zz-link", dataacl.ErrReparsePoint, herp)}
	_, err := ApplyFolderACL(context.Background(), env, lock("herp"), false)
	wantRefusal(t, err, CodeFolderReparse)
	sys.lockdownErr = map[string]error{herp: fmt.Errorf("%w: 20001", dataacl.ErrTooManyEntries)}
	_, err = ApplyFolderACL(context.Background(), env, lock("herp"), false)
	wantRefusal(t, err, CodeFolderTooLarge)
	sys.lockdownErr = nil

	// Re-review MINOR 3: unfinished index entries — an order frozen for
	// herp, a rule-less one for sendOrderDir — do not hold a lock-down back
	// (it changes none of SYSTEM's rights, and they are not in the running
	// connector's memory); an order the running connector holds does: the
	// restart after it would lose it (orders_pending, nothing written).
	ix := hasavshevet.OrderIndexDir(sys.dir)
	if err := os.MkdirAll(ix, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ix, "e.json"), []byte(`{"key":"k","number":1000300,"state":"retryable","reservedAt":"x","rule":{"counter":"b2b","folder":"herp","temporary":false,"fileName":"IMOVEIN_1000300.doc","counterTest":false,"folderTest":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ix, "p.json"), []byte(`{"key":"p","number":1000301,"state":"reserved","reservedAt":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sys.daemon.set(func(d *fakeDaemon) { d.queued = 1 })
	sys.calls = nil
	_, err = ApplyFolderACL(context.Background(), env, lock("herp"), false)
	wantRefusal(t, err, CodeOrdersPending)
	if slices.ContainsFunc(sys.calls, func(c string) bool { return strings.HasPrefix(c, "lockdown:") }) {
		t.Fatalf("a refusal wrote: %v", sys.calls)
	}
	sys.daemon.set(func(d *fakeDaemon) { d.queued = 0 })
	for _, n := range []string{"herp", "sendOrderDir"} {
		if _, err = ApplyFolderACL(context.Background(), env, lock(n), true); err != nil {
			t.Errorf("--check lock-down of %s with unfinished entries: %v", n, err)
		}
	}

	// A folder holding the data directory is never locked down.
	cfg, _ := readConfig(t, sys.dir)
	cfg.Documents.Folders["wide"] = config.FolderConfig{Kind: "orders", Path: filepath.Dir(sys.dir), NamePattern: "W_{n}.doc", Layout: "legacy"}
	writeConfig(t, sys.dir, cfg)
	_, err = ApplyFolderACL(context.Background(), env, lock("wide"), false)
	wantRefusal(t, err, CodeFolderForbidden)

	// The command line.
	out, code := Run(context.Background(), env, []string{"folder-acl", "--check", "--spec", "-"},
		strings.NewReader(`{"version":1,"changeSet":"cs-acl-0002","op":"lockdown","folders":["herp"],"path":"C:\\x"}`))
	if code != ExitRefused || !strings.Contains(string(out), CodeSpecInvalid) {
		t.Errorf("an unknown field: %d %s", code, out)
	}
}

// Review T1: folder-acl never locks down a folder code runs from — one that
// holds the importer's files or executables (PATHEXT, .dll, .ps1; never
// .php) — and trusts no one in a folder where an importer runs.
func TestFolderACL_ImporterFoldersAndExecutables(t *testing.T) {
	sys, env, orders, herp := aclBox(t)
	cfg, _ := readConfig(t, sys.dir)
	cfg.Documents.TrustedWriters = []config.TrustedWriter{{SID: "S-1-5-21-1-2-3-1105", Name: `BFL\wizsched`}}
	cfg.HasExePath = touch(t, filepath.Join(herp, "has.exe"))
	writeConfig(t, sys.dir, cfg)
	_, err := ApplyFolderACL(context.Background(), env, aclIn("lockdown", func(in *FolderACLInput) { in.Folders = []string{"herp"} }), true)
	if r := wantRefusal(t, err, CodeFolderHasExecutables); !strings.Contains(r.Message, "has.exe") {
		t.Errorf("refusal %+v", r)
	}

	// has.exe elsewhere: sendOrderDir is its working directory — no trusted writer there.
	cfg.HasExePath = touch(t, filepath.Join(mkdir(t, "Hash7"), "has.exe"))
	writeConfig(t, sys.dir, cfg)
	if _, err := ApplyFolderACL(context.Background(), env, aclIn("lockdown", func(in *FolderACLInput) { in.Folders = []string{"sendOrderDir"} }), true); err != nil {
		t.Fatal(err)
	}
	if len(sys.lastTrusted) != 0 {
		t.Errorf("trusted writers kept for has.exe's working directory %s: %v", orders, sys.lastTrusted)
	}
	if !sys.lastExecExts[".dll"] || !sys.lastExecExts[".ps1"] || !sys.lastExecExts[".exe"] || sys.lastExecExts[".php"] {
		t.Errorf("executable extensions %v", sys.lastExecExts)
	}
	// herp (no importer runs there): the trusted writer is kept.
	if _, err := ApplyFolderACL(context.Background(), env, aclIn("lockdown", func(in *FolderACLInput) { in.Folders = []string{"herp"} }), true); err != nil {
		t.Fatal(err)
	}
	if len(sys.lastTrusted) != 1 {
		t.Errorf("trusted writers for herp: %v", sys.lastTrusted)
	}
	// Review T2: the trusted writer has no entry of its own (the fake plan
	// lists only Users): it is told it keeps no write.
	res, err := ApplyFolderACL(context.Background(), env, aclIn("lockdown", func(in *FolderACLInput) { in.Folders = []string{"herp"} }), true)
	if err != nil || !warned(res, `trusted_writer_no_write: BFL\wizsched`) {
		t.Errorf("--check: %v %v", res.Warnings, err)
	}
	// An executable found by the walk.
	sys.executables = map[string][]string{herp: {`lib\wtsapi32.dll`}}
	_, err = ApplyFolderACL(context.Background(), env, aclIn("lockdown", func(in *FolderACLInput) { in.Folders = []string{"herp"} }), true)
	if r := wantRefusal(t, err, CodeFolderHasExecutables); !strings.Contains(r.Message, "wtsapi32.dll") {
		t.Errorf("refusal %+v", r)
	}
}

// Review T1: a trusted writer counts as a writer where an importer runs —
// the documents command refuses it, state lists it under writers.
func TestTrustedWriterCountsWhereAnImporterRuns(t *testing.T) {
	sys, env, _, _ := aclBox(t)
	kupain := mkdir(t, "kupain")
	cfg, _ := readConfig(t, sys.dir)
	cfg.Documents.TrustedWriters = []config.TrustedWriter{{SID: "S-1-5-21-1-2-3-1105", Name: `BFL\wizsched`}}
	cfg.HasExePath = touch(t, filepath.Join(mkdir(t, "Hash7"), "has.exe"))
	cfg.HasReceiptParamFile = touch(t, filepath.Join(mkdir(t, "Hash7p"), "kupain.prm"))
	writeConfig(t, sys.dir, cfg)
	sys.accounts = map[string]dataacl.Account{`bfl\wizsched`: {SID: "S-1-5-21-1-2-3-1105", Name: `BFL\wizsched`, Type: dataacl.AccountUser}}
	sys.writers = map[string][]string{kupain: {`BFL\wizsched`}}
	_, err := ApplyDocuments(context.Background(), env, docsIn(func(in *DocumentsInput) {
		in.Folders = map[string]*FolderInput{"herp_receipts": {Kind: "receipts", Path: kupain, NamePattern: "KUPAIN_{n}.doc"}}
		in.Receipts = &DocumentsReceiptsInput{Folder: strp("herp_receipts"), Counter: strp("receipts")}
	}), false)
	if r := wantRefusal(t, err, CodeFolderWritable); !strings.Contains(r.Message, `BFL\wizsched`) {
		t.Errorf("the bound receipt folder where has.exe runs: %+v", r)
	}
}

// Review T5: state lists the pre-images recorded on the box — the folder and
// path, the sha256 a restore takes, when, the change set and how many
// entries — so a lock-down killed before it answered can still be put back;
// a changed file is flagged, never listed as restorable.
func TestState_ListsFolderACLPreimages(t *testing.T) {
	sys, env, _, herp := aclBox(t)
	res, err := ApplyFolderACL(context.Background(), env, aclIn("lockdown", func(in *FolderACLInput) { in.Folders = []string{"herp"} }), false)
	if err != nil {
		t.Fatal(err)
	}
	sha := res.FolderACL.Folders[0].Preimage
	st, err := State(context.Background(), env, StateInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.FolderACL.Preimages) != 1 {
		t.Fatalf("preimages %+v", st.FolderACL)
	}
	p := st.FolderACL.Preimages[0]
	if p.Folder != "herp" || p.Path != herp || p.SHA256 != sha || p.At == "" || p.ChangeSet != "cs-acl-0001" || p.Entries != 2 || p.Error != "" {
		t.Errorf("preimage %+v", p)
	}
	b, _ := json.Marshal(st)
	if !strings.Contains(string(b), `"folderAcl":{"preimages":[{"folder":"herp"`) {
		t.Errorf("the wire shape: %s", b)
	}
	// A changed file is flagged.
	f := filepath.Join(sys.dir, "folder-acl", sha+".json")
	raw, _ := os.ReadFile(f)
	if err := os.WriteFile(f, append(raw, ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	st, _ = State(context.Background(), env, StateInput{})
	if len(st.FolderACL.Preimages) != 1 || st.FolderACL.Preimages[0].Error == "" || st.FolderACL.Preimages[0].Folder != "" {
		t.Errorf("a changed pre-image: %+v", st.FolderACL.Preimages)
	}
}

// v1.0.32: carryOver reaches the lock-down; the answer names the writers
// given back their write (carriedOver) and trusted_writer_no_write is gone
// for them; one whose groups do not resolve is carry_over_failed and keeps
// the warning; a restore takes no carryOver.
func TestFolderACL_CarryOver(t *testing.T) {
	sys, env, _, _ := aclBox(t)
	cfg, _ := readConfig(t, sys.dir)
	cfg.Documents.TrustedWriters = []config.TrustedWriter{{SID: "S-1-5-21-1-2-3-1105", Name: `BFL\wizsched`}, {SID: "S-1-5-21-1-2-3-1106", Name: `BFL\other`}}
	writeConfig(t, sys.dir, cfg)
	sys.carryFails = map[string]string{"S-1-5-21-1-2-3-1106": "the domain controller does not answer"}
	lock := aclIn("lockdown", func(in *FolderACLInput) { in.Folders = []string{"herp"}; in.CarryOver = true })
	res, err := ApplyFolderACL(context.Background(), env, lock, true)
	if err != nil || !sys.lastCarry {
		t.Fatalf("--check: %v (carry %v)", err, sys.lastCarry)
	}
	h := res.FolderACL.Folders[0]
	if len(h.CarriedOver) != 1 || h.CarriedOver[0].SID != "S-1-5-21-1-2-3-1105" || h.CarriedOver[0].Rights != "modify" || h.CarriedOver[0].Entries != 2 {
		t.Errorf("carriedOver %+v", h.CarriedOver)
	}
	if warned(res, `trusted_writer_no_write: BFL\wizsched`) || !warned(res, `trusted_writer_no_write: BFL\other`) || !warned(res, "carry_over_failed: S-1-5-21-1-2-3-1106") {
		t.Errorf("warnings %v", res.Warnings)
	}
	b, _ := json.Marshal(res)
	if !strings.Contains(string(b), `"carriedOver":[{"sid":"S-1-5-21-1-2-3-1105"`) {
		t.Errorf("the wire shape: %s", b)
	}
	// Review item 5: a carried entry not handed down to new files is said.
	sys.carryNotInheritable = map[string]int{"S-1-5-21-1-2-3-1105": 1}
	res, err = ApplyFolderACL(context.Background(), env, lock, true)
	if err != nil || !warned(res, "carried_not_inheritable: S-1-5-21-1-2-3-1105") || res.FolderACL.Folders[0].CarriedOver[0].NotInheritable != 1 {
		t.Errorf("not inheritable: %v %v", res.Warnings, err)
	}
	sys.carryNotInheritable = nil
	// Without it: today's lock-down.
	if _, err := ApplyFolderACL(context.Background(), env, aclIn("lockdown", func(in *FolderACLInput) { in.Folders = []string{"herp"} }), true); err != nil || sys.lastCarry {
		t.Errorf("without carryOver: %v (carry %v)", err, sys.lastCarry)
	}
	_, err = ApplyFolderACL(context.Background(), env, aclIn("restore", func(in *FolderACLInput) {
		in.Folder, in.Preimage, in.CarryOver = "herp", strings.Repeat("a", 64), true
	}), true)
	wantRefusal(t, err, CodeSpecInvalid)
}

// warnLog keeps what a component warns about (the folder guard, Recover).
type warnLog struct{ warns []string }

func (l *warnLog) Info(string)             {}
func (l *warnLog) Error(m string, _ error) { l.warns = append(l.warns, m) }
func (l *warnLog) Warn(m string)           { l.warns = append(l.warns, m) }
func (l *warnLog) Success(string)          {}
func (l *warnLog) Close() error            { return nil }

// Re-review MINOR 3, the receipt dead end: a receipt reserved for the bound
// receipt folder while the guard refuses that folder (others can write to
// it, behind the link). Recover cannot deliver it; folder-acl used to refuse
// the lock-down (folder_in_flight: a reserved receipt), inflight done
// answered RECOVER_DELIVERS and fail may_be_written — only a fix by hand on
// the box was left. Now the lock-down goes through (it changes none of
// SYSTEM's rights), and at the start after it the daemon's guard allows the
// folder and Recover delivers the receipt.
func TestFolderACL_LockdownLetsRecoverDeliverAReservedReceipt(t *testing.T) {
	sys, env, _ := countersBox(t)
	kupain := mkdir(t, "kupain")
	if _, err := ApplyDocuments(context.Background(), env, docsIn(func(in *DocumentsInput) {
		in.ChangeSet = "cs-minor3-d1"
		in.Folders = map[string]*FolderInput{"herp_receipts": {Kind: "receipts", Path: kupain, NamePattern: "KUPAIN_{n}.doc"}}
		in.Receipts = &DocumentsReceiptsInput{Folder: strp("herp_receipts"), Counter: strp("receipts")}
	}), false); err != nil {
		t.Fatal(err)
	}
	sys.writers = map[string][]string{kupain: {usersWrite}}
	cfg, _ := readConfig(t, sys.dir)

	// The receipt, reserved as the writer leaves it when the folder is
	// refused at its finishing (fake customer data).
	const key, number = "0b8f6a4e-3c1d-4e2f-9a7b-5c6d7e8f9a0b", 500_100
	req := hasavshevet.ReceiptRequest{IdempotencyKey: key, AccountKey: "100234", SettledDate: "2026-10-11", AmountMinor: 710700,
		Installments: 1, CardIssuer: "2", CardBrand: "2", Last4: "1234", ConfirmationCode: "0123456", ProviderUID: "26101112345678901234567",
		Remarks: "INV-1001", AgentKey: "17", Name: "Test Customer Ltd", Address: "1 Example St", Phone: "04-0000000", Email: "test@example.com"}
	b, err := json.Marshal(map[string]any{"key": key, "number": number, "state": "reserved", "request": req, "files": nil,
		"import": "not_run", "reservedAt": "2026-11-17T08:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	ix := filepath.Join(sys.dir, "receipts", "index")
	if err := os.MkdirAll(ix, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ix, key+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	// The store past it, as the reservation left it.
	if err := os.WriteFile(filepath.Join(sys.dir, "receipts", "receiptNumber.json"), []byte(fmt.Sprintf(`{"nextReceiptNumber": %d}`, number+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	delivered := filepath.Join(kupain, fmt.Sprintf("KUPAIN_%d.doc", number))

	// A daemon start: the guard reads the folders, then Recover runs.
	start := func() *warnLog {
		t.Helper()
		log := &warnLog{}
		cfg, _ := readConfig(t, sys.dir)
		guard := hasavshevet.NewFolderGuard(cfg, true, sys.FolderWriters, nil, log)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		guard.Run(ctx) // the first read of every folder, then it returns
		wc := hasavshevet.ReceiptWriterConfigFrom(cfg, sys.dir)
		wc.FolderCheck = guard.ReceiptCheck(cfg)
		w, err := hasavshevet.NewReceiptWriter(wc, log)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close() // the service stops before the next start
		if err := w.Recover(); err != nil {
			t.Fatalf("Recover: %v", err)
		}
		return log
	}

	log := start()
	if _, err := os.Stat(delivered); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused folder got the receipt: %v", err)
	}
	if !slices.ContainsFunc(log.warns, func(m string) bool { return strings.Contains(m, "reserved but not finished yet") }) {
		t.Fatalf("Recover did not say it waits for the folder: %v", log.warns)
	}
	if v, err := hasavshevet.InspectReceipt(sys.dir, cfg, number, time.Now()); err != nil || v.State != "reserved" {
		t.Fatalf("before the lock-down: %+v %v", v, err)
	}

	res, err := ApplyFolderACL(context.Background(), env, aclIn("lockdown", func(in *FolderACLInput) { in.Folders = []string{"herp_receipts"} }), false)
	if err != nil || len(res.FolderACL.Folders) != 1 || res.FolderACL.Folders[0].Written == 0 {
		t.Fatalf("lock-down with a reserved receipt in the folder: %+v %v", res, err)
	}

	start()
	if _, err := os.Stat(delivered); err != nil {
		t.Fatalf("Recover did not deliver the receipt after the lock-down: %v", err)
	}
	if v, err := hasavshevet.InspectReceipt(sys.dir, cfg, number, time.Now()); err != nil || v.State != "written" {
		t.Fatalf("after the lock-down and the start: %+v %v", v, err)
	}
}
