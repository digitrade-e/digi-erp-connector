package provision

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
)

func TestSeedReceipts(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	env := testEnv(sys, fdb)
	res, err := SeedReceipts(context.Background(), env, 500000)
	if err != nil {
		t.Fatalf("SeedReceipts: %v", err)
	}
	n, err := hasavshevet.NewReceiptNumberStore(hasavshevet.ReceiptNumberStorePath(sys.dir)).Peek()
	if err != nil || n != 500000 {
		t.Fatalf("the store says %d, %v", n, err)
	}
	if res.Restarted || len(res.Steps) != 1 || !res.Steps[0].Changed {
		t.Errorf("result %+v", res)
	}
	// Written once: a second seed, lower or higher, is refused and changes nothing.
	for _, again := range []int64{1, 900000} {
		_, err := SeedReceipts(context.Background(), env, again)
		wantRefusal(t, err, CodeReceiptStoreExists)
	}
	if n, _ := hasavshevet.NewReceiptNumberStore(hasavshevet.ReceiptNumberStorePath(sys.dir)).Peek(); n != 500000 {
		t.Errorf("a second seed moved the store to %d", n)
	}
}

func TestSeedReceipts_Refusals(t *testing.T) {
	t.Run("a damaged store is never replaced", func(t *testing.T) {
		sys, fdb, _ := handInstallBox(t)
		p := hasavshevet.ReceiptNumberStorePath(sys.dir)
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		_ = os.WriteFile(p, []byte("{broken"), 0o600)
		_, err := SeedReceipts(context.Background(), testEnv(sys, fdb), 500000)
		wantRefusal(t, err, CodeReceiptStoreExists)
		if b, _ := os.ReadFile(p); string(b) != "{broken" {
			t.Error("the damaged store was touched")
		}
	})
	t.Run("at or below a receipt already written", func(t *testing.T) {
		sys, fdb, _ := handInstallBox(t)
		_ = os.MkdirAll(filepath.Join(sys.dir, "history", "receipts", "500123"), 0o700)
		_, err := SeedReceipts(context.Background(), testEnv(sys, fdb), 500123)
		wantRefusal(t, err, CodeReceiptSeedLow)
		if _, err := SeedReceipts(context.Background(), testEnv(sys, fdb), 500124); err != nil {
			t.Errorf("a seed above the history: %v", err)
		}
	})
	for _, n := range []int64{0, -5, 1000000000} {
		sys, fdb, _ := handInstallBox(t)
		_, err := SeedReceipts(context.Background(), testEnv(sys, fdb), n)
		wantRefusal(t, err, CodeSpecInvalid)
	}
}

func TestRun_ReceiptSeed(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	env := testEnv(sys, fdb)
	for _, bad := range [][]string{{"receipt-seed"}, {"receipt-seed", "0"}, {"receipt-seed", "-1"}, {"receipt-seed", "1e3"},
		{"receipt-seed", "1000000000"}, {"receipt-seed", "5", "6"}} {
		if _, code := Run(context.Background(), env, bad, strings.NewReader("")); code != ExitUsage {
			t.Errorf("%v: code %d, want usage", bad, code)
		}
	}
	out, code := Run(context.Background(), env, []string{"receipt-seed", "700001"}, strings.NewReader(""))
	if code != ExitOK || !strings.Contains(string(out), "the next receipt is 700001") {
		t.Errorf("code %d, out %s", code, out)
	}
}
