package link

import (
	"bufio"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// fakeConfig is config.yaml as the watch loads it: the test changes it and touches the file the watch stats.
type fakeConfig struct {
	mu   sync.Mutex
	cfg  config.Config
	err  error
	path string
	n    int
}

func newFakeConfig(t *testing.T, listen string) *fakeConfig {
	c := &fakeConfig{path: filepath.Join(t.TempDir(), "config.yaml")}
	c.set(t, func(cfg *config.Config) { cfg.APIListen = listen })
	return c
}

func (c *fakeConfig) load() (config.Config, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg, c.err
}

func (c *fakeConfig) set(t *testing.T, f func(*config.Config)) {
	t.Helper()
	c.mu.Lock()
	f(&c.cfg)
	c.n++
	c.mu.Unlock()
	// a different size each time, so the stat sees the change whatever the clock's resolution
	if err := os.WriteFile(c.path, []byte(strings.Repeat("x", c.n)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func namedAPI(t *testing.T, name string) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, name) }))
	t.Cleanup(s.Close)
	return s
}

// get sends one request over a fresh data stream and returns the body ("" when the stream was closed unanswered).
func get(t *testing.T, rc *relayConn) string {
	t.Helper()
	st, err := rc.sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	_ = st.SetDeadline(time.Now().Add(10 * time.Second))
	_, _ = st.Write([]byte{linkproto.StreamData})
	_, _ = st.Write([]byte("GET /api/ping HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n"))
	resp, err := http.ReadResponse(bufio.NewReader(st), nil)
	if err != nil {
		return ""
	}
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

// An apply that moves the daemon's port reaches the data path at the next stream, with no restart of the link
// (plan 086 C5) — and until the daemon restarts on it, the old address still answers.
func TestADataStreamFollowsTheDaemonsAddress(t *testing.T) {
	one, two := namedAPI(t, "one"), namedAPI(t, "two")
	fc := newFakeConfig(t, one.Listener.Addr().String())
	watch := &DaemonWatch{Load: fc.load, ConfigPath: fc.path, Log: &testLog{}}
	r := newRelay(t)
	_, kr := team(t, 1)
	_, rc := pairedBox(t, r, "bfl-read", kr, "", func(c *Config) { c.Daemon = watch })
	if got := get(t, rc); got != "one" {
		t.Fatalf("first stream: %q", got)
	}
	fc.set(t, func(c *config.Config) { c.APIListen = two.Listener.Addr().String() })
	if got := get(t, rc); got != "two" {
		t.Fatalf("after the port moved: %q", got)
	}
	// config.yaml names a port nothing listens on yet (saved, not restarted): the last address answers
	idle := httptest.NewServer(http.NotFoundHandler())
	idleAddr := idle.Listener.Addr().String()
	idle.Close()
	fc.set(t, func(c *config.Config) { c.APIListen = idleAddr })
	if got := get(t, rc); got != "two" {
		t.Fatalf("a port nothing listens on: %q", got)
	}
	if !watch.Log.(*testLog).has("now at " + idleAddr) {
		t.Fatal("the change was not logged")
	}
}

// TLS turned on by an apply (a new self-signed pair) is picked up the same way, trusting only the new certificate.
func TestADataStreamFollowsTLS(t *testing.T) {
	plain := namedAPI(t, "plain")
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "tls") }))
	t.Cleanup(secure.Close)
	certFile := filepath.Join(t.TempDir(), "cert.pem")
	_ = os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: secure.Certificate().Raw}), 0o600)
	fc := newFakeConfig(t, plain.Listener.Addr().String())
	watch := &DaemonWatch{Load: fc.load, ConfigPath: fc.path, Log: &testLog{}}
	r := newRelay(t)
	_, kr := team(t, 1)
	_, rc := pairedBox(t, r, "bfl-read", kr, "", func(c *Config) { c.Daemon = watch })
	if got := get(t, rc); got != "plain" {
		t.Fatalf("%q", got)
	}
	fc.set(t, func(c *config.Config) {
		c.APIListen, c.TLS = secure.Listener.Addr().String(), config.TLSConfig{CertFile: certFile, KeyFile: certFile + ".key"}
	})
	if got := get(t, rc); got != "tls" {
		t.Fatalf("after TLS was turned on: %q", got)
	}
	// the plaintext address is never tried again: TLS never falls back to plaintext (review B1)
	if s := watch.Status(); !s.TLS || !s.CertReadable || s.Address != secure.Listener.Addr().String() || s.Previous != "" {
		t.Fatalf("status %+v", s)
	}
}

func tlsCertFile(t *testing.T, s *httptest.Server) string {
	certFile := filepath.Join(t.TempDir(), "cert.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile
}

// Review B1, the reviewer's scenario: TLS turned on, then the daemon is down (any restart) while a local user squats
// the old plaintext port. The stream must not go there in clear — before the fix the link fell back to it.
func TestTLSNeverFallsBackToAPlaintextSquatter(t *testing.T) {
	squatter := namedAPI(t, "plain-squatter") // the old plaintext address, now held by someone else
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "tls") }))
	certFile := tlsCertFile(t, secure)
	secureAddr := secure.Listener.Addr().String()
	fc := newFakeConfig(t, squatter.Listener.Addr().String())
	watch := &DaemonWatch{Load: fc.load, ConfigPath: fc.path, Log: &testLog{}}
	r := newRelay(t)
	_, kr := team(t, 1)
	_, rc := pairedBox(t, r, "bfl-read", kr, "", func(c *Config) { c.Daemon = watch })
	if got := get(t, rc); got != "plain-squatter" { // the daemon before TLS was turned on
		t.Fatalf("first stream: %q", got)
	}
	fc.set(t, func(c *config.Config) {
		c.APIListen, c.TLS = secureAddr, config.TLSConfig{CertFile: certFile, KeyFile: certFile + ".key"}
	})
	secure.Close() // the daemon restarts: nothing answers on the TLS address
	if got := get(t, rc); got != "" {
		t.Fatalf("the stream went in clear to %q", got)
	}
}

// An unusable certificate serves nothing (daemon.go's errDaemonCert): never another address, never plaintext.
func TestAnUnusableCertificateServesNoStream(t *testing.T) {
	old := namedAPI(t, "old-plaintext")
	fc := newFakeConfig(t, old.Listener.Addr().String())
	watch := &DaemonWatch{Load: fc.load, ConfigPath: fc.path, Log: &testLog{}}
	r := newRelay(t)
	_, kr := team(t, 1)
	b, rc := pairedBox(t, r, "bfl-read", kr, "", func(c *Config) { c.Daemon = watch })
	if got := get(t, rc); got != "old-plaintext" {
		t.Fatalf("first stream: %q", got)
	}
	missing := filepath.Join(t.TempDir(), "no-cert.pem")
	// TLS on a TLS daemon of the same kind (the old address stays plaintext), and the certificate file is unreadable
	secure := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(secure.Close)
	fc.set(t, func(c *config.Config) {
		c.APIListen, c.TLS = secure.Listener.Addr().String(), config.TLSConfig{CertFile: missing, KeyFile: missing + ".key"}
	})
	if got := get(t, rc); got != "" {
		t.Fatalf("an unusable certificate still served %q", got)
	}
	if !b.log.has("TLS certificate") {
		t.Fatal("not logged")
	}
	// the same with a TLS previous address: still nothing (no fallback at all on a bad pin)
	fc2 := newFakeConfig(t, secure.Listener.Addr().String())
	good := tlsCertFile(t, secure)
	fc2.set(t, func(c *config.Config) { c.TLS = config.TLSConfig{CertFile: good, KeyFile: good + ".key"} })
	w2 := &DaemonWatch{Load: fc2.load, ConfigPath: fc2.path, Log: &testLog{}}
	if ts := w2.Targets(); len(ts) != 1 {
		t.Fatal(ts)
	}
	other := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(other.Close)
	fc2.set(t, func(c *config.Config) { c.APIListen, c.TLS.CertFile = other.Listener.Addr().String(), missing })
	l := New(Config{Daemon: w2, Log: &testLog{}})
	if conn := l.dialTargets(); conn != nil {
		_ = conn.Close()
		t.Fatal("the previous address was tried after the certificate failed")
	}
}

// The previous address is a bridge for a config.yaml saved before the restart, no more: dropped once the new address
// has answered (review B1), and after prevTTL whatever happens.
func TestThePreviousAddressIsDroppedOnceTheNewOneAnswers(t *testing.T) {
	one, two := namedAPI(t, "one"), namedAPI(t, "two")
	fc := newFakeConfig(t, one.Listener.Addr().String())
	watch := &DaemonWatch{Load: fc.load, ConfigPath: fc.path, Log: &testLog{}}
	r := newRelay(t)
	_, kr := team(t, 1)
	_, rc := pairedBox(t, r, "bfl-read", kr, "", func(c *Config) { c.Daemon = watch })
	if got := get(t, rc); got != "one" {
		t.Fatalf("first stream: %q", got)
	}
	fc.set(t, func(c *config.Config) { c.APIListen = two.Listener.Addr().String() })
	if got := get(t, rc); got != "two" {
		t.Fatalf("after the move: %q", got)
	}
	two.Close() // the daemon restarts; a squatter (here: the old listener) holds the old port
	if got := get(t, rc); got != "" {
		t.Fatalf("the old address was tried after the new one had answered: %q", got)
	}
	if s := watch.Status(); s.Previous != "" {
		t.Fatalf("status %+v", s)
	}
}

func TestThePreviousAddressExpires(t *testing.T) {
	one := namedAPI(t, "one")
	now := time.Now()
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	fc := newFakeConfig(t, one.Listener.Addr().String())
	watch := &DaemonWatch{Load: fc.load, ConfigPath: fc.path, Log: &testLog{}, Now: clock}
	if ts := watch.Targets(); len(ts) != 1 {
		t.Fatal(ts)
	}
	idle := httptest.NewServer(http.NotFoundHandler())
	idleAddr := idle.Listener.Addr().String()
	idle.Close()
	fc.set(t, func(c *config.Config) { c.APIListen = idleAddr })
	if ts := watch.Targets(); len(ts) != 2 || ts[1].Addr != one.Listener.Addr().String() {
		t.Fatalf("right after the change: %+v", ts)
	}
	mu.Lock()
	now = now.Add(prevTTL)
	mu.Unlock()
	if ts := watch.Targets(); len(ts) != 1 || ts[0].Addr != idleAddr {
		t.Fatalf("after %s: %+v", prevTTL, ts)
	}
}

// Review m8: the certificate is read again every 30 s from wherever config.yaml points; one that users can change (or
// whose folder they can swap) is not trusted, and no stream is served.
func TestACertificateUsersCanChangeIsNotTrusted(t *testing.T) {
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "tls") }))
	t.Cleanup(secure.Close)
	certFile := tlsCertFile(t, secure)
	fc := newFakeConfig(t, secure.Listener.Addr().String())
	fc.set(t, func(c *config.Config) { c.TLS = config.TLSConfig{CertFile: certFile, KeyFile: certFile + ".key"} })
	var writable atomicBool
	log := &testLog{}
	watch := &DaemonWatch{Load: fc.load, ConfigPath: fc.path, Log: log, CertSafe: func(p string) error {
		if writable.get() && p == certFile {
			return errors.New(p + ` can be changed by BUILTIN\Users`)
		}
		return nil
	}}
	r := newRelay(t)
	_, kr := team(t, 1)
	_, rc := pairedBox(t, r, "bfl-read", kr, "", func(c *Config) { c.Daemon = watch })
	if got := get(t, rc); got != "tls" {
		t.Fatalf("a safe certificate: %q", got)
	}
	writable.set(true)
	fc.set(t, func(*config.Config) {}) // the next read (at the latest daemonRecheck later)
	if got := get(t, rc); got != "" {
		t.Fatalf("a certificate users can change still served %q", got)
	}
	if s := watch.Status(); s.CertReadable || !strings.Contains(s.Error, "not trusted") {
		t.Fatalf("status %+v", s)
	}
	if !log.has(`BUILTIN\Users`) {
		t.Fatal("not logged")
	}
	writable.set(false)
	fc.set(t, func(*config.Config) {})
	if got := get(t, rc); got != "tls" {
		t.Fatalf("made safe again: %q", got)
	}
}

type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (b *atomicBool) get() bool  { b.mu.Lock(); defer b.mu.Unlock(); return b.v }
func (b *atomicBool) set(v bool) { b.mu.Lock(); b.v = v; b.mu.Unlock() }

// On Windows the data path pipes only to a socket the connector's service owns (DaemonOwnerCheck): a stream whose
// far end is another process is closed unanswered, whichever address it is.
func TestAStreamGoesOnlyToTheDaemonsOwnProcess(t *testing.T) {
	squatter := namedAPI(t, "squatter")
	fc := newFakeConfig(t, squatter.Listener.Addr().String())
	var asked []string
	var mu sync.Mutex
	watch := &DaemonWatch{Load: fc.load, ConfigPath: fc.path, Log: &testLog{}, Owner: func(c net.Conn) error {
		mu.Lock()
		asked = append(asked, c.RemoteAddr().String())
		mu.Unlock()
		return fmt.Errorf("%w: held by process 4242", errDaemonOwner)
	}}
	r := newRelay(t)
	_, kr := team(t, 1)
	b, rc := pairedBox(t, r, "bfl-read", kr, "", func(c *Config) { c.Daemon = watch })
	if got := get(t, rc); got != "" {
		t.Fatalf("piped to %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 || asked[0] != squatter.Listener.Addr().String() {
		t.Fatalf("asked %v", asked)
	}
	if !b.log.has("process 4242") {
		t.Fatal("not logged")
	}
}

// A fresh install has no config.yaml: the link starts anyway (commands work), and data streams wait for one.
func TestTheLinkStartsWithoutConfigYAML(t *testing.T) {
	m := testMachine(t)
	m.LoadDaemon = func() (config.Config, error) { return config.Config{}, config.ErrNotFound }
	log := &testLog{}
	m.Log = log
	cfg, err := m.Config()
	if err != nil {
		t.Fatalf("no config.yaml stopped the link: %v", err)
	}
	if ts := cfg.Daemon.Targets(); len(ts) != 0 {
		t.Fatalf("targets %+v", ts)
	}
	if s := cfg.Daemon.Status(); !strings.Contains(s.Error, "config not found") || s.Address != "" {
		t.Fatalf("status %+v", s)
	}
	if !log.has("data streams wait") {
		t.Fatal("not logged")
	}
}

// An unreadable config.yaml keeps the last address known: the daemon still runs on it.
func TestAnUnreadableConfigKeepsTheLastAddress(t *testing.T) {
	fc := newFakeConfig(t, "127.0.0.1:8082")
	w := &DaemonWatch{Load: fc.load, ConfigPath: fc.path, Log: &testLog{}}
	if ts := w.Targets(); len(ts) != 1 || ts[0].Addr != "127.0.0.1:8082" {
		t.Fatal(ts)
	}
	fc.set(t, func(*config.Config) { fc.err = errors.New("yaml: line 3: did not find expected key") })
	if ts := w.Targets(); len(ts) != 1 || ts[0].Addr != "127.0.0.1:8082" {
		t.Fatalf("after a bad config.yaml: %+v", ts)
	}
	if s := w.Status(); !strings.Contains(s.Error, "line 3") {
		t.Fatalf("status %+v", s)
	}
}

func TestLinkStatusIsAnsweredByTheLink(t *testing.T) {
	fc := newFakeConfig(t, "0.0.0.0:8083")
	watch := &DaemonWatch{Load: fc.load, ConfigPath: fc.path}
	r := newRelay(t)
	priv, kr := team(t, 1)
	proxy := &ProxyFinder{Explicit: "http://svc:proxy-pass@proxy.b4l.local:3128"}
	b, rc := pairedBox(t, r, "bfl-read", kr, "", func(c *Config) { c.Daemon, c.Proxy = watch, proxy })
	b.link.status.failed(errors.New(`proxyconnect tcp: dial http://svc:proxy-pass@proxy.b4l.local:3128: refused`), time.Now())
	raw, _ := envelope(priv, "team-1", "bfl-read", CommandLinkStatus, nil, nil, time.Now())
	stdout, sa := send(t, rc, raw, nil)
	if a := answerOf(sa); a.Refusal != "" || a.Exit != 0 || b.runner.count() != 0 {
		t.Fatalf("answer %+v, %d provision runs", a, b.runner.count())
	}
	if strings.Contains(string(stdout), "proxy-pass") {
		t.Fatalf("a credential left the box: %s", stdout)
	}
	var s LinkStatus
	if err := json.Unmarshal(stdout, &s); err != nil {
		t.Fatal(err)
	}
	if s.Box != "bfl-read" || s.Build != "test" || s.Fingerprint != b.keys.Public().Fingerprint() || !s.Admin || s.Relay != r.url() {
		t.Errorf("identity %+v", s)
	}
	if !s.Connection.Connected || s.Connection.Epoch != rc.epoch || s.Connection.Relay != "relay-1" || s.Connection.Since.IsZero() {
		t.Errorf("connection %+v (epoch %d)", s.Connection, rc.epoch)
	}
	if s.Proxy.Via != "proxy.b4l.local:3128" || s.Proxy.Source != "link.yaml" {
		t.Errorf("proxy %+v", s.Proxy)
	}
	if s.KeyEpoch.Floor != 1 || len(s.KeyEpoch.Keys) != 1 || s.KeyEpoch.Keys[0].ID != "team-1" {
		t.Errorf("epoch %+v", s.KeyEpoch)
	}
	if s.Daemon.Address != "127.0.0.1:8083" || s.Daemon.TLS {
		t.Errorf("daemon %+v", s.Daemon)
	}
	if s.LastConnectError == nil || !strings.Contains(s.LastConnectError.Message, "http://proxy.b4l.local:3128") {
		t.Errorf("last error %+v", s.LastConnectError)
	}
	if s.Streams.Commands != 1 || s.StartedAt.IsZero() || s.UptimeSeconds < 0 {
		t.Errorf("streams %+v started %v", s.Streams, s.StartedAt)
	}
	// args are refused, like hashes'
	raw, _ = envelope(priv, "team-1", "bfl-read", CommandLinkStatus, []string{"x"}, nil, time.Now())
	if _, sa := send(t, rc, raw, nil); answerOf(sa).Refusal != linkproto.CodeMalformed {
		t.Fatalf("args: %+v", answerOf(sa))
	}
}
