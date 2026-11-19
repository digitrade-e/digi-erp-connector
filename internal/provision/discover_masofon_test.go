package provision

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/text/encoding/charmap"
)

// The BAT's Masofon profile reads another integration's file: discover names
// it (research P2), reports the profile, and never reads a .PRJ.
func TestDiscover_MasofonProfileReadsAnotherFile(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	hash := t.TempDir()
	bat := filepath.Join(hash, "importorders.bat")
	if err := os.WriteFile(bat, []byte("call has.exe\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ini := strings.Join([]string{
		`[commands]`,
		`importorders.bat=company_db -6 -e:\other\orders\imovein.doc -FixedLength -Ansi2Ansi -e:\other\orders\imovein.prm - -user - -0 -0 - -0 -YES -;`,
		`[general]`,
		`ErrDataPath=E:\other\log`,
		``,
	}, "\r\n")
	b, _ := charmap.Windows1255.NewEncoder().Bytes([]byte(ini))
	if err := os.WriteFile(filepath.Join(hash, "masofon.ini"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := handInstallConfig(orders)
	cfg.HasBatFile = bat
	writeConfig(t, sys.dir, cfg)

	f, err := Discover(context.Background(), testEnv(sys, fdb), DiscoverInput{})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	found := false
	for _, d := range f.Defects {
		if d.Code == "masofon_data_file_mismatch" {
			found = strings.Contains(d.Message, `e:\other\orders\imovein.doc`)
		}
	}
	if !found {
		t.Errorf("defects = %+v; want masofon_data_file_mismatch naming the profile's file", f.Defects)
	}
	if f.Masofon == nil || f.Masofon.Profile != "importorders.bat" || f.Masofon.ErrDataPath != `E:\other\log` || len(f.Masofon.Commands) != 1 {
		t.Errorf("masofon facts = %+v", f.Masofon)
	}
}
