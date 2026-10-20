package provision

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The pilot box (plan 068 §2.4), as Discover must see it: the
// five defects of a hand install, nothing else called a defect, and a
// suggestion that fixes them.
func TestDiscover_FindsTheFiveDefectsOfAHandInstall(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	// The daemon started on 2026-10-06; config.yaml was saved on 2026-10-19.
	sys.svc.ProcessStart = time.Now().Add(-14 * 24 * time.Hour)
	sys.sqlBrowser.State = "running"
	sys.instances = []SQLInstance{
		{Name: "WIZSOFT", ID: "MSSQL15.WIZSOFT", TCPEnabled: true, DynamicPort: 63501, MixedAuth: true, Service: "MSSQL$WIZSOFT"},
		{Name: "MSSQLSERVER", ID: "MSSQL16.MSSQLSERVER", Service: "MSSQLSERVER"},
		{Name: "SQLEXPRESS", ID: "MSSQL16.SQLEXPRESS", Service: "MSSQL$SQLEXPRESS"},
	}
	sys.listeners = []Listener{
		{Address: "0.0.0.0", Port: 63501, PID: 900, Process: "sqlservr.exe"},
		{Address: "127.0.0.1", Port: 8080, PID: 4242, Process: "digi-erp-connectord.exe"},
		{Address: "0.0.0.0", Port: 5985, PID: 4, Process: "System"},
		{Address: "0.0.0.0", Port: 3389, PID: 1100, Process: "svchost.exe"},
	}
	sys.candidates = []string{`C:\Hash7\has.exe`}
	fdb.databases = []DatabaseFact{{Name: "erp_db", ERP: "hasavshevet"}}
	env := testEnv(sys, fdb)

	f, err := Discover(context.Background(), env, DiscoverInput{DB: &DiscoverDB{User: "sa", Password: testSaPass}})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var codes []string
	for _, d := range f.Defects {
		codes = append(codes, d.Code)
	}
	slices.Sort(codes)
	want := []string{DefectConfigNewerThanStart, DefectDynamicPortPinned, DefectLoopbackListen, DefectNoImporter, DefectSaLogin}
	slices.Sort(want)
	if !slices.Equal(codes, want) {
		t.Errorf("defects = %v, want %v", codes, want)
	}
	if !hasNote(f, "firewall_profiles_off") {
		t.Errorf("notes = %+v: the firewall profiles are all off", f.Notes)
	}
	if got := f.SQL.Instances[0]; !slices.Equal(got.Flags, []string{"dynamic_port"}) || !slices.Equal(got.ListeningPorts, []int{63501}) {
		t.Errorf("WIZSOFT = %+v", got)
	}
	if got := f.SQL.Instances[1].Flags; !slices.Contains(got, "tcp_disabled") || !slices.Contains(got, "windows_auth_only") {
		t.Errorf("MSSQLSERVER flags = %v", got)
	}
	if len(f.Databases) != 1 || f.Databases[0].ERP != "hasavshevet" || f.DatabaseError != "" {
		t.Errorf("databases = %+v (%s)", f.Databases, f.DatabaseError)
	}
	for _, l := range f.Network.Listening {
		if l.Port == 3389 {
			t.Error("RDP is not one of the listeners that matter here")
		}
	}

	s := f.Suggested
	if s.DBHost != `127.0.0.1\WIZSOFT` || s.DBPort != 0 || s.APIListen != "0.0.0.0:8080" || !s.LeastPriv ||
		s.Importer == nil || s.Importer.Mode != "none" || !slices.Equal(s.DependOn, []string{"MSSQL$WIZSOFT"}) {
		t.Errorf("suggested = %+v", s)
	}

	// Nothing secret is in the facts: the credential is "set".
	if f.Connector.Config.AuthPassword != "set" || f.Connector.DBSecret != "set" {
		t.Errorf("config facts = %+v", f.Connector.Config)
	}
	out, _ := json.Marshal(f)
	for _, secret := range []string{testSaPass, "typed-in-the-gui-on-10-19", strings.Repeat("ab", 32)} {
		if strings.Contains(string(out), secret) {
			t.Errorf("the facts carry a secret")
		}
	}
}

// Without a credential Discover never touches SQL Server, and without a
// config it says the connector is not configured.
func TestDiscover_NoCredentialNoConfig(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	if err := os.Remove(filepath.Join(sys.dir, configFile)); err != nil {
		t.Fatal(err)
	}
	f, err := Discover(context.Background(), testEnv(sys, fdb), DiscoverInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(fdb.opened) != 0 {
		t.Errorf("SQL Server was opened as %v", fdb.opened)
	}
	if !hasNote(f, "no_config") || len(f.Defects) != 0 {
		t.Errorf("defects %+v, notes %+v", f.Defects, f.Notes)
	}
}

// A domain controller is called out: an administrator there administers the
// domain.
func TestDiscover_NamesADomainController(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	sys.host.DomainRole = 5
	f, _ := Discover(context.Background(), testEnv(sys, fdb), DiscoverInput{})
	if !hasNote(f, "domain_controller") {
		t.Errorf("notes = %+v", f.Notes)
	}
}

func hasNote(f *Facts, code string) bool {
	for _, n := range f.Notes {
		if n.Code == code {
			return true
		}
	}
	return false
}
