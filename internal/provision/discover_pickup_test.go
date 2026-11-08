package provision

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// A box whose orders go to a pickup folder for the ERP's scheduler (model a,
// DIG-1297) has an importer — the scheduler — so discover calls no
// "no_importer" defect there, and it reports the two order keys, so a plan
// (and an upgrade's inverse) can see that the box relies on them.
func TestDiscover_PickupBoxHasAnImporterAndReportsTheKeys(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	cfg, _ := readConfig(t, sys.dir)
	pickup := filepath.Join(t.TempDir(), "herp")
	cfg.SendOrderPickupDir, cfg.SendOrderFormat = pickup, config.OrderFormatLegacy
	writeConfig(t, sys.dir, cfg)

	f, err := Discover(context.Background(), testEnv(sys, fdb), DiscoverInput{DB: &DiscoverDB{User: "sa", Password: testSaPass}})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, d := range f.Defects {
		if d.Code == DefectNoImporter {
			t.Errorf("defect %s on a model (a) box: %s", d.Code, d.Message)
		}
	}
	c := f.Connector.Config
	if c == nil || c.SendOrderPickupDir != pickup || c.SendOrderFormat != config.OrderFormatLegacy {
		t.Fatalf("config facts = %+v; want sendOrderPickupDir %s and sendOrderFormat legacy", c, pickup)
	}
}
