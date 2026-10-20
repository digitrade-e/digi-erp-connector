package provision

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSpecValidate_Refusals(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Spec)
		code   string
	}{
		{"version", func(s *Spec) { s.Version = 2 }, CodeSpecInvalid},
		{"change set id", func(s *Spec) { s.ChangeSet = "../../x" }, CodeChangeSetID},
		{"loopback listen", func(s *Spec) { s.APIListen = "127.0.0.1:8082" }, CodeLoopbackListen},
		{"a name as listen host", func(s *Spec) { s.APIListen = "tsdc:8082" }, CodeSpecInvalid},
		{"sa kept without allowSa", func(s *Spec) { s.DB.LeastPrivilege = false }, CodeSaLogin},
		{"the login creating itself", func(s *Spec) { s.DB.User = LeastPrivilegeLogin }, CodeSpecInvalid},
		{"no port and no instance", func(s *Spec) { s.DB.Host = "127.0.0.1" }, CodeSpecInvalid},
		{"an instance name with a quote", func(s *Spec) { s.DB.Host = `127.0.0.1\WIZ'SOFT` }, CodeSpecInvalid},
		{"a database name with a bracket", func(s *Spec) { s.DB.Database = "groopy]new" }, CodeSpecInvalid},
		{"orders with no importer named", func(s *Spec) { s.Importer = nil }, CodeImporterRequired},
		{"an importer mode not known", func(s *Spec) { s.Importer = &ImporterSpec{Mode: "ftp"} }, CodeSpecInvalid},
		{"a relative has.exe", func(s *Spec) { s.Importer = &ImporterSpec{Mode: "hasExe", HasExePath: `Hash7\has.exe`} }, CodeSpecInvalid},
		{"orders for SAP", func(s *Spec) { s.ERP = "sap" }, CodeSpecInvalid},
		{"a short credential", func(s *Spec) { s.Auth.Password = "123456" }, CodeAuthPasswordWeak},
		{"a credential with a newline", func(s *Spec) { s.Auth.Password = strings.Repeat("x", 20) + "\n" }, CodeAuthPasswordWeak},
		{"the whole internet", func(s *Spec) { s.Firewall.AllowFrom = []string{"0.0.0.0/0"} }, CodeFirewallAny},
		{"an IPv6 source", func(s *Spec) { s.Firewall.AllowFrom = []string{"::1"} }, CodeSpecInvalid},
		{"no sources", func(s *Spec) { s.Firewall.AllowFrom = nil }, CodeSpecInvalid},
		{"a dependency that is not SQL Server", func(s *Spec) { s.Service.DependOn = []string{"WinRM"} }, CodeSpecInvalid},
		{"tls with no host", func(s *Spec) { s.TLS = &TLSSpec{Mode: "self-signed"} }, CodeSpecInvalid},
		{"tls with a bad host", func(s *Spec) { s.TLS = &TLSSpec{Mode: "self-signed", Hosts: []string{"a b"}} }, CodeSpecInvalid},
		{"an expected hash that is not one", func(s *Spec) { h := "abc"; s.Expect = &Expect{ConfigSHA256: &h} }, CodeSpecInvalid},
	}
	if r := groopySpec("/tmp/orders").Validate(); r != nil {
		t.Fatalf("the GROOPY spec itself: %v", r)
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := groopySpec("/tmp/orders")
			tc.mutate(&s)
			r := s.Validate()
			if r == nil || r.Code != tc.code {
				t.Fatalf("got %v, want %s", r, tc.code)
			}
			if r.Fix == "" {
				t.Error("a refusal without a fix line")
			}
			for _, secret := range s.secrets() {
				if strings.Contains(r.Message, secret) {
					t.Error("the refusal echoes a secret")
				}
			}
		})
	}
}

// Unknown fields and trailing data are refused: a misspelt key must not be
// silently ignored.
func TestDecodeSpec_IsStrict(t *testing.T) {
	for _, in := range []string{
		`{"version":1,"leastPrivilage":true}`,
		`{"version":1} {"version":1}`,
		`[]`,
	} {
		if _, r := DecodeSpec(strings.NewReader(in)); r == nil || r.Code != CodeSpecInvalid {
			t.Errorf("%s: got %v", in, r)
		}
	}
}

func TestNewSQLPassword(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		p, err := NewSQLPassword()
		if err != nil {
			t.Fatal(err)
		}
		if len(p) != sqlPasswordLength || strings.Trim(p, sqlPasswordAlphabet) != "" || !hasEveryClass(p) {
			t.Fatalf("%q: wrong length, alphabet or classes", p)
		}
		if seen[p] {
			t.Fatal("a password repeated")
		}
		seen[p] = true
	}
}

func TestLeastPrivilegeBatches(t *testing.T) {
	pw, _ := NewSQLPassword()
	batches, err := leastPrivilegeBatches("groopy_new", pw)
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(batches, "\n")
	for _, want := range []string{
		"CREATE LOGIN [erp_connector_read] WITH PASSWORD = N'" + pw + "', CHECK_POLICY = ON, DEFAULT_DATABASE = [groopy_new]",
		"ALTER LOGIN [erp_connector_read] WITH PASSWORD = N'" + pw + "'",
		"CREATE USER [erp_connector_read] FOR LOGIN [erp_connector_read]",
		"ALTER ROLE [db_datareader] ADD MEMBER [erp_connector_read]",
		"GRANT EXECUTE ON [dbo].[GPRICE_Bulk] TO [erp_connector_read]",
		"GRANT EXECUTE ON [dbo].[GetOnHandStockForSkus] TO [erp_connector_read]",
		"DENY INSERT, UPDATE, DELETE, ALTER, CREATE TABLE TO [erp_connector_read]",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing: %s", want)
		}
	}
	for i, b := range batches[1:] {
		if !strings.HasPrefix(b, "IF DB_NAME() <> N'groopy_new' THROW") {
			t.Errorf("database-scoped batch %d does not check it runs in groopy_new", i+2)
		}
	}
	for _, s := range []string{"GO", "USE "} {
		if strings.Contains(all, s+"\n") || strings.Contains(all, "\n"+s) {
			t.Errorf("%q is not something database/sql can run", s)
		}
	}

	if _, err := leastPrivilegeBatches("groopy]; DROP DATABASE x; --", pw); err == nil {
		t.Error("an unsafe database name was written into SQL")
	}
	if _, err := leastPrivilegeBatches("groopy_new", "abc'; DROP LOGIN sa; --"); err == nil {
		t.Error("a password outside the alphabet was written into SQL")
	}
}

func TestSelfSignedPair(t *testing.T) {
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	hosts := []string{"45.93.93.58", "tsdc.groopyltd.local"}
	certPEM, keyPEM, err := selfSignedPair(hosts, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		t.Fatalf("the key does not match the certificate: %v", err)
	}
	block, _ := pem.Decode(certPEM)
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	// erp-manager checks the host name: the public IP must be an IP SAN.
	if err := c.VerifyHostname("45.93.93.58"); err != nil {
		t.Errorf("the public IP: %v", err)
	}
	if err := c.VerifyHostname("tsdc.groopyltd.local"); err != nil {
		t.Errorf("the DNS name: %v", err)
	}
	if !certCovers(certPEM, hosts, now) || !certCovers(certPEM, []string{"tsdc.groopyltd.local", "45.93.93.58"}, now) {
		t.Error("the pair does not cover its own hosts")
	}
	if certCovers(certPEM, []string{"45.93.93.58"}, now) || certCovers(certPEM, append(hosts, "10.0.0.1"), now) {
		t.Error("a different host list was covered")
	}
	if certCovers(certPEM, hosts, now.Add(certValidity-certRenewBefore/2)) {
		t.Error("a pair inside its last year was kept")
	}
}

func TestRedact(t *testing.T) {
	out := string(Redact([]byte(`{"m":"login failed for sqlserver://sa:p\"w@host and p\"w again; short abc"}`), `p"w`, "", "abc"))
	if strings.Contains(out, `p\"w`) || strings.Contains(out, "abc") {
		t.Errorf("a secret survived: %s", out)
	}
	if strings.Count(out, Redacted) != 3 {
		t.Errorf("redactions = %d, want 3: %s", strings.Count(out, Redacted), out)
	}
}

// The firewall reports a /24 as an address and a mask, and a /32 as the bare
// address; a rule is the same however it is written.
func TestCanonicalSources(t *testing.T) {
	got := canonicalSources([]string{"10.1.2.0/255.255.255.0", "34.154.127.241/32", "34.154.127.241", "10.1.2.0/24"})
	if want := []string{"10.1.2.0/24", "34.154.127.241"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	a := FirewallRule{Name: "r", Enabled: true, Port: 8082, RemoteAddress: []string{"34.154.127.241"}}
	b := a
	b.RemoteAddress = []string{"34.154.127.241/255.255.255.255"}
	if !a.Same(b) {
		t.Error("the same rule, written two ways, differs")
	}
	b.Port = 8083
	if a.Same(b) {
		t.Error("another port is the same rule")
	}
}

func TestLocalAddr(t *testing.T) {
	for in, want := range map[string]string{
		"0.0.0.0:8082":   "127.0.0.1:8082",
		"[::]:8082":      "127.0.0.1:8082",
		"10.0.0.5:8082":  "10.0.0.5:8082",
		"127.0.0.1:8080": "127.0.0.1:8080",
	} {
		if got := LocalAddr(in); got != want {
			t.Errorf("LocalAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

// The daemon writes config.yaml itself just after its first start (it
// generates the signing secret): that is the config it runs, not a stale one.
func TestConfigStale(t *testing.T) {
	start := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		modified time.Time
		want     bool
	}{
		{start.Add(5 * time.Second), false},    // the daemon's own write
		{start.Add(-time.Hour), false},         // older than the process
		{start.Add(13 * 24 * time.Hour), true}, // GROOPY: saved on 10-19
		{time.Time{}, false},                   // no file
	} {
		if got := configStale(tc.modified, start); got != tc.want {
			t.Errorf("configStale(%v) = %v, want %v", tc.modified, got, tc.want)
		}
	}
	if configStale(start.Add(time.Hour), time.Time{}) {
		t.Error("a service that is not running cannot run a stale config")
	}
}
