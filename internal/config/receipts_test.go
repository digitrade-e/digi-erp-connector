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
			cfg := Config{ERP: ERPHasavshevet, SendOrderDir: tc.order, SendReceiptDir: tc.receipt}
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

// A relative sendReceiptDir would resolve against the service's working
// directory (System32 under the SCM). Daemon and GUI share this refusal.
func TestValidateReceipts_RefusesARelativeFolder(t *testing.T) {
	for _, dir := range []string{"herp", `.\herp`, "./herp", "../x/herp"} {
		if err := (Config{ERP: ERPHasavshevet, SendReceiptDir: dir}).ValidateReceipts(); err == nil || !strings.Contains(err.Error(), "absolute") {
			t.Errorf("ValidateReceipts(%q) = %v; want refused as not absolute", dir, err)
		}
	}
	if err := (Config{ERP: ERPHasavshevet, SendReceiptDir: "  " + t.TempDir() + " "}).ValidateReceipts(); err != nil {
		t.Errorf("an absolute folder with spaces around it was refused: %v", err)
	}
}

// The receipt import runs one way: a Masofon BAT (hasReceiptBatFile) or
// has.exe with a receipt parameter file (hasReceiptParamFile, the same has.exe
// the orders use). Both set is ambiguous and refused; every path is absolute,
// because the service's working directory is System32.
func TestValidateReceipts_ImportKeys(t *testing.T) {
	abs := func(p string) string { return filepath.Join(t.TempDir(), p) }
	herp := abs("herp")
	bat, prm, exe := abs("kupain.bat"), abs("kupain_perm.bat"), abs("has.exe")
	for _, tc := range []struct {
		name    string
		cfg     Config
		wantErr string // "" = accepted; else a fragment of the error
	}{
		{"neither (files only)", Config{ERP: ERPHasavshevet, SendReceiptDir: herp}, ""},
		{"BAT only", Config{ERP: ERPHasavshevet, SendReceiptDir: herp, HasReceiptBatFile: bat}, ""},
		{"has.exe only", Config{ERP: ERPHasavshevet, SendReceiptDir: herp, HasExePath: exe, HasReceiptParamFile: prm}, ""},
		{"has.exe with spaces around the keys", Config{ERP: ERPHasavshevet, SendReceiptDir: herp, HasExePath: " " + exe + " ", HasReceiptParamFile: "\t" + prm}, ""},
		{"both set", Config{ERP: ERPHasavshevet, SendReceiptDir: herp, HasReceiptBatFile: bat, HasExePath: exe, HasReceiptParamFile: prm}, "ambiguous"},
		{"both set, no hasExePath", Config{ERP: ERPHasavshevet, SendReceiptDir: herp, HasReceiptBatFile: bat, HasReceiptParamFile: prm}, "ambiguous"},
		{"param file without hasExePath", Config{ERP: ERPHasavshevet, SendReceiptDir: herp, HasReceiptParamFile: prm}, "hasExePath"},
		{"param file with a blank hasExePath", Config{ERP: ERPHasavshevet, SendReceiptDir: herp, HasExePath: "  ", HasReceiptParamFile: prm}, "hasExePath"},
		{"relative param file", Config{ERP: ERPHasavshevet, SendReceiptDir: herp, HasExePath: exe, HasReceiptParamFile: "kupain_perm.bat"}, "hasReceiptParamFile"},
		{"relative hasExePath", Config{ERP: ERPHasavshevet, SendReceiptDir: herp, HasExePath: "has.exe", HasReceiptParamFile: prm}, "hasExePath"},
		{"relative BAT", Config{ERP: ERPHasavshevet, SendReceiptDir: herp, HasReceiptBatFile: `.\kupain.bat`}, "hasReceiptBatFile"},
		// Orders alone may keep a relative has.exe: only the receipt import is checked.
		{"relative hasExePath, receipts not importing", Config{ERP: ERPHasavshevet, SendReceiptDir: herp, HasExePath: "has.exe"}, ""},
		{"v1.0.17 config", Config{HasExePath: "has.exe", HasParamFile: "digi_perm.bat"}, ""},
		// Dormant keys never stop a connector that writes no receipts: orders
		// must not go down over a stray line.
		{"stray keys, no sendReceiptDir", Config{ERP: ERPHasavshevet, HasReceiptBatFile: "kupain.bat", HasReceiptParamFile: "k.prm"}, ""},
		{"stray keys, erp: sap", Config{ERP: ERPSAP, SendReceiptDir: herp, HasReceiptBatFile: bat, HasReceiptParamFile: "k.prm"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.ValidateReceipts()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("ValidateReceipts = %v; want nil", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("ValidateReceipts = nil; want an error naming %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("error %q should name %q", err, tc.wantErr)
			}
		})
	}
}

func TestReceiptParamFileRoundTripsAndIsOmittedWhenUnset(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("hasReceiptParamFile: C:\\Hash7\\kupain_perm.bat\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.HasReceiptParamFile != `C:\Hash7\kupain_perm.bat` {
		t.Errorf("hasReceiptParamFile = %q", cfg.HasReceiptParamFile)
	}
	out, err := yaml.Marshal(Default())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "hasReceiptParamFile") {
		t.Errorf("an unconfigured config writes hasReceiptParamFile:\n%s", out)
	}
}

// A connector that writes no receipts (erp: sap, or no sendReceiptDir) is never
// stopped by a receipt key, however wrong: orders must keep running.
func TestValidateReceipts_DormantKeysNeverRefuse(t *testing.T) {
	orders := t.TempDir()
	for name, cfg := range map[string]Config{
		"sap, relative sendReceiptDir":    {ERP: ERPSAP, SendReceiptDir: "herp"},
		"sap, sendReceiptDir = order dir": {ERP: ERPSAP, SendOrderDir: orders, SendReceiptDir: orders},
		"sap, relative receipt BAT":       {ERP: ERPSAP, SendReceiptDir: orders + "/herp", HasReceiptBatFile: "k.bat"},
		"no erp, sendReceiptDir = order":  {SendOrderDir: orders, SendReceiptDir: orders},
		"hasavshevet, no sendReceiptDir":  {ERP: ERPHasavshevet, HasReceiptBatFile: "k.bat", HasReceiptParamFile: "k.prm"},
	} {
		if err := cfg.ValidateReceipts(); err != nil {
			t.Errorf("%s: ValidateReceipts = %v; want nil (receipts are not configured)", name, err)
		}
	}
}
