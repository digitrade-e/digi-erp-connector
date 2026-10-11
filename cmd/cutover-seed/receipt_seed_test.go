package main

import (
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
)

func TestSeedReceipts(t *testing.T) {
	dir := t.TempDir()
	store := hasavshevet.ReceiptNumberStorePath(dir)

	for _, bad := range []int64{0, -5} {
		if err := seedReceipts(dir, bad); err == nil {
			t.Errorf("seed %d accepted; N must be > 0", bad)
		}
	}
	if _, err := os.Stat(store); err == nil {
		t.Fatal("a refused seed created the store")
	}

	if err := seedReceipts(dir, 5_000_000); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if n, err := hasavshevet.NewReceiptNumberStore(store).Peek(); err != nil || n != 5_000_000 {
		t.Fatalf("store after seed = %d, %v; want 5000000", n, err)
	}

	// Written only when absent: a second seed — lower or higher — changes
	// nothing, so it can never lower the numbering.
	before, _ := os.ReadFile(store)
	for _, again := range []int64{1, 9_000_000} {
		if err := seedReceipts(dir, again); err == nil {
			t.Errorf("second seed %d accepted", again)
		}
	}
	if after, _ := os.ReadFile(store); string(after) != string(before) {
		t.Errorf("a second seed rewrote the store:\n%s\n->\n%s", before, after)
	}
}

// -receipt-seed is its own mode: combined with the provisioning flags it
// would re-save config.yaml with their defaults (db.encrypt=false, …) on a
// box that only wanted a seed.
func TestReceiptSeedStandsAlone(t *testing.T) {
	fs := flag.NewFlagSet("cutover-seed", flag.ContinueOnError)
	fs.Int64("receipt-seed", 0, "")
	fs.String("listen", "", "")
	fs.Bool("encrypt", false, "")

	if err := fs.Parse([]string{"-receipt-seed", "100"}); err != nil {
		t.Fatal(err)
	}
	if got := otherFlagsSet(fs, "receipt-seed"); len(got) != 0 {
		t.Errorf("otherFlagsSet = %v; want none", got)
	}

	fs2 := flag.NewFlagSet("cutover-seed", flag.ContinueOnError)
	fs2.Int64("receipt-seed", 0, "")
	fs2.String("listen", "", "")
	fs2.Bool("encrypt", false, "")
	if err := fs2.Parse([]string{"-receipt-seed", "100", "-encrypt"}); err != nil {
		t.Fatal(err)
	}
	if got := otherFlagsSet(fs2, "receipt-seed"); strings.Join(got, ",") != "encrypt" {
		t.Errorf("otherFlagsSet = %v; want [encrypt]", got)
	}
}
