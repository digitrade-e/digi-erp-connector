package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
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

// testEnv is a cutover-seed run against a temp data directory and a fake
// config.yaml, recording whether provisioning would have saved it.
type testEnv struct {
	env
	cfgPath string
	saves   int
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	te := &testEnv{cfgPath: filepath.Join(dir, "config.yaml")}
	// An existing install's config, with settings the provisioning defaults
	// would overwrite.
	if err := os.WriteFile(te.cfgPath, []byte("erp: sap\ndb:\n    encrypt: true\n    trustServerCertificate: true\n"+
		"auth:\n    username: bfl\n    password: pw\n    secret: 0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	te.env = env{
		dataDir: dir,
		loadConfig: func() (config.Config, error) {
			var c config.Config
			b, err := os.ReadFile(te.cfgPath)
			if err != nil {
				return c, err
			}
			return c, yaml.Unmarshal(b, &c)
		},
		saveConfig: func(c config.Config) error {
			te.saves++
			b, err := yaml.Marshal(c)
			if err != nil {
				return err
			}
			return os.WriteFile(te.cfgPath, b, 0o600)
		},
		out: io.Discard,
	}
	return te
}

// "-receipt-seed 0" — an empty script variable — must be refused as a seed.
// It used to fall through to provisioning (the mode was chosen by the value)
// and re-save config.yaml with db.encrypt=false, trust=false, erp=hasavshevet.
func TestRun_ReceiptSeedZeroIsRefusedAndTouchesNothing(t *testing.T) {
	for _, args := range [][]string{
		{"-receipt-seed", "0"},
		{"-receipt-seed=0"},
		{"-receipt-seed", "-3"},
		{"-receipt-seed", "100", "-encrypt"},
		{"-encrypt", "-receipt-seed", "0"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			te := newTestEnv(t)
			before, _ := os.ReadFile(te.cfgPath)

			if err := run(args, te.env); err == nil {
				t.Fatal("run succeeded; want the seed refused")
			}
			if te.saves != 0 {
				t.Errorf("config.yaml was saved %d time(s)", te.saves)
			}
			if after, _ := os.ReadFile(te.cfgPath); string(after) != string(before) {
				t.Errorf("config.yaml changed:\n%s\n->\n%s", before, after)
			}
			if _, err := os.Stat(hasavshevet.ReceiptNumberStorePath(te.dataDir)); err == nil {
				t.Error("a refused seed created the store")
			}
		})
	}
}

func TestRun_ReceiptSeedWritesOnlyTheStore(t *testing.T) {
	te := newTestEnv(t)
	before, _ := os.ReadFile(te.cfgPath)
	if err := run([]string{"-receipt-seed", "5000000"}, te.env); err != nil {
		t.Fatalf("run: %v", err)
	}
	if n, err := hasavshevet.NewReceiptNumberStore(hasavshevet.ReceiptNumberStorePath(te.dataDir)).Peek(); err != nil || n != 5000000 {
		t.Fatalf("store = %d, %v; want 5000000", n, err)
	}
	if after, _ := os.ReadFile(te.cfgPath); te.saves != 0 || string(after) != string(before) {
		t.Error("a receipt seed touched config.yaml")
	}
}

// Provisioning still saves — so the seam above is the one the tool uses.
func TestRun_ProvisioningSavesConfig(t *testing.T) {
	te := newTestEnv(t)
	if err := run([]string{"-auth-user", "bfl", "-listen", "127.0.0.1:8082"}, te.env); err != nil {
		t.Fatalf("run: %v", err)
	}
	if te.saves != 1 {
		t.Errorf("saves = %d; want 1", te.saves)
	}
}
