package hasavshevet

import (
	"strings"
	"testing"

	"golang.org/x/text/encoding/charmap"
)

// win1255 encodes a test fixture the way Hashavshevet writes its files.
func win1255(t *testing.T, s string) []byte {
	t.Helper()
	b, err := charmap.Windows1255.NewEncoder().Bytes([]byte(strings.ReplaceAll(s, "\n", "\r\n")))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A masofon.ini shaped as Hashavshevet 2026a SP2.1 writes it (synthetic names).
const masofonFixture = `[commands]
digi.bat=company_db -6 -c:\digi\orders\imovein.doc -FixedLength -Ansi2Ansi -c:\digi\orders\imovein.prm - -משתמש - -0 -0 - -0 -YES -;
wms.bat=company_db -6 -c:\wms\in\imovein*.dat -FixedLength -Ansi2Ansi -c:\wms\in\imovein.prm - -wms - -0 -0 - -1 -;company_db -118 -c:\wms\int\imovein*.dat -FixedLength -Ansi2Ansi -c:\wms\int\imovein.prm - -wms - -0 -0 - -1 -;
drafts.bat=company_db -118 -c:\dr\imovein.doc -FixedLength -Ansi2Ansi -c:\dr\imovein.prm - -משתמש - -0 -0 - -0 - -;
[commandsV2]
digi.bat=Company=other_db&ImportType= -99&FileStucture=Delimited&PrmFileName=x
[general]
ErrDataPath=E:\logs\orders
[stock]
AvoidDouble=NO
`

func TestParseMasofonINI(t *testing.T) {
	ini, err := ParseMasofonINI(win1255(t, masofonFixture))
	if err != nil {
		t.Fatal(err)
	}
	if ini.ErrDataPath != `E:\logs\orders` || ini.StockAvoidDouble != "NO" {
		t.Fatalf("general/stock = %q %q", ini.ErrDataPath, ini.StockAvoidDouble)
	}
	digi, ok := ini.Profile(`C:\Hash7\DIGI.bat`) // by file name, any case
	if !ok || len(digi) != 1 {
		t.Fatalf("digi.bat = %v %v", digi, ok)
	}
	want := MasofonCommand{Company: "company_db", ImportType: "6", DataFile: `c:\digi\orders\imovein.doc`, Structure: "FixedLength",
		PrmFile: `c:\digi\orders\imovein.prm`, User: "משתמש", AvoidDouble: "YES"}
	if digi[0] != want {
		t.Fatalf("digi.bat command = %+v; want %+v ([commandsV2] is the editor's copy, never read)", digi[0], want)
	}
	wms, _ := ini.Profile("wms.bat")
	if len(wms) != 2 || wms[1].ImportType != "118" || wms[0].AvoidDouble != "" {
		t.Fatalf("wms.bat commands = %+v; want two, the second type 118", wms)
	}
}

func TestDataFileMatches(t *testing.T) {
	for _, tc := range []struct {
		pattern, path string
		want          bool
	}{
		{`c:\digi\orders\imovein.doc`, `C:\Digi\Orders\IMOVEIN.doc`, true},
		{`c:\wms\in\imovein*.dat`, `c:\wms\in\imovein_12_3.dat`, true},
		{`c:\wms\in\imovein*.dat`, `c:\wms\in\imovein.doc`, false},
		{`c:\wms\in\imovein*.dat`, `c:\wms\other\imovein_1.dat`, false},
		{``, `c:\x.doc`, false},
	} {
		if got := dataFileMatches(tc.pattern, tc.path); got != tc.want {
			t.Errorf("dataFileMatches(%q, %q) = %v; want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func codes(fs []MasofonFinding) map[string]bool {
	m := map[string]bool{}
	for _, f := range fs {
		m[f.Code] = f.Defect
	}
	return m
}

func TestCheckMasofonProfile(t *testing.T) {
	ini, err := ParseMasofonINI(win1255(t, masofonFixture))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("the connector's own profile: clean", func(t *testing.T) {
		got := CheckMasofonProfile(ini, `C:\Hash7\digi.bat`, `c:\digi\orders\IMOVEIN.doc`, `c:\digi\orders\IMOVEIN.prm`)
		if len(got) != 0 {
			t.Fatalf("findings %+v; want none", got)
		}
	})
	t.Run("no line for the BAT", func(t *testing.T) {
		got := codes(CheckMasofonProfile(ini, `C:\Hash7\copy of digi.bat`, `c:\digi\orders\IMOVEIN.doc`, ""))
		if d, ok := got[MasofonProfileMissing]; !ok || !d {
			t.Fatalf("findings %v; want the defect %s", got, MasofonProfileMissing)
		}
	})
	t.Run("the profile reads another file", func(t *testing.T) {
		got := codes(CheckMasofonProfile(ini, `C:\Hash7\digi.bat`, `c:\connector\IMOVEIN.doc`, `c:\connector\IMOVEIN.prm`))
		if d, ok := got[MasofonDataFileForeign]; !ok || !d {
			t.Fatalf("findings %v; want the defect %s", got, MasofonDataFileForeign)
		}
	})
	t.Run("another profile reads the folder", func(t *testing.T) {
		got := codes(CheckMasofonProfile(ini, `C:\Hash7\wms.bat`, `c:\wms\in\imovein_5.dat`, `c:\wms\in\imovein.prm`))
		// wms.bat is the only reader of c:\wms\in, its own; its guard falls back to [stock] NO.
		if _, shared := got[MasofonSharedInput]; shared {
			t.Errorf("findings %v: a profile is not shared with itself", got)
		}
		if d, ok := got[MasofonDuplicateOff]; !ok || d {
			t.Errorf("findings %v; want the note %s (empty arg 14, [stock] AvoidDouble=NO)", got, MasofonDuplicateOff)
		}
	})
	t.Run("drafts profile, guard empty", func(t *testing.T) {
		got := codes(CheckMasofonProfile(ini, `C:\Hash7\drafts.bat`, `c:\dr\imovein.doc`, `c:\dr\imovein.prm`))
		if d, ok := got[MasofonNoIssue]; !ok || d {
			t.Errorf("findings %v; want the note %s for type 118", got, MasofonNoIssue)
		}
	})
	t.Run("shared input and dash", func(t *testing.T) {
		shared := strings.Replace(masofonFixture, `drafts.bat=company_db -118 -c:\dr\imovein.doc`, `drafts.bat=company_db -118 -c:\digi\orders\other.doc`, 1)
		ini2, err := ParseMasofonINI(win1255(t, shared))
		if err != nil {
			t.Fatal(err)
		}
		got := codes(CheckMasofonProfile(ini2, `C:\Hash7\digi.bat`, `c:\digi\orders\IMOVEIN.doc`, `c:\digi\orders\IMOVEIN.prm`))
		if d, ok := got[MasofonSharedInput]; !ok || !d {
			t.Errorf("findings %v; want the defect %s", got, MasofonSharedInput)
		}
		got = codes(CheckMasofonProfile(ini2, `C:\Hash7\digi.bat`, `c:\b2b-orders\IMOVEIN.doc`, ""))
		if d, ok := got[MasofonDashInPath]; !ok || !d {
			t.Errorf("findings %v; want the defect %s", got, MasofonDashInPath)
		}
	})
}
