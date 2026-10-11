package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The receipt importer's folder must never be the order importer's: both write
// fixed-name files and run their importer from there, and the KUPAIN folder
// sits under a web root on a real install.
func TestValidateReceipts_RefusesTheOrderFolder(t *testing.T) {
	root := t.TempDir()
	orders := filepath.Join(root, "orders")
	receipts := filepath.Join(root, "herp")
	for _, d := range []string{orders, receipts} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "orders-link")
	if err := os.Symlink(orders, link); err != nil {
		t.Logf("no symlink support (%v); symlink case skipped", err)
		link = ""
	}

	for _, tc := range []struct {
		name           string
		order, receipt string
		wantErr        bool
	}{
		{"both unset", "", "", false},
		{"receipts unset", orders, "", false},
		{"orders unset", "", receipts, false},
		{"different folders", orders, receipts, false},
		{"same folder", orders, orders, true},
		{"trailing slash", orders, orders + string(filepath.Separator), true},
		{"dot segments", orders, filepath.Join(root, "herp", "..", "orders"), true},
		{"surrounding spaces", orders, "  " + orders + " ", true},
		{"a folder that does not exist yet", filepath.Join(root, "new"), filepath.Join(root, "new") + "/", true},
		{"symlink to the order folder", orders, link, link != ""},
		{"case differs", orders, strings.ToUpper(orders), runtime.GOOS == "windows"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.receipt == "" && tc.name == "symlink to the order folder" {
				t.Skip("no symlink")
			}
			cfg := Config{SendOrderDir: tc.order, SendReceiptDir: tc.receipt}
			err := cfg.ValidateReceipts()
			if tc.wantErr && err == nil {
				t.Fatalf("ValidateReceipts(%q, %q) = nil; want refused", tc.order, tc.receipt)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateReceipts(%q, %q) = %v; want nil", tc.order, tc.receipt, err)
			}
			if err != nil && !strings.Contains(err.Error(), "sendReceiptDir") {
				t.Errorf("error %q should name sendReceiptDir", err)
			}
		})
	}
}

// Windows paths are case-insensitive: the same folder spelled in another case
// is the same folder. Pinned through the comparison itself so it runs here too.
func TestSameDir_FoldsCaseWhenAsked(t *testing.T) {
	a := filepath.Join(t.TempDir(), "Herp")
	if !sameDir(a, strings.ToUpper(a), true) {
		t.Error("case-folded comparison: different case should be the same folder")
	}
	if runtime.GOOS != "windows" && sameDir(a, strings.ToUpper(a), false) {
		t.Error("case-sensitive comparison treated different case as the same folder")
	}
}

func TestReceiptsConfigured(t *testing.T) {
	for _, tc := range []struct {
		erp  ERPType
		dir  string
		want bool
	}{
		{ERPHasavshevet, `C:\herp`, true},
		{ERPHasavshevet, "   ", false},
		{ERPHasavshevet, "", false},
		{ERPSAP, `C:\herp`, false},
	} {
		cfg := Config{ERP: tc.erp, SendReceiptDir: tc.dir}
		if got := cfg.ReceiptsConfigured(); got != tc.want {
			t.Errorf("ReceiptsConfigured(erp=%s dir=%q) = %v; want %v", tc.erp, tc.dir, got, tc.want)
		}
	}
}

// Without the new keys a saved config is what v1.0.17 wrote: the keys are
// omitted, not written empty.
func TestReceiptKeysAreOmittedWhenUnset(t *testing.T) {
	out, err := yaml.Marshal(Default())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"sendReceiptDir", "hasReceiptBatFile", "receiptCardNames"} {
		if strings.Contains(string(out), key) {
			t.Errorf("an unconfigured config writes %q:\n%s", key, out)
		}
	}
}

func TestReceiptKeysRoundTrip(t *testing.T) {
	in := `
erp: hasavshevet
sendReceiptDir: C:\Apache24\htdocs\herp
hasReceiptBatFile: C:\Hash7\kupain.bat
receiptCardNames:
  issuer:
    "6": מקס
  brand:
    "2": ויזה
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(in), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.SendReceiptDir != `C:\Apache24\htdocs\herp` || cfg.HasReceiptBatFile != `C:\Hash7\kupain.bat` {
		t.Errorf("dirs = %q / %q", cfg.SendReceiptDir, cfg.HasReceiptBatFile)
	}
	if cfg.ReceiptCardNames.Issuer["6"] != "מקס" || cfg.ReceiptCardNames.Brand["2"] != "ויזה" {
		t.Errorf("card names = %+v", cfg.ReceiptCardNames)
	}
}
