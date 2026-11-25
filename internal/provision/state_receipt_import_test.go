package provision

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
)

// state {receiptNumber} (plan 096 phase 3) asks the database this box reads, with the stored password, for the
// receipt's file under the default name, and passes the run-log folder it resolves.
func TestStateReceiptImportAsksTheDatabase(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	cfg, _ := readConfig(t, sys.dir)
	cfg.HasRunLogDir = `D:\HASH\rep\Err`
	writeConfig(t, sys.dir, cfg)
	st, err := State(context.Background(), testEnv(sys, fdb), StateInput{ReceiptNumber: 10200})
	if err != nil {
		t.Fatal(err)
	}
	ri := st.ReceiptImport
	if ri == nil || !ri.Untrusted || ri.Result != hasavshevet.ReceiptImportImported || ri.ReceiptNumber != 10200 {
		t.Fatalf("receiptImport %+v", ri)
	}
	if len(fdb.receiptAsked) != 1 || fdb.receiptAsked[0] != `erp_db|KUPAIN_10200.doc|D:\HASH\rep\Err` {
		t.Fatalf("asked %v", fdb.receiptAsked)
	}
}

func TestStateReceiptImportOnlyWhenAsked(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	st, err := State(context.Background(), testEnv(sys, fdb), StateInput{})
	if err != nil || st.ReceiptImport != nil || len(fdb.receiptAsked) != 0 {
		t.Fatalf("unasked: %+v, %v, asked %v", st.ReceiptImport, err, fdb.receiptAsked)
	}
	if _, err := State(context.Background(), testEnv(sys, fdb), StateInput{ReceiptNumber: -1}); err == nil {
		t.Fatal("a negative receipt number must be refused")
	}
}

// The cases that cannot look: each is unchecked with a reason, and a refused login never echoes the driver's
// error (it carries the DSN and the password).
func TestStateReceiptImportUnchecked(t *testing.T) {
	cases := map[string]struct {
		edit func(*config.Config, *fakeSys, *fakeDB)
		want string
	}{
		"no database": {func(c *config.Config, _ *fakeSys, _ *fakeDB) { c.DB.Database = "" }, "reads no database"},
		"unsafe name": {func(c *config.Config, _ *fakeSys, _ *fakeDB) { c.DB.Database = "erp];DROP" }, "not a plain database name"},
		"no password": {func(c *config.Config, s *fakeSys, _ *fakeDB) {
			_ = os.Remove(s.secretPath(config.DBPasswordKey(c.ERP)))
		}, "password is not stored"},
		"refused login": {func(_ *config.Config, _ *fakeSys, d *fakeDB) { d.saPass = "another" }, "cannot be opened"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			sys, fdb, _ := handInstallBox(t)
			cfg, _ := readConfig(t, sys.dir)
			tc.edit(&cfg, sys, fdb)
			writeConfig(t, sys.dir, cfg)
			st, _ := State(context.Background(), testEnv(sys, fdb), StateInput{ReceiptNumber: 7})
			ri := st.ReceiptImport
			if ri == nil || ri.Result != hasavshevet.ReceiptImportUnchecked || !strings.Contains(ri.Detail, tc.want) {
				t.Fatalf("receiptImport %+v", ri)
			}
			if strings.Contains(ri.Detail, testSaPass) || strings.Contains(ri.Detail, "sqlserver://") {
				t.Fatalf("detail leaks the login: %q", ri.Detail)
			}
			if len(fdb.receiptAsked) != 0 {
				t.Fatalf("looked anyway: %v", fdb.receiptAsked)
			}
		})
	}
}
