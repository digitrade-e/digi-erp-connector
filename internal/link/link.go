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

	HTTPClient *http.Client // the proxy and the TLS roots for the relay
	Runner     Runner       // provision.exe
	InstallDir string       // for "hashes"
	Log        Logger
	Now        func() time.Time

	RotateEvery  time.Duration // default 20 h ± 2 h, under the relay LB's 24 h WebSocket cap (§8 A4)
	LiveEvery    time.Duration // default 60 s (§11 finding 6)
	LiveTimeout  time.Duration // default 15 s
	PendingRetry time.Duration // default 30 s while a pairing waits for confirmation
}

// Link keeps one box connected to the relay.
type Link struct {
	cfg Config
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
	return &Link{cfg: cfg}
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
}

var (
	errPending   = errors.New("pairing redeemed; waiting for a human to confirm the fingerprint")
	errNotPaired = errors.New("not paired: no box id and no pairing code in link.yaml")
)

// Run keeps the box connected until ctx ends.
func (l *Link) Run(ctx context.Context) error {
	backoff := time.Second
	for ctx.Err() == nil {
		st, err := l.cfg.Store.LoadState()
		if err != nil {
			l.cfg.Log.Error("link.yaml", err)
			sleep(ctx, 30*time.Second)
			continue
		}
		c, wait, err := l.connect(ctx, st)
		if err != nil {
			if wait == 0 {
				wait = backoff + time.Duration(rand.Int64N(int64(backoff)/2+1))
				backoff = min(backoff*2, time.Minute)
			}
			if !errors.Is(err, errPending) {
				l.cfg.Log.Warn(fmt.Sprintf("link: %v; next try in %s", err, wait.Round(time.Second)))
			}
			sleep(ctx, wait)
			continue
		}
		backoff = time.Second
		l.serve(ctx, c)
	}
	return ctx.Err()
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
			return
		case <-c.sess.CloseChan():
			l.cfg.Log.Warn(fmt.Sprintf("link: connection to %s (epoch %d) closed", c.relay, c.epoch))
			return
		case <-live.C:
			go l.checkLive(c)
			continue
		case reason = <-c.move:
		case <-rotate.C:
			reason = "rotation"
		}
		st, err := l.cfg.Store.LoadState()
		var next *conn
		if err == nil {
			next, _, err = l.connect(ctx, st)
		}
		if err != nil {
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
		return nil, 5 * time.Minute, errNotPaired
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
			if hello.PairCode != "" && w.Box != "" {
				st.Box, st.PairCode = w.Box, ""
				if err := l.cfg.Store.SaveState(st); err != nil {
					return nil, 0, fmt.Errorf("saving the box id %s: %w", w.Box, err)
				}
				l.cfg.Log.Info(fmt.Sprintf("link: pairing code redeemed: box %s, fingerprint %s; waiting for confirmation",
					w.Box, l.cfg.Keys.Public().Fingerprint()))
			}
			return nil, l.cfg.PendingRetry, errPending
		case linkproto.CodeBoxKeyChanged, linkproto.CodeBoxUnknown, linkproto.CodePairExpired, linkproto.CodeBadPairCode, "box_disabled":
			return nil, 10 * time.Minute, fmt.Errorf("the relay refused this box: %s", w.Code)
		}
		return nil, 0, fmt.Errorf("the relay refused: %s", w.Code)
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
	l.cfg.Log.Info(fmt.Sprintf("link: connected to %s as %s (epoch %d)", w.Relay, w.Box, w.Epoch))
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
			l.cfg.Log.Info(fmt.Sprintf("link: the relay asks to move (%s)", m.Reason))
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
		go l.handleStream(st)
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
