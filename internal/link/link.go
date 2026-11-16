package link

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/hashicorp/yamux"

	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// Logger is what the link writes to (the service's log; every command also
// goes to the Windows Event Log there).
type Logger interface {
	Info(msg string)
	Warn(msg string)
	Error(msg string, err error)
}

// Config wires one link.
type Config struct {
	Store      *Store
	Keys       linkproto.BoxKeys
	Team       linkproto.Keyring
	Build      string
	Host       string
	Domain     string
	DomainRole int

	// APIAddr is the daemon's local address (127.0.0.1:<apiListen port>);
	// APITLS when the daemon serves TLS there.
	APIAddr string
	APITLS  bool
	APICert []byte // the daemon's certificate (PEM, its certFile) when APITLS: the only one the link trusts

	// Daemon, when set, is asked for the daemon's address on every data stream (plan 086 C5); nil = APIAddr, APITLS
	// and APICert as given.
	Daemon *DaemonWatch
	// Proxy is the finder behind HTTPClient, for link-status (nil: link-status names none).
	Proxy *ProxyFinder
	// Upgrade is what the upgrade command needs (plan 086 C8).
	Upgrade UpgradeConfig

	HTTPClient *http.Client // the proxy and the TLS roots for the relay
	Runner     Runner       // provision.exe
	InstallDir string       // for "hashes"
	Log        Logger
	Now        func() time.Time

	RotateEvery  time.Duration // default 20 h ± 2 h, under the relay LB's 24 h WebSocket cap (§8 A4)
	LiveEvery    time.Duration // default 60 s (§11 finding 6)
	LiveTimeout  time.Duration // default 15 s
	PendingRetry time.Duration // default 30 s while a pairing waits for confirmation

	// AutoEvery is the auto-update check's period (plan 088), default 10 min ± 1 min; negative: no scheduler (tests
	// call autoCheck themselves). AutoFirst is the first check's delay after start, default 1–3 min.
	AutoEvery time.Duration
	AutoFirst time.Duration
}

// Link keeps one box connected to the relay.
type Link struct {
	cfg       Config
	cmdSlots  chan struct{}
	dataSlots chan struct{}

	unverMu    sync.Mutex
	unverAt    time.Time
	unverCount int

	moveMu  sync.Mutex
	movedAt time.Time

	logMu    sync.Mutex
	loggedAt map[string]time.Time // logEvery

	status liveStatus // for link-status (plan 086 C5)

	// plan 088: the session marker, the connection reports go to, policy deliveries and the scheduler
	sessionMu   sync.Mutex
	curMu       sync.Mutex
	cur         *conn
	policySlots chan struct{}
	refusedMu   sync.Mutex
	refused     []linkproto.ReportRefusal // the last policy delivery's refusals
	autoMu      sync.Mutex
	autoKick    chan struct{}
}

// liveStatus is the link's own view of its connection, for link-status.
type liveStatus struct {
	mu          sync.Mutex
	started     time.Time
	connected   bool
	epoch       int64
	relay       string // the relay instance that answered (Welcome.Relay)
	connectedAt time.Time
	liveAt      time.Time // the relay last said the current connection is the one it routes to (plan 088)
	lastErr     string
	lastErrAt   time.Time
}

func (s *liveStatus) up(epoch int64, relay string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connected, s.epoch, s.relay, s.connectedAt, s.liveAt = true, epoch, relay, at, at
}

// down marks the connection of epoch closed; false when another one is current (a move made it so).
func (s *liveStatus) down(epoch int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.epoch == epoch {
		s.connected = false
		return true
	}
	return false
}

// live records that the relay answered that epoch's connection is current; false when it is not the live one.
func (s *liveStatus) live(epoch int64, at time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.connected || s.epoch != epoch {
		return false
	}
	s.liveAt = at
	return true
}

func (s *liveStatus) failed(err error, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastErr, s.lastErrAt = err.Error(), at
}

func New(cfg Config) *Link {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.RotateEvery == 0 {
		cfg.RotateEvery = 20 * time.Hour
	}
	if cfg.LiveEvery == 0 {
		cfg.LiveEvery = time.Minute
	}
	if cfg.LiveTimeout == 0 {
		cfg.LiveTimeout = 15 * time.Second
	}
	if cfg.PendingRetry == 0 {
		cfg.PendingRetry = 30 * time.Second
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.AutoEvery == 0 {
		cfg.AutoEvery = 10 * time.Minute
	}
	l := &Link{cfg: cfg, cmdSlots: make(chan struct{}, 2), dataSlots: make(chan struct{}, 64),
		policySlots: make(chan struct{}, 1), autoKick: make(chan struct{}, 1)}
	l.status.started = cfg.Now()
	return l
}

// daemonTargets are where a data stream may go, the current address first (DaemonWatch.Targets).
func (l *Link) daemonTargets() []DaemonTarget {
	if l.cfg.Daemon != nil {
		return l.cfg.Daemon.Targets()
	}
	return []DaemonTarget{{Addr: l.cfg.APIAddr, TLS: l.cfg.APITLS, Cert: l.cfg.APICert}}
}

// conn is one live connection to the relay.
type conn struct {
	sess    *yamux.Session
	epoch   int64
	relay   string
	ctlMu   sync.Mutex
	ctl     net.Conn
	move    chan string // a reason to move to a new connection: goaway, not_current
	liveAns chan bool
	// policies: the relay opened a policy stream on this connection (plan 088), so it takes report streams; the box
	// opens one on no other connection (an older relay closes a connection whose box opens a second stream).
	policies atomic.Bool
}

var (
	errPending   = errors.New("pairing redeemed; waiting for a human to confirm the fingerprint")
	errNotPaired = errors.New("not paired: no box id and no pairing code in link.yaml")
)

// Run keeps the box connected until ctx ends.
func (l *Link) Run(ctx context.Context) error {
	go l.runAuto(ctx) // plan 088: the auto-update scheduler
	backoff := time.Second
	quietUnpaired := false // "not paired" is logged once, not every 15 s
	for ctx.Err() == nil {
		st, err := l.cfg.Store.LoadState()
		if err != nil {
			l.cfg.Log.Error("link.yaml", err)
			sleep(ctx, 30*time.Second)
			continue
		}
		c, wait, err := l.connect(ctx, st)
		if err != nil {
			l.status.failed(err, l.cfg.Now())
			if wait == 0 {
				wait = backoff + time.Duration(rand.Int64N(int64(backoff)/2+1))
				backoff = min(backoff*2, time.Minute)
			}
			switch {
			case errors.Is(err, errPending):
			case errors.Is(err, errNotPaired):
				if !quietUnpaired {
					l.cfg.Log.Warn("link: " + err.Error() + "; waiting for `digi-erp-connector-link.exe pair <code>`")
					quietUnpaired = true
				}
			default:
				l.cfg.Log.Warn(fmt.Sprintf("link: %v; next try in %s", err, wait.Round(time.Second)))
			}
			sleep(ctx, wait)
			continue
		}
		backoff = time.Second
		l.setCurrent(c)
		l.serve(ctx, c)
	}
	return ctx.Err()
}

// setCurrent names the connection unsolicited reports go to (plan 088): the one the link serves.
func (l *Link) setCurrent(c *conn) {
	l.curMu.Lock()
	defer l.curMu.Unlock()
	l.cur = c
}

// current is the connection the link serves now, nil between connections.
func (l *Link) current() *conn {
	l.curMu.Lock()
	defer l.curMu.Unlock()
	return l.cur
}

// serve runs c until it dies, moving to a new connection first when the relay
// says so or the rotation is due (make-before-break).
func (l *Link) serve(ctx context.Context, c *conn) {
	rotate := time.NewTimer(l.rotateAfter())
	defer rotate.Stop()
	live := time.NewTicker(l.cfg.LiveEvery)
	defer live.Stop()
	for {
		var reason string
		select {
		case <-ctx.Done():
			_ = c.sess.Close()
			if l.status.down(c.epoch) {
				l.writeSession() // the service stops: the marker no longer says connected
			}
			return
		case <-c.sess.CloseChan():
			l.cfg.Log.Warn(fmt.Sprintf("link: connection to %s (epoch %d) closed", c.relay, c.epoch))
			if l.status.down(c.epoch) {
				l.writeSession()
			}
			l.setCurrent(nil)
			return
		case <-live.C:
			go l.checkLive(c)
			continue
		case reason = <-c.move:
			// A relay that asks to move on every round trip would have the box open TLS and WebSocket connections
			// without end, each old one held while a stream stays open: one move per 30 s at most (review 2026-11-15).
			l.moveMu.Lock()
			wait := 30*time.Second - l.cfg.Now().Sub(l.movedAt)
			l.moveMu.Unlock()
			if wait > 0 {
				sleep(ctx, wait)
			}
		case <-rotate.C:
			reason = "rotation"
		}
		l.moveMu.Lock()
		l.movedAt = l.cfg.Now()
		l.moveMu.Unlock()
		st, err := l.cfg.Store.LoadState()
		var next *conn
		if err == nil {
			next, _, err = l.connect(ctx, st)
		}
		if err != nil {
			l.status.failed(err, l.cfg.Now())
			l.cfg.Log.Warn(fmt.Sprintf("link: moving (%s) failed: %v; staying on epoch %d for now", reason, err, c.epoch))
			go func(c *conn, reason string) { // try again shortly
				sleep(ctx, 5*time.Second)
				select {
				case c.move <- reason:
				default:
				}
			}(c, reason)
			continue
		}
		l.cfg.Log.Info(fmt.Sprintf("link: moved (%s) from epoch %d to %d", reason, c.epoch, next.epoch))
		_ = c.sess.GoAway()
		go drain(c)
		c = next
		l.setCurrent(c)
		rotate.Reset(l.rotateAfter())
	}
}

func (l *Link) rotateAfter() time.Duration {
	spread := l.cfg.RotateEvery / 10
	if spread <= 0 {
		return l.cfg.RotateEvery
	}
	return l.cfg.RotateEvery - spread + time.Duration(rand.Int64N(int64(2*spread)))
}

// drain closes an old connection once its streams are done (at most 12 min:
// a /c/ call's deadline at the relay is 11).
func drain(c *conn) {
	deadline := time.Now().Add(12 * time.Minute)
	for c.sess.NumStreams() > 1 && time.Now().Before(deadline) { // the control stream stays open
		time.Sleep(500 * time.Millisecond)
	}
	_ = c.sess.Close()
}

func (l *Link) connect(ctx context.Context, st State) (*conn, time.Duration, error) {
	if st.Box == "" && st.PairCode == "" {
		// A code stored by `pair` (setup, or the person later) is picked up within 15 s.
		return nil, 15 * time.Second, errNotPaired
	}
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(dctx, st.RelayURL(), &websocket.DialOptions{HTTPClient: l.cfg.HTTPClient})
	if err != nil {
		return nil, 0, fmt.Errorf("dial %s: %w", st.RelayURL(), err)
	}
	var ch linkproto.Challenge
	if err := wsjson.Read(dctx, ws, &ch); err != nil {
		_ = ws.CloseNow()
		return nil, 0, fmt.Errorf("challenge: %w", err)
	}
	hello := linkproto.Hello{Box: st.Box, Build: l.cfg.Build, Host: l.cfg.Host, Domain: l.cfg.Domain, DomainRole: l.cfg.DomainRole}
	if st.Box == "" {
		hello.PairCode = st.PairCode
	}
	linkproto.SignHello(l.cfg.Keys, &hello, ch)
	if err := wsjson.Write(dctx, ws, hello); err != nil {
		_ = ws.CloseNow()
		return nil, 0, fmt.Errorf("hello: %w", err)
	}
	var w linkproto.Welcome
	if err := wsjson.Read(dctx, ws, &w); err != nil {
		_ = ws.CloseNow()
		return nil, 0, fmt.Errorf("welcome: %w", err)
	}
	if !w.OK {
		_ = ws.CloseNow()
		switch w.Code {
		case linkproto.CodePending:
			if hello.PairCode != "" && linkproto.ValidBoxID(w.Box) {
				st.Box, st.PairCode = w.Box, ""
				if err := l.cfg.Store.SaveState(st); err != nil {
					return nil, 0, fmt.Errorf("saving the box id %s: %w", w.Box, err)
				}
				l.cfg.Log.Info(fmt.Sprintf("link: pairing code redeemed: box %s, fingerprint %s; waiting for confirmation",
					w.Box, l.cfg.Keys.Public().Fingerprint()))
			}
			return nil, l.cfg.PendingRetry, errPending
		case linkproto.CodeBoxKeyChanged, linkproto.CodeBoxUnknown, linkproto.CodePairExpired, linkproto.CodeBadPairCode, "box_disabled":
			return nil, 10 * time.Minute, fmt.Errorf("the relay refused this box: %q", w.Code)
		}
		return nil, 0, fmt.Errorf("the relay refused: %q", w.Code)
	}
	ws.SetReadLimit(8 << 20) // yamux frames exceed the library's 32 KiB default (plan 083 §10 finding 2)
	nc := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	sess, err := yamux.Server(nc, YamuxConfig())
	if err != nil {
		_ = nc.Close()
		return nil, 0, err
	}
	ctl, err := sess.OpenStream()
	if err == nil {
		_, err = ctl.Write([]byte{linkproto.StreamControl})
	}
	if err != nil {
		_ = sess.Close()
		return nil, 0, fmt.Errorf("control stream: %w", err)
	}
	c := &conn{sess: sess, epoch: w.Epoch, relay: w.Relay, ctl: ctl, move: make(chan string, 4), liveAns: make(chan bool, 1)}
	go l.readControl(c)
	go l.accept(c)
	l.cfg.Log.Info(fmt.Sprintf("link: connected to %q as %q (epoch %d)", w.Relay, w.Box, w.Epoch))
	l.status.up(w.Epoch, w.Relay, l.cfg.Now())
	l.writeSession()
	return c, 0, nil
}

func (l *Link) readControl(c *conn) {
	for {
		var m linkproto.Control
		if err := linkproto.ReadJSONFrame(c.ctl, linkproto.MaxControlFrame, &m); err != nil {
			return
		}
		switch m.T {
		case linkproto.CtlGoAway:
			l.cfg.Log.Info(fmt.Sprintf("link: the relay asks to move (%q)", m.Reason))
			select {
			case c.move <- "goaway":
			default:
			}
		case linkproto.CtlLive:
			select {
			case c.liveAns <- m.Current != nil && *m.Current:
			default:
			}
		}
	}
}

// checkLive asks the relay whether c is still the connection it routes to.
func (l *Link) checkLive(c *conn) {
	c.ctlMu.Lock()
	_ = c.ctl.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err := linkproto.WriteJSONFrame(c.ctl, linkproto.Control{T: linkproto.CtlLiveQuery, Epoch: c.epoch})
	c.ctlMu.Unlock()
	if err != nil {
		_ = c.sess.Close()
		return
	}
	select {
	case current := <-c.liveAns:
		if !current {
			select {
			case c.move <- "not_current":
			default:
			}
		} else if l.status.live(c.epoch, l.cfg.Now()) {
			l.writeSession() // plan 088: the runner's health check reads when the relay last confirmed this connection
		}
	case <-time.After(l.cfg.LiveTimeout):
		l.cfg.Log.Warn(fmt.Sprintf("link: no liveness answer in %s; reconnecting", l.cfg.LiveTimeout))
		_ = c.sess.Close()
	case <-c.sess.CloseChan():
	}
}

func (l *Link) accept(c *conn) {
	for {
		st, err := c.sess.AcceptStream()
		if err != nil {
			return
		}
		go l.handleStream(c, st)
	}
}

// YamuxConfig is both ends' session configuration.
func YamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = 20 * time.Second
	cfg.ConnectionWriteTimeout = 30 * time.Second
	cfg.MaxStreamWindowSize = 256 << 10
	cfg.LogOutput = io.Discard
	return cfg
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
