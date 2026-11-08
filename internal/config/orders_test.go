package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateOrders(t *testing.T) {
	root := t.TempDir()
	own := filepath.Join(root, "orders")
	herp := filepath.Join(root, "herp")
	for _, d := range []string{own, herp} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	base := Config{ERP: ERPHasavshevet, SendOrderDir: own}
	for _, tc := range []struct {
		name    string
		edit    func(c *Config)
		wantErr string // "" = accepted
	}{
		{"today: nothing set", func(c *Config) {}, ""},
		{"official", func(c *Config) { c.SendOrderFormat = "official" }, ""},
		{"legacy", func(c *Config) { c.SendOrderFormat = "legacy" }, ""},
		{"an unknown layout", func(c *Config) { c.SendOrderFormat = "Legacy" }, "sendOrderFormat"},
		{"pickup", func(c *Config) { c.SendOrderPickupDir = herp }, ""},
		{"pickup with legacy and has.exe for receipts", func(c *Config) {
			c.SendOrderPickupDir, c.SendOrderFormat, c.HasExePath = herp, "legacy", `C:\Hash7\has.exe`
		}, ""},
		{"pickup relative", func(c *Config) { c.SendOrderPickupDir = "herp" }, "absolute"},
		{"pickup is the order folder", func(c *Config) { c.SendOrderPickupDir = own + string(filepath.Separator) }, "holds the sendOrderDir"},
		{"pickup holds the order folder", func(c *Config) {
			c.SendOrderDir, c.SendOrderPickupDir = filepath.Join(herp, "connector"), herp
		}, "holds the sendOrderDir"},
		{"pickup is the receipt folder", func(c *Config) {
			c.SendOrderPickupDir, c.SendReceiptDir = herp, filepath.Join(herp, ".")
		}, "sendReceiptDir"},
		{"pickup beside its receipts subfolder", func(c *Config) {
			c.SendOrderPickupDir, c.SendReceiptDir = herp, filepath.Join(herp, "receipts")
		}, ""},
		{"pickup and a BAT importer", func(c *Config) { c.SendOrderPickupDir, c.HasBatFile = herp, `C:\Hash7\digi.bat` }, "importer"},
		{"pickup and a has.exe parameter file", func(c *Config) { c.SendOrderPickupDir, c.HasParamFile = herp, "digi_perm.bat" }, "importer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			tc.edit(&c)
			err := c.ValidateOrders()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateOrders = %v; want accepted", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateOrders = %v; want an error naming %q", err, tc.wantErr)
			}
		})
	}
}

// A sendOrderDir that does not exist yet, under a pickup folder reached by
// another name (a link here; on Windows an 8.3 short name like the CI's temp
// folder), is still seen inside it: the path is resolved through its nearest
// existing folder.
func TestValidateOrders_ANewOrderFolderUnderTheLinkedPickupFolder(t *testing.T) {
	root := t.TempDir()
	herp := filepath.Join(root, "herp")
	if err := os.MkdirAll(herp, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "herp-link")
	if err := os.Symlink(herp, link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	c := Config{ERP: ERPHasavshevet, SendOrderDir: filepath.Join(link, "connector", "orders"), SendOrderPickupDir: herp}
	if err := c.ValidateOrders(); err == nil || !strings.Contains(err.Error(), "holds the sendOrderDir") {
		t.Fatalf("ValidateOrders = %v; want the pickup folder seen holding the (not yet created) sendOrderDir", err)
	}
}

// Keys on a connector that writes no orders never refuse: a stray line must
// not stop a query-only node from starting.
func TestValidateOrders_DormantKeysNeverRefuse(t *testing.T) {
	for name, c := range map[string]Config{
		"no sendOrderDir": {ERP: ERPHasavshevet, SendOrderFormat: "nonsense", SendOrderPickupDir: "relative"},
		"erp sap":         {ERP: ERPSAP, SendOrderDir: t.TempDir(), SendOrderFormat: "nonsense", SendOrderPickupDir: "relative"},
	} {
		if err := c.ValidateOrders(); err != nil {
			t.Errorf("%s: ValidateOrders = %v; want nil", name, err)
		}
	}
}

func TestOrderFormatAndPickup(t *testing.T) {
	c := Config{ERP: ERPHasavshevet, SendOrderDir: "/x"}
	if c.OrderFormat() != OrderFormatOfficial || c.OrdersPickup() {
		t.Fatalf("defaults: format %q pickup %v; want official, false", c.OrderFormat(), c.OrdersPickup())
	}
	c.SendOrderFormat, c.SendOrderPickupDir = " legacy ", "/herp"
	if c.OrderFormat() != OrderFormatLegacy || !c.OrdersPickup() {
		t.Fatalf("set: format %q pickup %v; want legacy, true", c.OrderFormat(), c.OrdersPickup())
	}
	c.SendOrderDir = ""
	if c.OrdersPickup() {
		t.Fatal("pickup without orders configured")
	}
}
