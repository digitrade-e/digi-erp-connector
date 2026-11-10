package provision

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		{"a name as listen host", func(s *Spec) { s.APIListen = "erp01:8082" }, CodeSpecInvalid},
		{"sa kept without allowSa", func(s *Spec) { s.DB.LeastPrivilege = false }, CodeSaLogin},
		{"the login creating itself", func(s *Spec) { s.DB.User = DefaultLogin }, CodeSpecInvalid},
		{"a login name that is not ours", func(s *Spec) { s.DB.Login = "dbo" }, CodeSpecInvalid},
		{"a login name in capitals", func(s *Spec) { s.DB.Login = "erp_connector_B" }, CodeSpecInvalid},
		{"a login name without least privilege", func(s *Spec) { s.DB.Login, s.DB.LeastPrivilege, s.DB.AllowSa = "erp_connector_b", false, true }, CodeSpecInvalid},
		{"no port and no instance", func(s *Spec) { s.DB.Host = "127.0.0.1" }, CodeSpecInvalid},
		{"an instance name with a quote", func(s *Spec) { s.DB.Host = `127.0.0.1\WIZ'SOFT` }, CodeSpecInvalid},
		{"a database name with a bracket", func(s *Spec) { s.DB.Database = "erp]db" }, CodeSpecInvalid},
		{"an importer mode not known", func(s *Spec) { s.Importer = &ImporterSpec{Mode: "ftp"} }, CodeSpecInvalid},
		{"a relative has.exe", func(s *Spec) { s.Importer = &ImporterSpec{Mode: "hasExe", HasExePath: `Hash7\has.exe`} }, CodeSpecInvalid},
		{"orders for SAP", func(s *Spec) { s.ERP = "sap" }, CodeSpecInvalid},
		{"a short credential", func(s *Spec) { s.Auth.Password = "123456" }, CodeAuthPasswordWeak},
		{"a credential with a newline", func(s *Spec) { s.Auth.Password = strings.Repeat("x", 20) + "\n" }, CodeAuthPasswordWeak},
		{"the whole internet", func(s *Spec) { s.Firewall.AllowFrom = []string{"0.0.0.0/0"} }, CodeFirewallAny},
		{"an IPv6 source", func(s *Spec) { s.Firewall.AllowFrom = []string{"::1"} }, CodeSpecInvalid},
		{"an IPv4-mapped CIDR that is all of IPv4", func(s *Spec) { s.Firewall.AllowFrom = []string{"::ffff:8.0.0.0/96"} }, CodeSpecInvalid},
		{"an IPv4-mapped address", func(s *Spec) { s.Firewall.AllowFrom = []string{"::ffff:198.51.100.7"} }, CodeSpecInvalid},
		{"no sources", func(s *Spec) { s.Firewall.AllowFrom = nil }, CodeSpecInvalid},
		{"a dependency that is not SQL Server", func(s *Spec) { s.Service.DependOn = []string{"WinRM"} }, CodeSpecInvalid},
		{"tls with no host", func(s *Spec) { s.TLS = &TLSSpec{Mode: "self-signed"} }, CodeSpecInvalid},
		{"tls with a bad host", func(s *Spec) { s.TLS = &TLSSpec{Mode: "self-signed", Hosts: []string{"a b"}} }, CodeSpecInvalid},
		{"an expected hash that is not one", func(s *Spec) { h := "abc"; s.Expect = &Expect{ConfigSHA256: &h} }, CodeSpecInvalid},
	}
	// Absolute on every OS: "/tmp/orders" is not absolute on Windows.
	orders := filepath.Join(os.TempDir(), "orders")
	if r := handInstallSpec(orders).Validate(); r != nil {
		t.Fatalf("the pilot spec itself: %v", r)
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := handInstallSpec(orders)
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
	batches, err := leastPrivilegeBatches(DefaultLogin, "erp_db", pw)
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(batches, "\n")
	for _, want := range []string{
		"CREATE LOGIN [erp_connector_read] WITH PASSWORD = N'" + pw + "', CHECK_POLICY = ON, DEFAULT_DATABASE = [erp_db]",
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
		if !strings.HasPrefix(b, "IF DB_NAME() <> N'erp_db' THROW") {
			t.Errorf("database-scoped batch %d does not check it runs in erp_db", i+2)
		}
	}
	for _, s := range []string{"GO", "USE "} {
		if strings.Contains(all, s+"\n") || strings.Contains(all, "\n"+s) {
			t.Errorf("%q is not something database/sql can run", s)
		}
	}

	if _, err := leastPrivilegeBatches(DefaultLogin, "erp]; DROP DATABASE x; --", pw); err == nil {
		t.Error("an unsafe database name was written into SQL")
	}
	if _, err := leastPrivilegeBatches(DefaultLogin, "erp_db", "abc'; DROP LOGIN sa; --"); err == nil {
		t.Error("a password outside the alphabet was written into SQL")
	}
	if _, err := leastPrivilegeBatches("erp_connector_x]; DROP LOGIN sa; --", "erp_db", pw); err == nil {
		t.Error("an unsafe login name was written into SQL")
	}
	named, err := leastPrivilegeBatches("erp_connector_node2", "erp_db", pw)
	if err != nil || !strings.Contains(strings.Join(named, "\n"), "CREATE USER [erp_connector_node2] FOR LOGIN [erp_connector_node2]") {
		t.Errorf("a named login is not the one created: %v", err)
	}
}

func TestSelfSignedPair(t *testing.T) {
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	hosts := []string{"203.0.113.58", "erp01.example.local"}
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
	if err := c.VerifyHostname("203.0.113.58"); err != nil {
		t.Errorf("the public IP: %v", err)
	}
	if err := c.VerifyHostname("erp01.example.local"); err != nil {
		t.Errorf("the DNS name: %v", err)
	}
	if !certCovers(certPEM, hosts, now) || !certCovers(certPEM, []string{"erp01.example.local", "203.0.113.58"}, now) {
		t.Error("the pair does not cover its own hosts")
	}
	if certCovers(certPEM, []string{"203.0.113.58"}, now) || certCovers(certPEM, append(hosts, "10.0.0.1"), now) {
		t.Error("a different host list was covered")
	}
	if certCovers(certPEM, hosts, now.Add(certValidity-certRenewBefore/2)) {
		t.Error("a pair inside its last year was kept")
	}
}

// Redact knows a secret as given and as a URL escapes it — a driver's parse
// error quotes the DSN, password URL-escaped in its userinfo.
func TestRedact(t *testing.T) {
	secret := "P@ss word!x"
	for _, in := range []string{
		`parse "sqlserver://sa:P%40ss%20word%21x@my%20host:1433": invalid character`,
		"login failed: P@ss word!x",
		"q=P%40ss+word%21x",
	} {
		if out := Redact(in, secret); strings.Contains(out, "word") {
			t.Errorf("%q survived as %q", in, out)
		}
	}
}

// A short secret — "1", "true", a port — must not touch the JSON's shape:
// redaction runs inside string values only.
func TestRedactJSON_KeepsTheShape(t *testing.T) {
	in := []byte(`{"ok":true,"port":8082,"n":1,"msg":"pass 1 is true at 8082","list":["true"]}`)
	out, err := redactJSON(in, "true", "1", "8082")
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("not JSON any more: %s", out)
	}
	if v["ok"] != true || v["port"] != float64(8082) || v["n"] != float64(1) {
		t.Errorf("a value outside a string changed: %s", out)
	}
	if msg := v["msg"].(string); strings.Contains(msg, "true") || strings.Contains(msg, "8082") {
		t.Errorf("msg = %q", msg)
	}
}

// The firewall reports a /24 as an address and a mask, and a /32 as the bare
// address; a rule is the same however it is written.
func TestCanonicalSources(t *testing.T) {
	got := canonicalSources([]string{"10.1.2.0/255.255.255.0", "198.51.100.241/32", "198.51.100.241", "10.1.2.0/24"})
	if want := []string{"10.1.2.0/24", "198.51.100.241"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	a := FirewallRule{Name: "r", Enabled: true, Port: 8082, RemoteAddress: []string{"198.51.100.241"}}
	b := a
	b.RemoteAddress = []string{"198.51.100.241/255.255.255.255"}
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
		{start.Add(13 * 24 * time.Hour), true}, // the pilot box: saved 13 days later
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

// The output is ASCII whatever the box holds — a Hebrew folder name, an em
// dash, an emoji — and decodes back to the same text. On a Hebrew box, PowerShell
// 5.1 showed the UTF-8 "—" of a warning as "ג€”".
func TestEnvelopeIsASCII(t *testing.T) {
	in := map[string]string{"folder": "C:\\הזמנות", "note": "a — b", "emoji": "ok 👍"}
	out := envelope("discover", in, nil, nil)
	for _, c := range out {
		if c >= 0x80 {
			t.Fatalf("non-ASCII byte in %s", out)
		}
	}
	var back struct{ Result map[string]string }
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	for k, v := range in {
		if back.Result[k] != v {
			t.Errorf("%s = %q, want %q", k, back.Result[k], v)
		}
	}
}

// The proof's client trusts exactly the connector's own pair (no InsecureSkipVerify): its certificate passes, any
// other certificate is refused, and an unreadable one trusts nothing.
func TestPinnedClientTrustsTheConnectorsOwnPair(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()
	own := filepath.Join(t.TempDir(), "cert.pem")
	if err := os.WriteFile(own, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	if res, err := pinnedClient(nil, own).Get(srv.URL); err != nil || res.StatusCode != 204 {
		t.Fatalf("its own certificate: %v", err)
	}
	// httptest's servers all share one built-in certificate: the other one serves a pair of its own.
	certPEM, keyPEM, err := selfSignedPair([]string{"127.0.0.1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	other := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	other.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	other.StartTLS()
	defer other.Close()
	if _, err := pinnedClient(nil, own).Get(other.URL); err == nil {
		t.Error("another certificate was trusted")
	}
	if _, err := pinnedClient(nil, filepath.Join(t.TempDir(), "none.pem")).Get(srv.URL); err == nil {
		t.Error("no certificate trusted everything")
	}
}
