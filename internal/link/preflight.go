package link

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// Preflight is what setup.exe checks before it installs (plan 083 §3.4): one
// line per check and, when something is wrong, the exact fix. It finds the
// proxy the way the service will (proxy.go), so a pass here is a pass for the
// service (§9.2 O5). It writes nothing: no keys, no link.yaml.

// Check levels.
const (
	CheckOK   = "OK"
	CheckWarn = "WARN"
	CheckStop = "STOP"
)

// Check is one preflight line.
type Check struct {
	Level  string `json:"level"`
	Name   string `json:"name"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// PreflightEnv is the machine the preflight looks at; nil functions are not checked.
type PreflightEnv struct {
	State     State // link.yaml: its relay and proxy, its box
	Proxy     *ProxyFinder
	Elevated  func() (bool, error)
	OS        func() (desc string, supported bool)
	UserProxy func() string                        // this account's own proxy, which LocalSystem does not see
	Daemon    func() (apiListen string, err error) // the connector's config.yaml; an error = not installed
	Now       func() time.Time
	Timeout   time.Duration // each network check; default 15 s
	// TrustedIssuers name the relay certificate's issuers (organization): any
	// other means a proxy re-signs it on the way (TLS inspection).
	TrustedIssuers []string
	RootCAs        *x509.CertPool // nil = the system's (tests set their own)
	Dial           func(ctx context.Context, network, addr string) (net.Conn, error)
	Resolve        func(ctx context.Context, host string) ([]string, error)
}

// MaxClockSkew is how far this server's clock may be from the relay's: a
// signed command lives five minutes and one issued more than two in the
// future is refused (plan 083 §8 A17).
const MaxClockSkew = 2 * time.Minute

// Preflight runs the checks in order. Network checks after a failed one are skipped.
func Preflight(ctx context.Context, env PreflightEnv) []Check {
	if env.Now == nil {
		env.Now = time.Now
	}
	if env.Timeout == 0 {
		env.Timeout = 15 * time.Second
	}
	if env.TrustedIssuers == nil {
		env.TrustedIssuers = []string{"Google Trust Services", "Let's Encrypt"}
	}
	if env.Dial == nil {
		env.Dial = (&net.Dialer{}).DialContext
	}
	if env.Resolve == nil {
		env.Resolve = net.DefaultResolver.LookupHost
	}
	if env.Proxy == nil {
		env.Proxy = &ProxyFinder{Explicit: env.State.Proxy}
	}
	var out []Check
	add := func(level, name, detail, fix string) { out = append(out, Check{level, name, detail, fix}) }

	if env.Elevated != nil {
		if ok, err := env.Elevated(); err != nil || !ok {
			add(CheckStop, "administrator", "not running as an administrator", "run setup.exe as an administrator (right-click, Run as administrator)")
		} else {
			add(CheckOK, "administrator", "running as an administrator", "")
		}
	}
	if env.OS != nil {
		if desc, ok := env.OS(); ok {
			add(CheckOK, "Windows", desc, "")
		} else {
			add(CheckStop, "Windows", desc+": older than Windows Server 2016", "install on Windows Server 2016 or later")
		}
	}
	switch {
	case env.State.Box != "":
		add(CheckOK, "this server", "paired with Digitrade as "+env.State.Box+": a reinstall keeps the pairing", "")
	case env.Daemon != nil:
		if listen, err := env.Daemon(); err == nil {
			add(CheckOK, "this server", "the connector is installed (API on "+listen+"): an upgrade keeps its config.yaml", "")
		} else {
			add(CheckOK, "this server", "a fresh install", "")
		}
	}

	relay, err := url.Parse(env.State.RelayURL())
	if err != nil || relay.Host == "" {
		add(CheckStop, "relay", fmt.Sprintf("link.yaml relay %q is not a URL", env.State.Relay), "remove relay: from link.yaml")
		return out
	}
	httpsURL := *relay
	httpsURL.Scheme = map[string]string{"wss": "https", "ws": "http"}[relay.Scheme]
	host := relay.Hostname()

	choice, err := env.Proxy.Find(&httpsURL)
	if err != nil {
		add(CheckStop, "proxy", err.Error(), "fix proxy: in link.yaml (like http://proxy.example.local:8080), or remove it")
		return out
	}
	user := ""
	if choice.URL == nil && env.UserProxy != nil {
		user = env.UserProxy()
	}
	if user != "" {
		add(CheckWarn, "proxy", "the service goes "+choice.String()+", but this account's Internet settings use "+user+
			": the service runs as LocalSystem and does not see them",
			"if this server reaches the internet only through that proxy: run `netsh winhttp import proxy source=ie` "+
				"as an administrator, or put proxy: http://<host>:<port> in link.yaml")
	} else {
		add(CheckOK, "proxy", choice.String(), "")
	}

	ctx, cancel := context.WithTimeout(ctx, 4*env.Timeout)
	defer cancel()
	step := func() (context.Context, context.CancelFunc) { return context.WithTimeout(ctx, env.Timeout) }

	ips := ""
	{
		c, cancel := step()
		addrs, err := env.Resolve(c, host)
		cancel()
		switch {
		case err == nil:
			ips = strings.Join(addrs, ", ")
			add(CheckOK, "DNS", host+" resolves to "+ips, "")
		case choice.URL != nil:
			add(CheckWarn, "DNS", "this server cannot resolve "+host+"; fine through the proxy, which resolves it", "")
		default:
			add(CheckStop, "DNS", "this server cannot resolve "+host+": "+err.Error(),
				"let this server resolve "+host+" (public DNS, or a forwarder on the domain's DNS)")
			return out
		}
	}

	{
		addr, name, fix := net.JoinHostPort(host, portOf(relay)), "outbound 443",
			"allow outbound TCP 443 from this server to "+host+orIPs(ips)+": nothing inbound is needed"
		if choice.URL != nil {
			addr, name, fix = choice.URL.Host, "proxy reachable", "this server cannot reach its proxy "+choice.URL.Host+": fix the proxy setting"
		}
		c, cancel := step()
		conn, err := env.Dial(c, "tcp", addr)
		cancel()
		if err != nil {
			add(CheckStop, name, err.Error(), fix)
			return out
		}
		_ = conn.Close()
		add(CheckOK, name, "connected to "+addr, "")
	}

	client, err := HTTPClientVia(env.Proxy)
	if err != nil {
		add(CheckStop, "proxy", err.Error(), "fix proxy: in link.yaml")
		return out
	}
	tr := client.Transport.(*http.Transport)
	tr.DialContext = env.Dial
	tr.TLSClientConfig.RootCAs = env.RootCAs
	{
		c, cancel := step()
		resp, err := httpsGet(c, client, httpsURL.Scheme+"://"+httpsURL.Host+"/healthz")
		cancel()
		if err != nil {
			add(CheckStop, "HTTPS", err.Error(), httpsFix(err, host))
			return out
		}
		_ = resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusProxyAuthRequired:
			add(CheckStop, "HTTPS", "the proxy answered 407: it wants a login", "exempt "+host+" from proxy authentication (the service has no user to log in as)")
			return out
		case resp.StatusCode != http.StatusOK:
			add(CheckStop, "HTTPS", fmt.Sprintf("%s answered %s instead of the relay", host, resp.Status),
				"something on the way (a proxy or a filter) blocks "+host+": allow it")
			return out
		}
		add(CheckOK, "HTTPS", host+" answered", "")
		if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
			iss := resp.TLS.PeerCertificates[0].Issuer
			who := strings.TrimSpace(strings.Join(iss.Organization, " ") + " " + iss.CommonName)
			if trustedIssuer(iss.Organization, env.TrustedIssuers) {
				add(CheckOK, "TLS", "the relay's own certificate (issued by "+who+")", "")
			} else {
				add(CheckWarn, "TLS", "the certificate is issued by "+who+", not the relay's public CA: TLS is inspected on the way, and this server trusts the inspector",
					"fine if the WebSocket check passes; if it fails, exempt "+host+" from TLS inspection")
			}
		}
		if d, err := http.ParseTime(resp.Header.Get("Date")); err != nil {
			add(CheckWarn, "clock", "the relay's answer carried no time to compare with", "")
		} else if skew := env.Now().Sub(d); skew > MaxClockSkew || skew < -MaxClockSkew {
			add(CheckStop, "clock", fmt.Sprintf("this server's clock is %s off: Digitrade's signed commands would be refused", skew.Round(time.Second)),
				"sync the clock (w32tm /resync; a domain member syncs from its domain controller)")
		} else {
			add(CheckOK, "clock", fmt.Sprintf("%s from the relay's", absDur(skew).Round(time.Second)), "")
		}
	}

	{
		c, cancel := step()
		err := wsChallenge(c, client, relay.String())
		cancel()
		if err != nil {
			add(CheckStop, "WebSocket", err.Error(),
				"allow WebSocket (HTTP Upgrade) to "+host+" on the proxy or firewall; the link keeps one connection open")
			return out
		}
		add(CheckOK, "WebSocket", "the relay answered on "+relay.Path, "")
	}
	return out
}

func httpsGet(ctx context.Context, c *http.Client, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}

// wsChallenge opens the link's WebSocket and reads the relay's first message,
// the challenge; it answers nothing and closes.
func wsChallenge(ctx context.Context, c *http.Client, relay string) error {
	ws, _, err := websocket.Dial(ctx, relay, &websocket.DialOptions{HTTPClient: c})
	if err != nil {
		return err
	}
	defer func() { _ = ws.Close(websocket.StatusNormalClosure, "preflight") }()
	var ch linkproto.Challenge
	if err := wsjson.Read(ctx, ws, &ch); err != nil {
		return fmt.Errorf("the WebSocket opened but the relay's first message did not arrive: %w", err)
	}
	return nil
}

func httpsFix(err error, host string) string {
	var unknown x509.UnknownAuthorityError
	if errors.As(err, &unknown) {
		who := "an unknown issuer"
		if unknown.Cert != nil {
			who = strings.TrimSpace(strings.Join(unknown.Cert.Issuer.Organization, " ") + " " + unknown.Cert.Issuer.CommonName)
		}
		return "a proxy inspects TLS and this server does not trust its CA (" + who + "): add that CA to the Local Computer's " +
			"Trusted Root Certification Authorities, or exempt " + host + " from TLS inspection"
	}
	if strings.Contains(err.Error(), "407") || strings.Contains(err.Error(), "Proxy Authentication") {
		return "the proxy wants a login: exempt " + host + " from proxy authentication (the service has no user to log in as)"
	}
	var hostErr x509.HostnameError
	if errors.As(err, &hostErr) {
		return "something answers for " + host + " with another name's certificate: a filter or a captive portal; allow " + host
	}
	return "allow outbound HTTPS from this server to " + host + " (through its proxy, if it has one)"
}

func trustedIssuer(orgs, trusted []string) bool {
	for _, o := range orgs {
		for _, t := range trusted {
			if strings.HasPrefix(o, t) {
				return true
			}
		}
	}
	return false
}

func portOf(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "ws" {
		return "80"
	}
	return "443"
}

func orIPs(ips string) string {
	if ips == "" {
		return ""
	}
	return " (" + ips + ")"
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// FormatChecks is the preflight as setup.exe and a console show it.
func FormatChecks(cs []Check) string {
	var b strings.Builder
	for _, c := range cs {
		fmt.Fprintf(&b, "%-5s %-16s %s\n", c.Level, c.Name, c.Detail)
		if c.Fix != "" {
			fmt.Fprintf(&b, "%-22s fix: %s\n", "", c.Fix)
		}
	}
	switch PreflightExit(cs) {
	case 0:
		b.WriteString("\nReady: this server can reach Digitrade.\n")
	case 3:
		b.WriteString("\nReady, with the warnings above.\n")
	default:
		b.WriteString("\nThe link cannot connect until the STOP lines are fixed. The connector itself installs and works without it.\n")
	}
	return b.String()
}

// PreflightExit is the preflight's exit code: 0 ready, 3 ready with warnings, 1 a STOP.
func PreflightExit(cs []Check) int {
	code := 0
	for _, c := range cs {
		switch c.Level {
		case CheckStop:
			return 1
		case CheckWarn:
			code = 3
		}
	}
	return code
}
