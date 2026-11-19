package link

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/hashicorp/yamux"

	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// --- test doubles ------------------------------------------------------------------------------------------------

type memSecrets struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (s *memSecrets) Get(k string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[k]
	if !ok {
		return nil, ErrNoSecret
	}
	return append([]byte(nil), v...), nil
}

func (s *memSecrets) Set(k string, v []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string][]byte{}
	}
	s.m[k] = append([]byte(nil), v...)
	return nil
}

type testLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *testLog) add(s string)  { l.mu.Lock(); l.lines = append(l.lines, s); l.mu.Unlock() }
func (l *testLog) Info(m string) { l.add("INFO " + m) }
func (l *testLog) Warn(m string) { l.add("WARN " + m) }
func (l *testLog) Error(m string, err error) {
	l.add(fmt.Sprintf("ERROR %s: %v", m, err))
}
func (l *testLog) has(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	stdin [][]byte
	exit  int // what every run exits with
}

func (r *fakeRunner) Run(_ context.Context, argv []string, stdin []byte, _ int) ([]byte, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, argv)
	r.stdin = append(r.stdin, stdin)
	return []byte(`{"command":"` + argv[0] + `","ok":true}`), r.exit, nil
}

func (r *fakeRunner) count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.calls) }

// --- a fake relay that speaks the protocol ------------------------------------------------------------------------

type relayConn struct {
	box   string
	epoch int64
	sess  *yamux.Session
	ctl   net.Conn
}

type fakeRelay struct {
	t         *testing.T
	srv       *httptest.Server
	mu        sync.Mutex
	codes     map[string]string // normalized pair code → box id
	madeBy    map[string]string // normalized pair code → the installer who made it (plan 090); none: the team
	pending   map[string]linkproto.BoxPublic
	confirmed map[string]linkproto.BoxPublic
	conns     chan *relayConn
	hellos    atomic.Int64
	current   atomic.Bool // what a liveness answer says
	answer    atomic.Bool // whether liveness is answered at all
}

func newRelay(t *testing.T) *fakeRelay {
	r := &fakeRelay{t: t, codes: map[string]string{}, madeBy: map[string]string{}, pending: map[string]linkproto.BoxPublic{},
		confirmed: map[string]linkproto.BoxPublic{}, conns: make(chan *relayConn, 16)}
	r.current.Store(true)
	r.answer.Store(true)
	r.srv = httptest.NewServer(http.HandlerFunc(r.handle))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *fakeRelay) url() string { return "ws" + strings.TrimPrefix(r.srv.URL, "http") + "/link" }

func (r *fakeRelay) handle(w http.ResponseWriter, req *http.Request) {
	c, err := websocket.Accept(w, req, nil)
	if err != nil {
		return
	}
	ctx := context.Background()
	ch, _ := linkproto.NewChallenge(rand.Reader, "relay-1")
	_ = wsjson.Write(ctx, c, ch)
	var h linkproto.Hello
	if err := wsjson.Read(ctx, c, &h); err != nil {
		return
	}
	r.hellos.Add(1)
	pub, err := linkproto.VerifyHello(h, ch)
	welcome := func(w linkproto.Welcome) { w.V, w.Relay = linkproto.Version, "relay-1"; _ = wsjson.Write(ctx, c, w) }
	if err != nil {
		welcome(linkproto.Welcome{Code: err.(*linkproto.Refusal).Code})
		_ = c.CloseNow()
		return
	}
	r.mu.Lock()
	if h.PairCode != "" {
		n, _ := linkproto.NormalizePairCode(h.PairCode)
		id, ok := r.codes[n]
		installer := r.madeBy[n]
		delete(r.codes, n)
		if ok {
			r.pending[id] = pub
		}
		r.mu.Unlock()
		if !ok {
			welcome(linkproto.Welcome{Code: linkproto.CodePairExpired})
		} else {
			welcome(linkproto.Welcome{Code: linkproto.CodePending, Box: id, Installer: installer})
		}
		_ = c.CloseNow()
		return
	}
	known, ok := r.confirmed[h.Box]
	_, isPending := r.pending[h.Box]
	r.mu.Unlock()
	switch {
	case isPending && !ok:
		welcome(linkproto.Welcome{Code: linkproto.CodePending, Box: h.Box})
		_ = c.CloseNow()
		return
	case !ok:
		welcome(linkproto.Welcome{Code: linkproto.CodeBoxUnknown})
		_ = c.CloseNow()
		return
	case !known.Equal(pub):
		welcome(linkproto.Welcome{Code: linkproto.CodeBoxKeyChanged})
		_ = c.CloseNow()
		return
	}
	epoch := time.Now().UnixNano()
	welcome(linkproto.Welcome{OK: true, Box: h.Box, Epoch: epoch})
	c.SetReadLimit(8 << 20)
	sess, err := yamux.Client(websocket.NetConn(context.Background(), c, websocket.MessageBinary), YamuxConfig())
	if err != nil {
		return
	}
	ctl, err := sess.AcceptStream()
	if err != nil {
		return
	}
	var kind [1]byte
	if _, err := io.ReadFull(ctl, kind[:]); err != nil || kind[0] != linkproto.StreamControl {
		r.t.Errorf("the box's first stream is not its control stream")
		return
	}
	rc := &relayConn{box: h.Box, epoch: epoch, sess: sess, ctl: ctl}
	go func() {
		for {
			var m linkproto.Control
			if err := linkproto.ReadJSONFrame(ctl, linkproto.MaxControlFrame, &m); err != nil {
				return
			}
			if m.T == linkproto.CtlLiveQuery && r.answer.Load() {
				cur := r.current.Load()
				_ = linkproto.WriteJSONFrame(ctl, linkproto.Control{T: linkproto.CtlLive, Epoch: m.Epoch, Current: &cur})
			}
		}
	}()
	r.conns <- rc
	<-sess.CloseChan()
}

func (r *fakeRelay) confirm(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.confirmed[id] = r.pending[id]
	delete(r.pending, id)
}

func (r *fakeRelay) next(t *testing.T) *relayConn {
	t.Helper()
	select {
	case c := <-r.conns:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("the box did not connect")
	}
	return nil
}

// --- the box under test -------------------------------------------------------------------------------------------

type box struct {
	link   *Link
	store  *Store
	keys   linkproto.BoxKeys
	runner *fakeRunner
	log    *testLog
	cancel context.CancelFunc
	done   chan struct{}
}

func startBox(t *testing.T, r *fakeRelay, st State, team linkproto.Keyring, api string, tune func(*Config)) *box {
	t.Helper()
	store := &Store{Dir: t.TempDir(), Secrets: &memSecrets{}}
	keys, _, err := store.Keys(func() (linkproto.BoxKeys, error) { return linkproto.NewBoxKeys(rand.Reader) })
	if err != nil {
		t.Fatal(err)
	}
	st.Relay = r.url()
	if err := store.SaveState(st); err != nil {
		t.Fatal(err)
	}
	b := &box{store: store, keys: keys, runner: &fakeRunner{}, log: &testLog{}, done: make(chan struct{})}
	cfg := Config{Store: store, Keys: keys, Team: team, Build: "test", Host: "SQLSRV", Domain: "b4l.local", DomainRole: 3,
		APIAddr: api, Runner: b.runner, InstallDir: t.TempDir(), Log: b.log, PendingRetry: 50 * time.Millisecond,
		RotateEvery: time.Hour, LiveEvery: time.Hour}
	if tune != nil {
		tune(&cfg)
	}
	b.link = New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel
	go func() { _ = b.link.Run(ctx); close(b.done) }()
	t.Cleanup(func() { cancel(); <-b.done })
	return b
}

// pairedBox makes a box already confirmed at the relay as id.
func pairedBox(t *testing.T, r *fakeRelay, id string, team linkproto.Keyring, api string, tune func(*Config)) (*box, *relayConn) {
	t.Helper()
	code := "ABCDE-FGHJK"
	n, _ := linkproto.NormalizePairCode(code)
	r.mu.Lock()
	r.codes[n] = id
	r.mu.Unlock()
	b := startBox(t, r, State{PairCode: code}, team, api, tune)
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, _ := b.store.LoadState()
		if st.Box == id {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the pairing code was never redeemed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.confirm(id)
	return b, r.next(t)
}

func team(t *testing.T, epoch int) (ed25519.PrivateKey, linkproto.Keyring) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	return priv, linkproto.Keyring{{ID: fmt.Sprintf("team-%d", epoch), Epoch: epoch, Pub: pub}}
}

// send runs one command over a fresh command stream and returns stdout and the verified answer.
func send(t *testing.T, rc *relayConn, signed []byte, stdin []byte) ([]byte, linkproto.SignedAnswer) {
	t.Helper()
	st, err := rc.sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	_, _ = st.Write([]byte{linkproto.StreamCommand})
	_ = linkproto.WriteFrame(st, signed)
	_ = linkproto.WriteFrame(st, stdin)
	_ = st.SetReadDeadline(time.Now().Add(5 * time.Second))
	stdout, err := linkproto.ReadFrame(st, linkproto.MaxPayload)
	if err != nil {
		t.Fatalf("no stdout frame: %v", err)
	}
	raw, err := linkproto.ReadFrame(st, linkproto.MaxControlFrame)
	if err != nil {
		t.Fatalf("no answer frame: %v", err)
	}
	var sa linkproto.SignedAnswer
	_ = json.Unmarshal(raw, &sa)
	return stdout, sa
}

func envelope(priv ed25519.PrivateKey, keyID, box, command string, args []string, stdin []byte, now time.Time) ([]byte, linkproto.Envelope) {
	e := linkproto.Envelope{Box: box, Command: command, Args: args, StdinSHA256: linkproto.SHA256Hex(stdin),
		Nonce: linkproto.RandomNonce(16), IssuedAt: now.Unix(), ExpiresAt: now.Add(4 * time.Minute).Unix(), By: "ilya"}
	s, _ := linkproto.SignEnvelope(priv, keyID, e)
	raw, _ := json.Marshal(s)
	return raw, e
}

// --- the tests ----------------------------------------------------------------------------------------------------

func TestPairingSavesTheBoxAndWaitsForConfirmation(t *testing.T) {
	r := newRelay(t)
	_, kr := team(t, 1)
	b, rc := pairedBox(t, r, "bfl-read", kr, "127.0.0.1:1", nil)
	st, _ := b.store.LoadState()
	if st.PairCode != "" {
		t.Fatal("the pairing code stayed in link.yaml after it was redeemed")
	}
	if rc.box != "bfl-read" {
		t.Fatalf("connected as %q", rc.box)
	}
	if !b.log.has(b.keys.Public().Fingerprint()) {
		t.Fatal("the fingerprint a human compares was never logged")
	}
}

func TestADataStreamReachesTheLocalAPI(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = fmt.Fprintf(w, "pong %s %s", req.URL.Path, req.Header.Get("Authorization"))
	}))
	defer api.Close()
	r := newRelay(t)
	_, kr := team(t, 1)
	_, rc := pairedBox(t, r, "bfl-read", kr, api.Listener.Addr().String(), nil)

	st, err := rc.sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st.Write([]byte{linkproto.StreamData})
	_, _ = st.Write([]byte("GET /api/ping HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer x\r\nConnection: close\r\n\r\n"))
	resp, err := http.ReadResponse(bufio.NewReader(st), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "pong /api/ping Bearer x" {
		t.Fatalf("got %q", body)
	}
}

func TestACommandRunsProvisionAndTheAnswerIsSigned(t *testing.T) {
	r := newRelay(t)
	priv, kr := team(t, 1)
	b, rc := pairedBox(t, r, "bfl-read", kr, "127.0.0.1:1", nil)
	stdin := []byte(`{"db":{"user":"sa"}}`)
	raw, e := envelope(priv, "team-1", "bfl-read", "discover", nil, stdin, time.Now())
	stdout, sa := send(t, rc, raw, stdin)
	a, err := linkproto.VerifyAnswer(b.keys.Public(), sa, "bfl-read", e.Nonce, digestOf(raw), stdout)
	if err != nil {
		t.Fatalf("the answer does not verify with the box's key: %v", err)
	}
	if a.Refusal != "" || a.Exit != 0 {
		t.Fatalf("answer %+v", a)
	}
	if b.runner.count() != 1 || strings.Join(b.runner.calls[0], " ") != "discover --spec -" || !bytes.Equal(b.runner.stdin[0], stdin) {
		t.Fatalf("provision ran %v with %q", b.runner.calls, b.runner.stdin)
	}
	if !b.log.has(`command "discover"`) {
		t.Fatal("the command was not logged")
	}
}

func TestCommandRefusals(t *testing.T) {
	r := newRelay(t)
	priv, kr := team(t, 1)
	other, _ := team(t, 1)
	b, rc := pairedBox(t, r, "bfl-read", kr, "127.0.0.1:1", nil)
	now := time.Now()
	stdin := []byte(`{}`)
	replayRaw, _ := envelope(priv, "team-1", "bfl-read", "verify", nil, stdin, now)
	send(t, rc, replayRaw, stdin) // spends the nonce
	before := b.runner.count()

	cases := []struct {
		name   string
		raw    func() []byte
		stdin  []byte
		wanted string
	}{
		{"a key this release does not trust", func() []byte { x, _ := envelope(other, "team-9", "bfl-read", "verify", nil, stdin, now); return x }, stdin, linkproto.CodeKeyUnknown},
		{"forged with another key under our key's id", func() []byte { x, _ := envelope(other, "team-1", "bfl-read", "verify", nil, stdin, now); return x }, stdin, linkproto.CodeBadSignature},
		{"another box's envelope", func() []byte { x, _ := envelope(priv, "team-1", "bfl-write", "verify", nil, stdin, now); return x }, stdin, linkproto.CodeWrongBox},
		{"expired", func() []byte {
			x, _ := envelope(priv, "team-1", "bfl-read", "verify", nil, stdin, now.Add(-10*time.Minute))
			return x
		}, stdin, linkproto.CodeExpired},
		{"a replay", func() []byte { return replayRaw }, stdin, linkproto.CodeReplay},
		{"a command outside the fixed set", func() []byte { x, _ := envelope(priv, "team-1", "bfl-read", "powershell", nil, stdin, now); return x }, stdin, linkproto.CodeCommandUnknown},
		{"arguments a fixed command takes none of", func() []byte {
			x, _ := envelope(priv, "team-1", "bfl-read", "discover", []string{"--spec", "C:\\x"}, stdin, now)
			return x
		}, stdin, linkproto.CodeMalformed},
		{"restore with a change-set that is not one", func() []byte {
			x, _ := envelope(priv, "team-1", "bfl-read", "restore", []string{"x & calc"}, stdin, now)
			return x
		}, stdin, linkproto.CodeMalformed},
		{"a changed stdin", func() []byte { x, _ := envelope(priv, "team-1", "bfl-read", "verify", nil, stdin, now); return x }, []byte(`{"other":1}`), linkproto.CodeBodyMismatch},
		{"not JSON", func() []byte { return []byte("nope") }, stdin, linkproto.CodeMalformed},
	}
	// before the signature verified, an answer names no nonce and no envelope: it says nothing about a command
	unbound := map[string]bool{linkproto.CodeKeyUnknown: true, linkproto.CodeBadSignature: true, linkproto.CodeWrongBox: true,
		linkproto.CodeExpired: true, linkproto.CodeMalformed + " (not JSON)": true}
	for _, c := range cases {
		raw := c.raw()
		stdout, sa := send(t, rc, raw, c.stdin)
		var a linkproto.Answer
		_ = json.Unmarshal(sa.Body, &a)
		if a.Refusal != c.wanted {
			t.Errorf("%s: refusal %q, want %q", c.name, a.Refusal, c.wanted)
		}
		key := c.wanted
		if c.name == "not JSON" {
			key += " (not JSON)"
		}
		if bound := a.Nonce != "" || a.Env != ""; bound == unbound[key] {
			t.Errorf("%s: nonce %q env %q — bound %v, want %v", c.name, a.Nonce, a.Env, bound, !unbound[key])
		}
		if !unbound[key] && a.Env != digestOf(raw) {
			t.Errorf("%s: the answer is bound to another envelope", c.name)
		}
		if len(stdout) != 0 {
			t.Errorf("%s: stdout %q from a refused command", c.name, stdout)
		}
	}
	if n := b.runner.count(); n != before {
		t.Fatalf("provision ran %d more times for refused envelopes", n-before)
	}
}

func TestTheClientsOffSwitch(t *testing.T) {
	r := newRelay(t)
	priv, kr := team(t, 1)
	b, rc := pairedBox(t, r, "bfl-read", kr, "127.0.0.1:1", nil)
	st, _ := b.store.LoadState()
	off := false
	st.Admin = &off
	_ = b.store.SaveState(st)
	stdin := []byte(`{}`)
	raw, _ := envelope(priv, "team-1", "bfl-read", "verify", nil, stdin, time.Now())
	_, sa := send(t, rc, raw, stdin)
	var a linkproto.Answer
	_ = json.Unmarshal(sa.Body, &a)
	if a.Refusal != "admin_off" || b.runner.count() != 0 {
		t.Fatalf("admin: false did not stop a command: %+v, %d runs", a, b.runner.count())
	}
}

func TestARetiredKeyStaysRetired(t *testing.T) {
	r := newRelay(t)
	priv1, kr1 := team(t, 1)
	priv2, kr2 := team(t, 2)
	kr := append(kr1, kr2...)
	b, rc := pairedBox(t, r, "bfl-read", kr, "127.0.0.1:1", nil)
	stdin := []byte(`{}`)
	raw, _ := envelope(priv2, "team-2", "bfl-read", "verify", nil, stdin, time.Now())
	if _, sa := send(t, rc, raw, stdin); answerOf(sa).Refusal != "" {
		t.Fatalf("epoch 2: %+v", answerOf(sa))
	}
	raw, _ = envelope(priv1, "team-1", "bfl-read", "verify", nil, stdin, time.Now())
	if _, sa := send(t, rc, raw, stdin); answerOf(sa).Refusal != linkproto.CodeKeyRevoked {
		t.Fatalf("an epoch-1 key after epoch 2 was seen: %+v", answerOf(sa))
	}
	if b.store.MinEpoch(CompiledMinEpoch) != 2 {
		t.Fatal("the highest epoch seen was not recorded")
	}
}

func TestASealedSecretIsOpenedOnTheBoxOnly(t *testing.T) {
	r := newRelay(t)
	priv, kr := team(t, 1)
	b, rc := pairedBox(t, r, "bfl-read", kr, "127.0.0.1:1", nil)
	plain := []byte(`{"db":{"user":"sa","password":"correct horse"}}`)
	sealed, err := linkproto.Seal(b.keys.Public().Seal, plain)
	if err != nil {
		t.Fatal(err)
	}
	e := linkproto.Envelope{Box: "bfl-read", Command: "apply", StdinSHA256: linkproto.SHA256Hex(sealed), Sealed: true,
		Nonce: linkproto.RandomNonce(16), IssuedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(time.Minute).Unix()}
	s, _ := linkproto.SignEnvelope(priv, "team-1", e)
	raw, _ := json.Marshal(s)
	_, sa := send(t, rc, raw, sealed)
	if a := answerOf(sa); a.Refusal != "" {
		t.Fatalf("refused: %+v", a)
	}
	if !bytes.Equal(b.runner.stdin[0], plain) {
		t.Fatal("provision did not get the opened secret")
	}
	if b.log.has("correct horse") {
		t.Fatal("the secret reached the log")
	}
}

func TestHashesAreAnsweredByTheLink(t *testing.T) {
	r := newRelay(t)
	priv, kr := team(t, 1)
	install := t.TempDir()
	_ = os.WriteFile(filepath.Join(install, "digi-erp-connectord.exe"), []byte("daemon"), 0o600)
	b, rc := pairedBox(t, r, "bfl-read", kr, "127.0.0.1:1", func(c *Config) { c.InstallDir = install })
	raw, _ := envelope(priv, "team-1", "bfl-read", CommandHashes, nil, nil, time.Now())
	stdout, sa := send(t, rc, raw, nil)
	if answerOf(sa).Refusal != "" || b.runner.count() != 0 {
		t.Fatalf("hashes: %+v, %d provision runs", answerOf(sa), b.runner.count())
	}
	if !strings.Contains(string(stdout), linkproto.SHA256Hex([]byte("daemon"))) {
		t.Fatalf("hashes: %s", stdout)
	}
}

func TestGoAwayMovesToANewConnectionFirst(t *testing.T) {
	r := newRelay(t)
	_, kr := team(t, 1)
	_, old := pairedBox(t, r, "bfl-read", kr, "127.0.0.1:1", nil)
	_ = linkproto.WriteJSONFrame(old.ctl, linkproto.Control{T: linkproto.CtlGoAway, Reason: "test"})
	next := r.next(t)
	if next.epoch == old.epoch {
		t.Fatal("no new connection")
	}
	select {
	case <-old.sess.CloseChan():
	case <-time.After(5 * time.Second):
		t.Fatal("the old connection was not let go after the move")
	}
}

func TestNotCurrentAndSilenceBothReconnect(t *testing.T) {
	for _, c := range []struct {
		name    string
		current bool
		answer  bool
	}{{"the relay says not current", false, true}, {"the relay says nothing", true, false}} {
		t.Run(c.name, func(t *testing.T) {
			r := newRelay(t)
			_, kr := team(t, 1)
			_, first := pairedBox(t, r, "bfl-read", kr, "127.0.0.1:1", func(cfg *Config) {
				cfg.LiveEvery, cfg.LiveTimeout = 100*time.Millisecond, 300*time.Millisecond
			})
			r.current.Store(c.current)
			r.answer.Store(c.answer)
			second := r.next(t)
			if second.epoch == first.epoch {
				t.Fatal("no new connection")
			}
		})
	}
}

func answerOf(sa linkproto.SignedAnswer) linkproto.Answer {
	var a linkproto.Answer
	_ = json.Unmarshal(sa.Body, &a)
	return a
}

// The fixed command set: adding one is a reviewed change here, never a relay's choice.
func TestTheFixedCommandSet(t *testing.T) {
	want := map[string]string{
		"version": "version", "discover": "discover --spec -", "apply-check": "apply --check --spec -",
		"apply": "apply --spec -", "options-check": "options --check --spec -", "options": "options --spec -",
		"verify": "verify --spec -",
		// plan 086 (B2)
		"logs": "logs --spec -", "state": "state --spec -",
	}
	if len(commands) != len(want)+1+len(provisionCommands086)+len(provisionCommands087)+len(provisionCommandsV1032) { // + restore, + plan 086 B1's (commands_086_provision_test.go), + plan 087's (commands_087_test.go), + v1.0.32's (commands_v1032_test.go)
		t.Fatalf("the command set has %d entries", len(commands))
	}
	for name, line := range want {
		argv, err := Argv(linkproto.Envelope{Command: name})
		if err != nil || strings.Join(argv, " ") != line {
			t.Errorf("%s: %v %v", name, argv, err)
		}
	}
	if argv, err := Argv(linkproto.Envelope{Command: "restore", Args: []string{"cs-bfl-0001", "--assume-idle"}}); err != nil || strings.Join(argv, " ") != "restore cs-bfl-0001 --assume-idle" {
		t.Errorf("restore: %v %v", argv, err)
	}
	// the ones the link answers itself
	var self []string
	for name := range selfCommands {
		self = append(self, name)
	}
	slices.Sort(self)
	if strings.Join(self, " ") != "hashes link-status upgrade upgrade-result" {
		t.Errorf("the link answers %v itself", self)
	}
}

// Installed but not paired: the code `pair` stores later (setup's page, or the person at the
// server) is picked up within 15 s, not after a long sleep.
func TestAnUnpairedLinkLooksAgainSoon(t *testing.T) {
	l := New(Config{Store: &Store{Dir: t.TempDir(), Secrets: &memSecrets{}}, Log: &testLog{}})
	_, wait, err := l.connect(context.Background(), State{})
	if !errors.Is(err, errNotPaired) || wait <= 0 || wait > 15*time.Second {
		t.Fatalf("an unpaired box: %v, next look in %s", err, wait)
	}
}

func digestOf(raw []byte) string {
	var s linkproto.Signed
	_ = json.Unmarshal(raw, &s)
	return linkproto.EnvelopeDigest(s)
}

// The review's attack (2026-11-15): a relay first delivers a tampered copy of E to collect a signed refusal for E's
// nonce, then the real E, then hands digi-mcp the refusal. The tampered copy's refusal now names no nonce, so it
// answers nothing; a second delivery of the real E gets replayed_nonce bound to E — "it reached the box before".
func TestARelayCannotPassOffARefusalAsTheAnswer(t *testing.T) {
	r := newRelay(t)
	priv, kr := team(t, 1)
	b, rc := pairedBox(t, r, "bfl-read", kr, "127.0.0.1:1", nil)
	stdin := []byte(`{}`)
	raw, e := envelope(priv, "team-1", "bfl-read", "options", nil, stdin, time.Now()) // apply must be sealed (review m5)
	var s linkproto.Signed
	_ = json.Unmarshal(raw, &s)
	sig := []byte(s.Sig)
	sig[3] ^= 1
	s.Sig = string(sig)
	tampered, _ := json.Marshal(s)

	_, pre := send(t, rc, tampered, stdin)
	if _, err := linkproto.VerifyAnswer(b.keys.Public(), pre, "bfl-read", e.Nonce, digestOf(raw), nil); err == nil {
		t.Fatal("a refusal of a tampered copy verifies as the answer to the real envelope")
	}
	stdout, real := send(t, rc, raw, stdin)
	a, err := linkproto.VerifyAnswer(b.keys.Public(), real, "bfl-read", e.Nonce, digestOf(raw), stdout)
	if err != nil || a.Refusal != "" || b.runner.count() != 1 {
		t.Fatalf("the real envelope: %v %+v, %d runs", err, a, b.runner.count())
	}
	_, again := send(t, rc, raw, stdin)
	a, err = linkproto.VerifyAnswer(b.keys.Public(), again, "bfl-read", e.Nonce, digestOf(raw), nil)
	if err != nil || a.Refusal != linkproto.CodeReplay || b.runner.count() != 1 {
		t.Fatalf("a second delivery: %v %+v, %d runs", err, a, b.runner.count())
	}
}

// A changed stdin spends the nonce: the real envelope cannot run after the relay collected body_mismatch for it, so
// that answer stays true.
func TestABodyMismatchSpendsTheNonce(t *testing.T) {
	r := newRelay(t)
	priv, kr := team(t, 1)
	b, rc := pairedBox(t, r, "bfl-read", kr, "127.0.0.1:1", nil)
	stdin := []byte(`{}`)
	raw, _ := envelope(priv, "team-1", "bfl-read", "apply", nil, stdin, time.Now())
	send(t, rc, raw, []byte(`{"other":1}`))
	_, sa := send(t, rc, raw, stdin)
	var a linkproto.Answer
	_ = json.Unmarshal(sa.Body, &a)
	if a.Refusal != linkproto.CodeReplay || b.runner.count() != 0 {
		t.Fatalf("the real envelope after a body mismatch: %+v, %d runs", a, b.runner.count())
	}
}

// A nonce file that will not read refuses commands; a misspelt off switch refuses commands (review 2026-11-15).
func TestStateFailsClosed(t *testing.T) {
	r := newRelay(t)
	priv, kr := team(t, 1)
	b, rc := pairedBox(t, r, "bfl-read", kr, "127.0.0.1:1", nil)
	stdin := []byte(`{}`)
	if err := os.MkdirAll(filepath.Dir(b.store.noncePath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.store.noncePath(), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.store.nonces = nil // as after a restart
	raw, _ := envelope(priv, "team-1", "bfl-read", "verify", nil, stdin, time.Now())
	_, sa := send(t, rc, raw, stdin)
	var a linkproto.Answer
	_ = json.Unmarshal(sa.Body, &a)
	if a.Refusal != "state_unreadable" || b.runner.count() != 0 {
		t.Fatalf("an unreadable nonce file: %+v, %d runs", a, b.runner.count())
	}
	_ = os.Remove(b.store.noncePath())
	b.store.nonces = nil
	st, _ := b.store.LoadState()
	y := "box: " + st.Box + "\nAdmin: false\n"
	_ = os.WriteFile(filepath.Join(b.store.Dir, "link.yaml"), []byte(y), 0o600)
	raw, _ = envelope(priv, "team-1", "bfl-read", "verify", nil, stdin, time.Now())
	_, sa = send(t, rc, raw, stdin)
	_ = json.Unmarshal(sa.Body, &a)
	if a.Refusal != "state_unreadable" || b.runner.count() != 0 {
		t.Fatalf("a misspelt off switch: %+v, %d runs", a, b.runner.count())
	}
}
