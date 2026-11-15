package link

import (
	"net/http"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

func testMachine(t *testing.T) Machine {
	return Machine{DataDir: t.TempDir(), InstallDir: t.TempDir(), Secrets: &memSecrets{},
		LoadDaemon: func() (config.Config, error) { c := config.Default(); c.APIListen = "[::]:8082"; return c, nil }}
}

func TestPairStoresTheCodeAndShowsTheFingerprint(t *testing.T) {
	m := testMachine(t)
	fp, err := m.Pair("abcde fghjk")
	if err != nil {
		t.Fatal(err)
	}
	st, _ := m.Store().LoadState()
	if st.PairCode != "ABCDE-FGHJK" {
		t.Fatalf("stored %q", st.PairCode)
	}
	k, _ := m.Keys()
	if k.Public().Fingerprint() != fp {
		t.Fatal("pair showed a fingerprint that is not the box's key")
	}
	if _, err := m.Pair("ABCDE"); err == nil {
		t.Fatal("a short code was accepted")
	}
	st.Box = "bfl-read"
	_ = m.Store().SaveState(st)
	if _, err := m.Pair("ABCDE-FGHJK"); err == nil || !strings.Contains(err.Error(), "already paired") {
		t.Fatalf("a paired box took a new code: %v", err)
	}
	k2, _ := m.Keys()
	if !k2.Public().Equal(k.Public()) {
		t.Fatal("the box's keys changed between calls")
	}
}

func TestConfigReachesTheDaemonOnLoopback(t *testing.T) {
	m := testMachine(t)
	cfg, err := m.Config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIAddr != "127.0.0.1:8082" {
		t.Fatalf("APIAddr %q", cfg.APIAddr)
	}
	for in, want := range map[string]string{"0.0.0.0:8083": "127.0.0.1:8083", "127.0.0.1:8080": "127.0.0.1:8080", "192.168.0.5:8082": "192.168.0.5:8082"} {
		if got := LocalAddr(in); got != want {
			t.Errorf("LocalAddr(%q) = %q", in, got)
		}
	}
}

func TestHTTPClientProxy(t *testing.T) {
	c, err := HTTPClient("http://proxy.b4l.local:3128")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://link.digitrade-admin.org/link", nil)
	u, _ := c.Transport.(*http.Transport).Proxy(req)
	if u == nil || u.Host != "proxy.b4l.local:3128" {
		t.Fatalf("proxy %v", u)
	}
	if _, err := HTTPClient("not a url"); err == nil {
		t.Fatal("a bad proxy was accepted")
	}
}
