package link

import (
	"context"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// fakeRelayFront answers /healthz and sends a challenge on /link, like connector-link; ws false = a proxy that
// lets HTTPS through but not the WebSocket upgrade.
func fakeRelayFront(ws bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			_, _ = w.Write([]byte("ok"))
		case "/link":
			if !ws {
				http.Error(w, "upgrade not allowed", http.StatusForbidden)
				return
			}
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = c.CloseNow() }()
			_ = wsjson.Write(r.Context(), c, linkproto.Challenge{Nonce: "n"})
			_, _, _ = c.Read(r.Context())
		default:
			http.NotFound(w, r)
		}
	})
}

func tlsRelay(t *testing.T, ws bool) (string, *x509.CertPool) {
	t.Helper()
	srv := httptest.NewTLSServer(fakeRelayFront(ws))
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return "wss://" + srv.Listener.Addr().String() + "/link", pool
}

func checks(cs []Check) map[string]Check {
	m := map[string]Check{}
	for _, c := range cs {
		m[c.Name] = c
	}
	return m
}

func runPreflight(t *testing.T, env PreflightEnv) (map[string]Check, []Check) {
	t.Helper()
	env.Timeout = 3 * time.Second
	if env.Proxy == nil {
		env.Proxy = &ProxyFinder{Explicit: env.State.Proxy, Env: func(*http.Request) (*url.URL, error) { return nil, nil },
			Machine: func(string) (ProxyChoice, error) { return ProxyChoice{Source: "direct"}, nil }}
	}
	cs := Preflight(context.Background(), env)
	t.Log("\n" + FormatChecks(cs))
	return checks(cs), cs
}

func TestPreflightReady(t *testing.T) {
	relay, roots := tlsRelay(t, true)
	m, cs := runPreflight(t, PreflightEnv{State: State{Relay: relay}, RootCAs: roots, TrustedIssuers: []string{"Acme Co"},
		Elevated: func() (bool, error) { return true, nil }, OS: func() (string, bool) { return "Windows 10.0.17763", true },
		Daemon: func() (string, error) { return "0.0.0.0:8082", nil }})
	for _, name := range []string{"administrator", "Windows", "this server", "proxy", "DNS", "outbound 443", "HTTPS", "TLS", "clock", "WebSocket"} {
		if m[name].Level != CheckOK {
			t.Errorf("%s: %+v", name, m[name])
		}
	}
	if PreflightExit(cs) != 0 || !strings.Contains(m["this server"].Detail, "0.0.0.0:8082") {
		t.Fatalf("exit %d, %+v", PreflightExit(cs), m["this server"])
	}
}

// A TLS-inspecting proxy whose CA this server does not trust: the plain-words STOP of plan 083 §10 rule 5.
func TestPreflightUntrustedInspection(t *testing.T) {
	relay, _ := tlsRelay(t, true)
	m, cs := runPreflight(t, PreflightEnv{State: State{Relay: relay}})
	if c := m["HTTPS"]; c.Level != CheckStop || !strings.Contains(c.Fix, "Trusted Root Certification Authorities") || !strings.Contains(c.Fix, "Acme Co") {
		t.Fatalf("HTTPS: %+v", c)
	}
	if _, ran := m["WebSocket"]; ran || PreflightExit(cs) != 1 {
		t.Fatal("checks ran after a STOP, or the exit code is not 1")
	}
}

// Trusted, but not the relay's issuer: inspected on the way. A warning, not a stop.
func TestPreflightTrustedInspection(t *testing.T) {
	relay, roots := tlsRelay(t, true)
	m, cs := runPreflight(t, PreflightEnv{State: State{Relay: relay}, RootCAs: roots})
	if m["TLS"].Level != CheckWarn || !strings.Contains(m["TLS"].Detail, "inspected") || m["WebSocket"].Level != CheckOK || PreflightExit(cs) != 3 {
		t.Fatalf("TLS %+v, WebSocket %+v, exit %d", m["TLS"], m["WebSocket"], PreflightExit(cs))
	}
}

func TestPreflightClock(t *testing.T) {
	relay, roots := tlsRelay(t, true)
	for skew, want := range map[time.Duration]string{0: CheckOK, 90 * time.Second: CheckOK, 3 * time.Minute: CheckStop, -3 * time.Minute: CheckStop} {
		m, _ := runPreflight(t, PreflightEnv{State: State{Relay: relay}, RootCAs: roots, TrustedIssuers: []string{"Acme Co"},
			Now: func() time.Time { return time.Now().Add(skew) }})
		if m["clock"].Level != want || (want == CheckStop && !strings.Contains(m["clock"].Fix, "w32tm")) {
			t.Errorf("skew %s: %+v", skew, m["clock"])
		}
	}
}

func TestPreflightNoOutbound(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	_ = l.Close()
	m, cs := runPreflight(t, PreflightEnv{State: State{Relay: "wss://" + addr + "/link"}})
	if c := m["outbound 443"]; c.Level != CheckStop || !strings.Contains(c.Fix, "outbound TCP 443") || !strings.Contains(c.Fix, "nothing inbound") {
		t.Fatalf("outbound: %+v", c)
	}
	if _, ran := m["HTTPS"]; ran || PreflightExit(cs) != 1 {
		t.Fatal("HTTPS ran after the port was closed")
	}
	if !strings.Contains(FormatChecks(cs), "The connector itself installs and works without it") {
		t.Fatal("the summary does not say the connector still works")
	}
}

func TestPreflightWebSocketBlocked(t *testing.T) {
	relay, roots := tlsRelay(t, false)
	m, _ := runPreflight(t, PreflightEnv{State: State{Relay: relay}, RootCAs: roots, TrustedIssuers: []string{"Acme Co"}})
	if m["HTTPS"].Level != CheckOK || m["WebSocket"].Level != CheckStop || !strings.Contains(m["WebSocket"].Fix, "HTTP Upgrade") {
		t.Fatalf("HTTPS %+v, WebSocket %+v", m["HTTPS"], m["WebSocket"])
	}
}

// A proxy that wants a login: the service, LocalSystem, has no user to give it.
func TestPreflightProxyWantsALogin(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	defer proxy.Close()
	m, _ := runPreflight(t, PreflightEnv{
		State: State{Relay: "ws://link.example.invalid/link", Proxy: proxy.URL},
		Resolve: func(context.Context, string) ([]string, error) {
			return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
		},
	})
	if m["proxy"].Level != CheckOK || !strings.Contains(m["proxy"].Detail, "link.yaml") {
		t.Fatalf("proxy: %+v", m["proxy"])
	}
	if m["DNS"].Level != CheckWarn || m["proxy reachable"].Level != CheckOK {
		t.Fatalf("DNS %+v, proxy reachable %+v", m["DNS"], m["proxy reachable"])
	}
	if c := m["HTTPS"]; c.Level != CheckStop || !strings.Contains(c.Fix, "proxy authentication") {
		t.Fatalf("HTTPS: %+v", c)
	}
	// Over TLS the same 407 comes back as CONNECT's error.
	m, _ = runPreflight(t, PreflightEnv{
		State: State{Relay: "wss://link.example.invalid/link", Proxy: proxy.URL},
		Resolve: func(context.Context, string) ([]string, error) {
			return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
		},
	})
	if c := m["HTTPS"]; c.Level != CheckStop || !strings.Contains(c.Fix, "proxy authentication") {
		t.Fatalf("HTTPS over CONNECT: %+v", c)
	}
}

// The administrator's browser goes through a proxy the service will not see (O5).
func TestPreflightNamesTheUsersOwnProxy(t *testing.T) {
	relay, roots := tlsRelay(t, true)
	m, _ := runPreflight(t, PreflightEnv{State: State{Relay: relay}, RootCAs: roots, TrustedIssuers: []string{"Acme Co"},
		UserProxy: func() string { return "PAC file http://wpad.b4l.local/proxy.pac" }})
	if c := m["proxy"]; c.Level != CheckWarn || !strings.Contains(c.Detail, "LocalSystem") || !strings.Contains(c.Fix, "netsh winhttp import proxy source=ie") {
		t.Fatalf("proxy: %+v", c)
	}
}

func TestPreflightHostChecks(t *testing.T) {
	relay, roots := tlsRelay(t, true)
	m, cs := runPreflight(t, PreflightEnv{State: State{Relay: relay, Box: "bfl-read"}, RootCAs: roots, TrustedIssuers: []string{"Acme Co"},
		Elevated: func() (bool, error) { return false, nil }, OS: func() (string, bool) { return "Windows 6.3.9600", false }})
	if m["administrator"].Level != CheckStop || m["Windows"].Level != CheckStop || PreflightExit(cs) != 1 {
		t.Fatalf("admin %+v, Windows %+v", m["administrator"], m["Windows"])
	}
	if !strings.Contains(m["this server"].Detail, "bfl-read") {
		t.Fatalf("this server: %+v", m["this server"])
	}
}

// A web filter answering for the relay (403, a block page): a STOP, not a pass.
func TestPreflightAFilterAnswersInstead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "blocked by policy", http.StatusForbidden)
	}))
	defer srv.Close()
	m, _ := runPreflight(t, PreflightEnv{State: State{Relay: "ws://" + srv.Listener.Addr().String() + "/link"}})
	if c := m["HTTPS"]; c.Level != CheckStop || !strings.Contains(c.Detail, "403") {
		t.Fatalf("HTTPS: %+v", c)
	}
	if _, ran := m["WebSocket"]; ran {
		t.Fatal("WebSocket ran after the filter answered")
	}
}
