package hasavshevet

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// ValidateReceiptConfig is the one check the daemon and the GUI share for the
// receipt keys: a config the GUI saves is one the daemon starts on.
func TestValidateReceiptConfig(t *testing.T) {
	dir := t.TempDir()
	orders, herp := filepath.Join(dir, "orders"), filepath.Join(dir, "herp")
	for _, tc := range []struct {
		name    string
		cfg     config.Config
		wantErr string
	}{
		{"no receipt keys (v1.0.17)", config.Config{ERP: config.ERPHasavshevet, SendOrderDir: orders}, ""},
		{"relative receipt folder", config.Config{ERP: config.ERPHasavshevet, SendOrderDir: orders, SendReceiptDir: "herp"}, "absolute"},
		{"separate folders", config.Config{ERP: config.ERPHasavshevet, SendOrderDir: orders, SendReceiptDir: herp}, ""},
		{"same folder", config.Config{ERP: config.ERPHasavshevet, SendOrderDir: orders, SendReceiptDir: orders + "/"}, "sendReceiptDir"},
		{"has.exe receipt import", config.Config{ERP: config.ERPHasavshevet, SendOrderDir: orders, SendReceiptDir: herp,
			HasExePath: filepath.Join(dir, "has.exe"), HasReceiptParamFile: filepath.Join(dir, "kupain_perm.bat")}, ""},
		{"BAT and has.exe both set", config.Config{ERP: config.ERPHasavshevet, SendOrderDir: orders, SendReceiptDir: herp,
			HasReceiptBatFile: filepath.Join(dir, "kupain.bat"),
			HasExePath:        filepath.Join(dir, "has.exe"), HasReceiptParamFile: filepath.Join(dir, "kupain_perm.bat")}, "ambiguous"},
		{"a card name that fits", config.Config{ReceiptCardNames: config.ReceiptCardNamesConfig{
			Issuer: map[string]string{"6": "מקס"}}}, ""},
		{"a card name too wide for F17", config.Config{ReceiptCardNames: config.ReceiptCardNamesConfig{
			Issuer: map[string]string{"6": strings.Repeat("א", 26)}}}, "receipt card names"},
		{"a card name not in Windows-1255", config.Config{ReceiptCardNames: config.ReceiptCardNamesConfig{
			Brand: map[string]string{"2": "فيزا"}}}, "receipt card names"},
		{"a blank card code", config.Config{ReceiptCardNames: config.ReceiptCardNamesConfig{
			Brand: map[string]string{" ": "ויזה"}}}, "receipt card names"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateReceiptConfig(tc.cfg)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("ValidateReceiptConfig = %v; want nil", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("ValidateReceiptConfig = nil; want an error naming %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("error %q should name %q", err, tc.wantErr)
			}
		})
	}
}

// What the config holds is what the writer uses.
func TestReceiptWriterConfigFrom(t *testing.T) {
	cfg := config.Config{
		SendReceiptDir:    `C:\herp`,
		HasReceiptBatFile: `C:\Hash7\kupain.bat`,
		ReceiptCardNames: config.ReceiptCardNamesConfig{
			Issuer: map[string]string{"6": "מקס"},
			Brand:  map[string]string{"2": "ויזה"},
		},
	}
	got := ReceiptWriterConfigFrom(cfg, "/data")
	if got.SendReceiptDir != `C:\herp` || got.DataDir != "/data" || got.HasReceiptBatFile != `C:\Hash7\kupain.bat` {
		t.Errorf("writer config = %+v", got)
	}
	if got.CardNames.Issuer["6"] != "מקס" || got.CardNames.Brand["2"] != "ויזה" {
		t.Errorf("card names = %+v", got.CardNames)
	}
	// An override from the config is what F17 gets.
	if name, _, _ := cardName("6", "", got.CardNames); name != "מקס" {
		t.Errorf("card name for issuer 6 = %q", name)
	}
}

// A card-name override the builder cannot write is never a FieldError (which
// the route would answer 400).
func TestCardNamesConfigErrorIsNotAFieldError(t *testing.T) {
	names := ReceiptCardNames{Issuer: map[string]string{"2": strings.Repeat("א", 26)}}
	_, _, err := buildKUPAINRecord(goldenReceipt(), 1, names)
	var fe *FieldError
	var ce *CardNamesConfigError
	if !errors.As(err, &ce) || errors.As(err, &fe) {
		t.Fatalf("err = %v; want a CardNamesConfigError and no FieldError", err)
	}
}
