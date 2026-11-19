package provision

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

func mkdir(t *testing.T, parts ...string) string {
	t.Helper()
	d := filepath.Join(append([]string{t.TempDir()}, parts...)...)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	// A folder a human names is its long name: the CI runner's TempDir is
	// C:\Users\RUNNER~1\…, an 8.3 short name link mode refuses (review m6).
	if runtime.GOOS == "windows" {
		if long, err := filepath.EvalSymlinks(d); err == nil {
			d = long
		}
	}
	return d
}

func touch(t *testing.T, p string) string {
	t.Helper()
	if err := os.WriteFile(p, []byte("@echo off\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func receiptsInput(f func(*ReceiptsInput)) ReceiptsInput {
	in := ReceiptsInput{Version: 1, ChangeSet: "cs-receipts-01"}
	f(&in)
	return in
}

func TestApplyReceipts_SetsOnlyTheReceiptSettings(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	env := testEnv(sys, fdb)
	before, beforeBytes := readConfig(t, sys.dir)
	dir := mkdir(t, "kupain")
	bat := touch(t, filepath.Join(dir, "import_kupain.bat"))
	in := receiptsInput(func(in *ReceiptsInput) {
		in.SendReceiptDir, in.HasReceiptBatFile = &dir, &bat
		in.ReceiptCardNames = &config.ReceiptCardNamesConfig{Issuer: map[string]string{"1": "ישראכרט"}}
	})

	chk, err := ApplyReceipts(context.Background(), env, in, true)
	if err != nil || chk.Check == nil || chk.Check.Config != "changed: receipts" || !chk.Check.Restart {
		t.Fatalf("--check = %+v, %v", chk.Check, err)
	}
	res, err := ApplyReceipts(context.Background(), env, in, false)
	if err != nil {
		t.Fatalf("ApplyReceipts: %v", err)
	}
	after, _ := readConfig(t, sys.dir)
	want := before
	want.SendReceiptDir, want.HasReceiptBatFile = dir, bat
	want.ReceiptCardNames = *in.ReceiptCardNames
	wb, _ := config.Marshal(want)
	gb, _ := config.Marshal(after)
	if !bytes.Equal(wb, gb) {
		t.Errorf("config.yaml changed beyond the receipts:\n got %s\nwant %s", gb, wb)
	}
	if !res.Restarted || res.Proof == nil {
		t.Errorf("restarted %v, proof %+v", res.Restarted, res.Proof)
	}
	if _, err := Restore(context.Background(), env, RestoreInput{ChangeSet: "cs-receipts-01"}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, b := readConfig(t, sys.dir); !bytes.Equal(b, beforeBytes) {
		t.Error("restore did not put config.yaml back")
	}
}

func TestApplyReceipts_Refusals(t *testing.T) {
	dir := mkdir(t, "kupain")
	bat := touch(t, filepath.Join(dir, "import_kupain.bat"))
	exe := touch(t, filepath.Join(dir, "import.exe"))
	prm := touch(t, filepath.Join(dir, "kupain.prm"))
	missing := filepath.Join(dir, "nope")
	tests := []struct {
		name   string
		in     ReceiptsInput
		linked bool
		setup  func(*fakeSys, string)
		code   string
	}{
		{"nothing", receiptsInput(func(*ReceiptsInput) {}), false, nil, CodeSpecInvalid},
		{"a relative folder", receiptsInput(func(in *ReceiptsInput) { in.SendReceiptDir = ptr(`kupain`) }), false, nil, CodeSpecInvalid},
		{"a missing folder", receiptsInput(func(in *ReceiptsInput) { in.SendReceiptDir = &missing }), false, nil, CodeReceiptDirMissing},
		{"a missing BAT", receiptsInput(func(in *ReceiptsInput) { in.SendReceiptDir, in.HasReceiptBatFile = &dir, &missing }), false, nil, CodeImporterMissing},
		{"BAT and parameter file", receiptsInput(func(in *ReceiptsInput) {
			in.SendReceiptDir, in.HasReceiptBatFile, in.HasReceiptParamFile = &dir, &bat, &prm
		}),
			false, nil, CodeConfigRefused},
		{"the order folder", receiptsInput(func(in *ReceiptsInput) {}), false, nil, CodeConfigRefused}, // set in setup
		{"a BAT others can change", receiptsInput(func(in *ReceiptsInput) { in.SendReceiptDir, in.HasReceiptBatFile = &dir, &bat }), false,
			func(s *fakeSys, _ string) { s.writers = map[string][]string{bat: {`BUILTIN\Users`}} }, CodeImporterWritable},
		{"not a batch file behind the link", receiptsInput(func(in *ReceiptsInput) { in.SendReceiptDir, in.HasReceiptBatFile = &dir, &exe }), true, nil, CodeImporterNotERP},
		{"allowWritable behind the link", receiptsInput(func(in *ReceiptsInput) {
			in.SendReceiptDir, in.HasReceiptBatFile, in.AllowWritable = &dir, &bat, true
		}), true, nil, CodeImporterNotERP},
		{"orders pending", receiptsInput(func(in *ReceiptsInput) { in.SendReceiptDir = &dir }), false,
			func(s *fakeSys, _ string) { s.daemon.set(func(d *fakeDaemon) { d.queued = 1 }) }, CodeOrdersPending},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sys, fdb, orders := handInstallBox(t)
			if tc.linked {
				linkBox(t, sys)
			}
			if tc.setup != nil {
				tc.setup(sys, orders)
			}
			in := tc.in
			if tc.name == "the order folder" {
				in.SendReceiptDir = &orders
			}
			_, before := readConfig(t, sys.dir)
			res, err := ApplyReceipts(context.Background(), testEnv(sys, fdb), in, false)
			wantRefusal(t, err, tc.code)
			if _, after := readConfig(t, sys.dir); !bytes.Equal(before, after) || res.Restarted || res.PreimageCaptured {
				t.Errorf("work was done before the refusal: %+v", res)
			}
		})
	}
	t.Run("allowWritable over WinRM is a warning", func(t *testing.T) {
		sys, fdb, _ := handInstallBox(t)
		sys.writers = map[string][]string{bat: {`BUILTIN\Users`}}
		res, err := ApplyReceipts(context.Background(), testEnv(sys, fdb), receiptsInput(func(in *ReceiptsInput) {
			in.SendReceiptDir, in.HasReceiptBatFile, in.AllowWritable = &dir, &bat, true
		}), false)
		if err != nil || len(res.Warnings) == 0 {
			t.Errorf("%v, warnings %v", err, res.Warnings)
		}
	})
}

func ordersInput(f func(*OrdersInput)) OrdersInput {
	in := OrdersInput{Version: 1, ChangeSet: "cs-orders-001"}
	f(&in)
	return in
}

// BFL's model (a): one file per order into the scheduler's folder, in the
// legacy layout — without restating the credential or the database.
func TestApplyOrders_ModelA(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	env := testEnv(sys, fdb)
	before, _ := readConfig(t, sys.dir)
	pickup := mkdir(t, "herp")
	in := ordersInput(func(in *OrdersInput) {
		in.SendOrderPickupDir, in.SendOrderFormat, in.Importer = &pickup, ptr(config.OrderFormatLegacy), &ImporterSpec{Mode: "none"}
	})
	res, err := ApplyOrders(context.Background(), env, in, false)
	if err != nil {
		t.Fatalf("ApplyOrders: %v", err)
	}
	after, _ := readConfig(t, sys.dir)
	if after.SendOrderPickupDir != pickup || after.SendOrderFormat != "legacy" || after.Auth != before.Auth || after.DB != before.DB {
		t.Errorf("config = %+v", after)
	}
	if !res.Restarted || res.Proof == nil {
		t.Errorf("restarted %v, proof %+v", res.Restarted, res.Proof)
	}
	if len(fdb.opened) != 0 {
		t.Errorf("the database was opened: %v", fdb.opened)
	}
}

// A new sendOrderDir takes the counter along, at the restart.
func TestApplyOrders_MovesTheCounter(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	if err := os.WriteFile(filepath.Join(orders, orderNumberFile), []byte(`{"lastOrderNumber":4711}`), 0o600); err != nil {
		t.Fatal(err)
	}
	moved := mkdir(t, "orders2")
	in := ordersInput(func(in *OrdersInput) { in.SendOrderDir, in.Importer = &moved, &ImporterSpec{Mode: "none"} })
	chk, err := ApplyOrders(context.Background(), testEnv(sys, fdb), in, true)
	if err != nil || chk.Check == nil || !chk.Check.OrderCounter {
		t.Fatalf("--check = %+v, %v", chk.Check, err)
	}
	if _, err := ApplyOrders(context.Background(), testEnv(sys, fdb), in, false); err != nil {
		t.Fatalf("ApplyOrders: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(moved, orderNumberFile))
	if err != nil || lastOrderNumber(b) != 4711 {
		t.Errorf("the counter in the new folder: %s, %v", b, err)
	}
}

func TestApplyOrders_Refusals(t *testing.T) {
	pickup := mkdir(t, "herp")
	tests := []struct {
		name   string
		in     OrdersInput
		linked bool
		code   string
	}{
		{"nothing", ordersInput(func(*OrdersInput) {}), false, CodeSpecInvalid},
		{"an unknown layout", ordersInput(func(in *OrdersInput) { in.SendOrderFormat = ptr("fancy") }), false, CodeSpecInvalid},
		{"a pickup folder without a layout", ordersInput(func(in *OrdersInput) {
			in.SendOrderPickupDir, in.Importer = &pickup, &ImporterSpec{Mode: "none"}
		}), false, CodeConfigRefused},
		{"no importer stated", ordersInput(func(in *OrdersInput) { in.SendOrderFormat = ptr("official") }), false, CodeImporterRequired},
		{"a missing pickup folder", ordersInput(func(in *OrdersInput) {
			in.SendOrderPickupDir, in.SendOrderFormat, in.Importer = ptr(filepath.Join(pickup, "nope")), ptr("legacy"), &ImporterSpec{Mode: "none"}
		}), false, CodeSendOrderDirMissing},
		{"not has.exe behind the link", ordersInput(func(in *OrdersInput) {
			in.Importer = &ImporterSpec{Mode: "hasExe", HasExePath: filepath.Join(pickup, "evil.exe")}
		}), true, CodeImporterNotERP},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sys, fdb, _ := handInstallBox(t)
			if tc.linked {
				linkBox(t, sys)
			}
			_, before := readConfig(t, sys.dir)
			res, err := ApplyOrders(context.Background(), testEnv(sys, fdb), tc.in, false)
			wantRefusal(t, err, tc.code)
			if _, after := readConfig(t, sys.dir); !bytes.Equal(before, after) || res.Restarted || res.PreimageCaptured {
				t.Errorf("work was done before the refusal: %+v", res)
			}
		})
	}
}

func TestRun_ReceiptsAndOrders(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	env := testEnv(sys, fdb)
	for _, cmd := range []string{"receipts", "orders"} {
		out, code := Run(context.Background(), env, []string{cmd, "--check", "--spec", "-"},
			strings.NewReader(`{"version":1,"changeSet":"cs-x-000001","auth":{"password":"x"}}`))
		if code != ExitRefused || !strings.Contains(string(out), CodeSpecInvalid) {
			t.Errorf("%s with an API credential: code %d, out %s", cmd, code, out)
		}
		if _, code := Run(context.Background(), env, []string{cmd}, strings.NewReader("{}")); code != ExitUsage {
			t.Errorf("%s without --spec -: code %d", cmd, code)
		}
	}
}

// The order importer's settings (v1.0.34–v1.0.36: timeout, move refused,
// pre-check, run-log folder) are set through the orders command, each left
// out kept, and --check names them.
func TestApplyOrders_ImportSettings(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	bat := filepath.Join(orders, "digi.bat")
	if err := os.WriteFile(bat, []byte("call has.exe\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := handInstallConfig(orders)
	cfg.HasBatFile = bat
	writeConfig(t, sys.dir, cfg)
	env := testEnv(sys, fdb)
	logs := mkdir(t, "ORDERS_log")

	in := ordersInput(func(in *OrdersInput) {
		in.HasImportTimeoutSeconds, in.HasMoveRefusedOrders, in.HasOrderPrecheck, in.HasRunLogDir = ptr(600), ptr(true), ptr(true), &logs
	})
	chk, err := ApplyOrders(context.Background(), env, in, true)
	if err != nil || chk.Check == nil || !strings.Contains(chk.Check.Config, "orderImport") {
		t.Fatalf("--check = %+v, %v; want the change named orderImport", chk.Check, err)
	}
	if _, err := ApplyOrders(context.Background(), env, in, false); err != nil {
		t.Fatalf("ApplyOrders: %v", err)
	}
	after, _ := readConfig(t, sys.dir)
	if after.HasImportTimeoutSeconds != 600 || !after.HasMoveRefusedOrders || !after.HasOrderPrecheck || after.HasRunLogDir != logs ||
		after.HasBatFile != bat || after.SendOrderDir != orders || after.Auth != cfg.Auth {
		t.Fatalf("config = %+v", after)
	}

	// One setting at a time: the rest is kept; "" and false turn them off.
	in = ordersInput(func(in *OrdersInput) { in.HasRunLogDir, in.HasMoveRefusedOrders = ptr(""), ptr(false) })
	if _, err := ApplyOrders(context.Background(), env, in, false); err != nil {
		t.Fatalf("ApplyOrders: %v", err)
	}
	after, _ = readConfig(t, sys.dir)
	if after.HasRunLogDir != "" || after.HasMoveRefusedOrders || after.HasImportTimeoutSeconds != 600 || !after.HasOrderPrecheck {
		t.Fatalf("config = %+v; want hasRunLogDir and hasMoveRefusedOrders cleared, the rest kept", after)
	}
}

func TestApplyOrders_ImportSettingRefusals(t *testing.T) {
	tests := []struct {
		name string
		in   OrdersInput
		code string
	}{
		{"a timeout out of range", ordersInput(func(in *OrdersInput) { in.HasImportTimeoutSeconds = ptr(5) }), CodeSpecInvalid},
		{"a relative run-log folder", ordersInput(func(in *OrdersInput) { in.HasRunLogDir = ptr(`logs`) }), CodeSpecInvalid},
		// The hand-installed box has no importer: the connector would not start.
		{"move refused orders without an importer", ordersInput(func(in *OrdersInput) { in.HasMoveRefusedOrders = ptr(true) }), CodeConfigRefused},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sys, fdb, _ := handInstallBox(t)
			_, before := readConfig(t, sys.dir)
			res, err := ApplyOrders(context.Background(), testEnv(sys, fdb), tc.in, false)
			wantRefusal(t, err, tc.code)
			if _, after := readConfig(t, sys.dir); !bytes.Equal(before, after) || res.Restarted {
				t.Errorf("work was done before the refusal: %+v", res)
			}
		})
	}
}
