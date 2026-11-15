package link

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// How the link finds its proxy (plan 083 §9.2 O5). The service runs as
// LocalSystem, which has no per-user Internet settings, so it reads only what
// is the same for every account — and the installer's preflight, run as the
// logged-in administrator, reads the same things in the same order:
//
//  1. link.yaml's proxy (the client's IT, by hand);
//  2. HTTPS_PROXY in the service's environment;
//  3. the machine's WinHTTP proxy (`netsh winhttp show proxy`);
//  4. WPAD (`wpad.<domain>` or DHCP), the way WinHTTP auto-detects it;
//  5. direct.
//
// An administrator's own browser proxy (IE / Edge settings, a PAC file set by
// policy) is not on the list: the preflight names it when it differs, with the
// fix (`netsh winhttp import proxy source=ie`, or link.yaml's proxy).

// ProxyChoice is where the link goes for one URL: URL nil means direct.
type ProxyChoice struct {
	URL    *url.URL
	Source string // "link.yaml", "HTTPS_PROXY", "netsh winhttp", "WPAD", "direct"
}

func (c ProxyChoice) String() string {
	if c.URL == nil {
		return "direct (" + c.Source + ")"
	}
	return c.URL.Host + " (" + c.Source + ")"
}

// MachineProxy asks the operating system (steps 3 and 4) for target's proxy.
type MachineProxy func(target string) (ProxyChoice, error)

// ProxyFinder resolves the proxy for the relay's URLs, caching the machine's
// answer for ten minutes (WPAD can take seconds; the link reconnects rarely).
type ProxyFinder struct {
	Explicit string                                // link.yaml's proxy
	Machine  MachineProxy                          // nil = this machine's (machineProxy)
	Env      func(*http.Request) (*url.URL, error) // nil = http.ProxyFromEnvironment
	Log      Logger                                // says once which way the link goes, and again when it changes

	mu    sync.Mutex
	cache map[string]cachedChoice
	last  string
}

type cachedChoice struct {
	c  ProxyChoice
	at time.Time
}

const proxyCacheFor = 10 * time.Minute

// Find returns the proxy for target.
func (f *ProxyFinder) Find(target *url.URL) (ProxyChoice, error) {
	c, err := f.find(target)
	if err == nil && f.Log != nil {
		f.mu.Lock()
		changed := c.String() != f.last
		f.last = c.String()
		f.mu.Unlock()
		if changed {
			f.Log.Info("link: reaching " + target.Host + " " + c.String())
		}
	}
	return c, err
}

func (f *ProxyFinder) find(target *url.URL) (ProxyChoice, error) {
	if f.Explicit != "" {
		u, err := parseProxyURL(f.Explicit)
		if err != nil {
			return ProxyChoice{}, err
		}
		return ProxyChoice{URL: u, Source: "link.yaml"}, nil
	}
	if ip := net.ParseIP(target.Hostname()); ip != nil && ip.IsLoopback() || strings.EqualFold(target.Hostname(), "localhost") {
		return ProxyChoice{Source: "loopback"}, nil
	}
	env := f.Env
	if env == nil {
		env = http.ProxyFromEnvironment
	}
	if u, err := env(&http.Request{URL: target}); err != nil || u != nil {
		return ProxyChoice{URL: u, Source: "HTTPS_PROXY"}, err
	}
	key := target.Scheme + "://" + target.Host
	f.mu.Lock()
	if cc, ok := f.cache[key]; ok && time.Since(cc.at) < proxyCacheFor {
		f.mu.Unlock()
		return cc.c, nil
	}
	f.mu.Unlock()
	m := f.Machine
	if m == nil {
		m = machineProxy
	}
	c, err := m(target.String())
	if err != nil {
		return ProxyChoice{}, err
	}
	f.mu.Lock()
	if f.cache == nil {
		f.cache = map[string]cachedChoice{}
	}
	f.cache[key] = cachedChoice{c: c, at: time.Now()}
	f.mu.Unlock()
	return c, nil
}

// ProxyFunc is Find for an http.Transport.
func (f *ProxyFinder) ProxyFunc(req *http.Request) (*url.URL, error) {
	c, err := f.Find(req.URL)
	return c.URL, err
}

func parseProxyURL(s string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("link.yaml proxy %q is not a URL like http://proxy.example.local:8080", s)
	}
	return u, nil
}

// ParseWinHTTPProxy reads a WinHTTP proxy list ("proxy:8080",
// "http=p1:80;https=p2:8080", "https://p:443") and returns the proxy for an
// https URL, the way WinHTTP picks it: the https= entry, else the first entry
// without a scheme; nil when only other schemes are named.
func ParseWinHTTPProxy(list string) *url.URL {
	var plain string
	for _, e := range splitProxyList(list) {
		scheme, rest, hasScheme := strings.Cut(e, "=")
		switch {
		case !hasScheme:
			if plain == "" {
				plain = e
			}
		case strings.EqualFold(scheme, "https"):
			return proxyHostURL(rest)
		}
	}
	if plain == "" {
		return nil
	}
	return proxyHostURL(plain)
}

func proxyHostURL(e string) *url.URL {
	scheme := "http"
	if s, rest, ok := strings.Cut(e, "://"); ok {
		if strings.EqualFold(s, "https") {
			scheme = "https"
		}
		e = rest
	}
	e = strings.TrimRight(e, "/")
	if e == "" {
		return nil
	}
	if _, _, err := net.SplitHostPort(e); err != nil {
		e = net.JoinHostPort(e, "80") // WinHTTP's default proxy port
	}
	return &url.URL{Scheme: scheme, Host: e}
}

// Bypassed reports whether host is in a WinHTTP bypass list ("<local>;*.corp;10.*").
func Bypassed(host, list string) bool {
	host = strings.ToLower(host)
	for _, e := range splitProxyList(list) {
		e = strings.ToLower(e)
		if e == "<local>" {
			if !strings.Contains(host, ".") {
				return true
			}
			continue
		}
		re := "^" + strings.ReplaceAll(regexp.QuoteMeta(e), `\*`, ".*") + "$"
		if ok, _ := regexp.MatchString(re, host); ok {
			return true
		}
	}
	return false
}

func splitProxyList(list string) []string {
	return strings.FieldsFunc(list, func(r rune) bool { return r == ';' || r == ' ' || r == '\t' || r == '\r' || r == '\n' })
}
