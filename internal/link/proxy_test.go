package link

import (
	"net/http"
	"net/url"
	"testing"
)

func TestParseWinHTTPProxy(t *testing.T) {
	for list, want := range map[string]string{
		"proxy.b4l.local:3128":                   "http://proxy.b4l.local:3128",
		"proxy.b4l.local":                        "http://proxy.b4l.local:80",
		"http=p1:80;https=p2:8443":               "http://p2:8443",
		"http=p1:80 https=p2:8443":               "http://p2:8443",
		"https=https://p2:443":                   "https://p2:443",
		"http=p1:80;ftp=f:21":                    "", // WinHTTP goes direct for https when only other schemes are named
		"socks=s:1080;plain:3128":                "http://plain:3128",
		"p1:3128;p2:3128":                        "http://p1:3128", // a PAC answer's list: the first
		"http://proxy.b4l.local:8080/":           "http://proxy.b4l.local:8080",
		"":                                       "",
		"[2001:db8::1]:3128":                     "http://[2001:db8::1]:3128",
		"HTTPS=proxy.b4l.local:8080;http=x:1234": "http://proxy.b4l.local:8080",
	} {
		got := ""
		if u := ParseWinHTTPProxy(list); u != nil {
			got = u.String()
		}
		if got != want {
			t.Errorf("%q: %q, want %q", list, got, want)
		}
	}
}

func TestBypassed(t *testing.T) {
	cases := []struct {
		host, list string
		want       bool
	}{
		{"link.digitrade-admin.org", "<local>", false},
		{"sqlsrv", "<local>", true},
		{"link.digitrade-admin.org", "*.digitrade-admin.org", true},
		{"link.digitrade-admin.org", "<local>;*.b4l.local;10.*", false},
		{"LINK.digitrade-admin.org", "link.DIGITRADE-admin.org", true},
		{"evil-digitrade-admin.org", "*.digitrade-admin.org", false},
		{"linkXdigitrade-admin.org", "link.digitrade-admin.org", false}, // a dot is a dot, not any character
		{"10.0.0.5", "10.*", true},
		{"evillink.digitrade-admin.org", "link.digitrade-admin.org", false},          // anchored at the start
		{"link.digitrade-admin.org.evil.example", "link.digitrade-admin.org", false}, // and at the end
		{"link.digitrade-admin.org", "", false},
	}
	for _, c := range cases {
		if got := Bypassed(c.host, c.list); got != c.want {
			t.Errorf("%s in %q: %v", c.host, c.list, got)
		}
	}
}

func TestProxyFinderOrder(t *testing.T) {
	relay, _ := url.Parse("https://link.digitrade-admin.org/link")
	machineCalls := 0
	machine := func(string) (ProxyChoice, error) {
		machineCalls++
		u, _ := url.Parse("http://wpad-proxy:8080")
		return ProxyChoice{URL: u, Source: "WPAD"}, nil
	}
	noEnv := func(*http.Request) (*url.URL, error) { return nil, nil }

	// link.yaml beats everything, and the machine is not asked.
	f := &ProxyFinder{Explicit: "http://proxy.b4l.local:3128", Machine: machine, Env: noEnv}
	if c, err := f.Find(relay); err != nil || c.URL.Host != "proxy.b4l.local:3128" || c.Source != "link.yaml" || machineCalls != 0 {
		t.Fatalf("link.yaml: %v %v, %d machine calls", c, err, machineCalls)
	}
	// A loopback relay (tests, a local run) is never proxied.
	lo, _ := url.Parse("ws://127.0.0.1:9/link")
	if c, _ := (&ProxyFinder{Machine: machine, Env: noEnv}).Find(lo); c.URL != nil || machineCalls != 0 {
		t.Fatalf("loopback: %v", c)
	}
	// Then the machine's answer, cached.
	f = &ProxyFinder{Machine: machine, Env: noEnv, Log: &testLog{}}
	for range 3 {
		if c, err := f.Find(relay); err != nil || c.URL.Host != "wpad-proxy:8080" || c.Source != "WPAD" {
			t.Fatalf("machine: %v %v", c, err)
		}
	}
	if machineCalls != 1 {
		t.Fatalf("the machine was asked %d times in a row; WPAD can take seconds", machineCalls)
	}
	lines := f.Log.(*testLog).lines
	if len(lines) != 1 || lines[0] != "INFO link: reaching link.digitrade-admin.org wpad-proxy:8080 (WPAD)" {
		t.Fatalf("the log says which way once: %q", lines)
	}
	// A bad link.yaml proxy is an error, not a silent direct connection.
	if _, err := (&ProxyFinder{Explicit: "proxy.b4l.local:3128"}).Find(relay); err == nil {
		t.Fatal("a proxy without a scheme passed")
	}
	if _, err := HTTPClient("ftp://x:21"); err == nil {
		t.Fatal("an ftp proxy passed")
	}
}

// HTTPS_PROXY in the service's environment comes before the machine's setting.
func TestProxyFinderTakesTheEnvironment(t *testing.T) {
	relay, _ := url.Parse("https://link.digitrade-admin.org/link")
	called := false
	f := &ProxyFinder{
		Env:     func(*http.Request) (*url.URL, error) { return url.Parse("http://env-proxy:3128") },
		Machine: func(string) (ProxyChoice, error) { called = true; return ProxyChoice{}, nil },
	}
	c, err := f.Find(relay)
	if err != nil || c.URL == nil || c.URL.Host != "env-proxy:3128" || c.Source != "HTTPS_PROXY" || called {
		t.Fatalf("env: %v %v, machine asked: %v", c, err, called)
	}
}
