package link

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// DaemonTarget is where a data stream goes: the daemon's API on this machine.
type DaemonTarget struct {
	Addr     string // 127.0.0.1:<apiListen port> (LocalAddr)
	TLS      bool
	CertFile string
	Cert     []byte // the daemon's certificate (PEM) when TLS: the only one trusted (APITLSConfig)
}

func (t DaemonTarget) same(o DaemonTarget) bool {
	return t.Addr == o.Addr && t.TLS == o.TLS && t.CertFile == o.CertFile && bytes.Equal(t.Cert, o.Cert)
}

// DaemonWatch reads the daemon's address, TLS and certificate from config.yaml again whenever config.yaml or the
// certificate file changes, and at least every daemonRecheck (plan 086 C5). Before it, the link read them once at
// service start, so an apply or options that moved the port or turned TLS on stranded the data path until someone
// restarted the link — and nothing remote could. It also lets the link start on a box with no config.yaml yet (a fresh
// install, provisioned over the link): data streams wait for one, commands do not need it.
type DaemonWatch struct {
	Load       func() (config.Config, error) // config.Load
	ConfigPath string                        // config.yaml, watched for changes (os.Stat)
	Log        Logger
	Now        func() time.Time
	// Owner, when set, checks the process at the far end of a connection to a target before anything is sent: on
	// Windows, that it is the connector's own service (DaemonOwnerCheck, review B1). nil: no check (off Windows, and
	// tests).
	Owner func(net.Conn) error

	mu        sync.Mutex
	loaded    bool
	cur, prev DaemonTarget // prev: the target before the last change, tried when cur does not answer
	hasPrev   bool
	changedAt time.Time // when prev was set; it is dropped prevTTL later
	cfgStamp  fileStamp
	certStamp fileStamp
	loadedAt  time.Time
	err       error
	logged    string
}

// daemonRecheck bounds how long a target is trusted without reading config.yaml again.
const daemonRecheck = 30 * time.Second

// prevTTL bounds how long the previous address is tried (review B1). It bridges a config.yaml saved before the
// daemon's restart, and nothing more: from the stop on, the old daemon no longer listens there anyway, and a stop drains
// for at most autostart.StopTimeout (150 s) — within the setup's 180 s wait. Past it, a port left behind is one a local
// user could take.
const prevTTL = 3 * time.Minute

type fileStamp struct {
	size int64
	mod  time.Time
	ok   bool
}

func stampOf(path string) fileStamp {
	if path == "" {
		return fileStamp{}
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{size: fi.Size(), mod: fi.ModTime(), ok: true}
}

func (w *DaemonWatch) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// Targets are the addresses to try for one data stream, in order: the current one, then the one before the last
// change (a config.yaml saved but not yet restarted names a port nothing listens on yet). Empty when config.yaml has
// never been read. The previous one only within prevTTL of the change and until the current one has answered once
// (Answered), and never a plaintext one behind a TLS one (review B1: a local user squatting the old port would get the
// stream in clear).
func (w *DaemonWatch) Targets() []DaemonTarget {
	w.mu.Lock()
	defer w.mu.Unlock()
	cs, ks := stampOf(w.ConfigPath), stampOf(w.cur.CertFile)
	if !w.loaded || cs != w.cfgStamp || ks != w.certStamp || w.now().Sub(w.loadedAt) >= daemonRecheck {
		w.reload(cs)
	}
	if !w.loaded {
		return nil
	}
	if w.hasPrev && w.now().Sub(w.changedAt) >= prevTTL {
		w.dropPrev(fmt.Sprintf("%s has passed since the change", prevTTL))
	}
	out := []DaemonTarget{w.cur}
	if w.hasPrev && !w.prev.same(w.cur) && (w.prev.TLS || !w.cur.TLS) {
		out = append(out, w.prev)
	}
	return out
}

// Answered tells the watch that t answered a stream: once the current address has, the previous one is never tried
// again (review B1).
func (w *DaemonWatch) Answered(t DaemonTarget) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.hasPrev && w.loaded && t.same(w.cur) {
		w.dropPrev("the connector answers at " + w.cur.Addr)
	}
}

func (w *DaemonWatch) dropPrev(why string) {
	w.hasPrev = false
	if w.Log != nil {
		w.Log.Info(fmt.Sprintf("link: %s is no longer tried: %s", w.prev.Addr, why))
	}
	w.prev = DaemonTarget{}
}

func (w *DaemonWatch) reload(cs fileStamp) {
	w.loadedAt = w.now()
	cfg, err := w.Load()
	if err != nil {
		// The daemon still runs on what it read at its start: keep the last target.
		w.err = err
		if msg := err.Error(); msg != w.logged && w.Log != nil {
			w.Log.Warn("link: the connector's config.yaml cannot be read (" + msg + "); data streams keep the last address known")
			w.logged = msg
		}
		return
	}
	t := DaemonTarget{Addr: LocalAddr(cfg.APIListen), TLS: cfg.TLS.Enabled(), CertFile: cfg.TLS.CertFile}
	if t.TLS {
		t.Cert = readCert(t.CertFile)
	}
	w.err, w.logged = nil, ""
	w.cfgStamp, w.certStamp = cs, stampOf(t.CertFile)
	if w.loaded && !t.same(w.cur) {
		// A plaintext target is never kept behind a TLS one: falling back would undo the pin.
		w.prev, w.hasPrev, w.changedAt = w.cur, w.cur.TLS || !t.TLS, w.now()
		if !w.hasPrev {
			w.prev = DaemonTarget{}
		}
		if w.Log != nil {
			w.Log.Info(fmt.Sprintf("link: the connector is now at %s (TLS %s), was %s (TLS %s)", t.Addr, onOff(t.TLS), w.cur.Addr, onOff(w.cur.TLS)))
		}
	}
	w.cur, w.loaded = t, true
}

// DaemonStatus is what link-status reports of the target.
type DaemonStatus struct {
	Address      string    `json:"address"`
	TLS          bool      `json:"tls"`
	CertReadable bool      `json:"certReadable,omitempty"`
	Previous     string    `json:"previous,omitempty"` // tried when the address does not answer (≤ prevTTL, until it has answered)
	ReadAt       time.Time `json:"readAt,omitzero"`
	Error        string    `json:"error,omitempty"`
}

// Status reads config.yaml if it changed, then reports.
func (w *DaemonWatch) Status() DaemonStatus {
	ts := w.Targets()
	w.mu.Lock()
	defer w.mu.Unlock()
	s := DaemonStatus{ReadAt: w.loadedAt.UTC()}
	if w.err != nil {
		s.Error = w.err.Error()
	}
	if len(ts) > 0 {
		s.Address, s.TLS = ts[0].Addr, ts[0].TLS
		if ts[0].TLS {
			_, err := APITLSConfig(ts[0].Cert)
			s.CertReadable = err == nil
		}
	}
	if len(ts) > 1 {
		s.Previous = ts[1].Addr
	}
	return s
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// errDaemonCert: the daemon serves TLS and its certificate file cannot be used — no data stream is served then, to no
// address (pipe tries no other one).
var errDaemonCert = errors.New("the daemon's certificate is unusable")

// errDaemonOwner: the process at the far end of the connection is not the connector's service (DaemonWatch.Owner).
var errDaemonOwner = errors.New("not the connector's own process")

// dialDaemon connects to t: TCP, the owner check when there is one (before a byte is sent), and with TLS a handshake
// that trusts only the daemon's own certificate — never "skip verification", even on loopback (CodeQL
// go/disabled-certificate-check). An unusable certificate refuses before anything is dialled.
func dialDaemon(t DaemonTarget, owner func(net.Conn) error) (net.Conn, error) {
	var conf *tls.Config
	if t.TLS {
		var err error
		if conf, err = APITLSConfig(t.Cert); err != nil {
			return nil, fmt.Errorf("%w: %w", errDaemonCert, err)
		}
	}
	conn, err := net.DialTimeout("tcp", t.Addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if owner != nil {
		if err := owner(conn); err != nil {
			_ = conn.Close()
			if !errors.Is(err, errDaemonOwner) {
				err = fmt.Errorf("%w: %w", errDaemonOwner, err)
			}
			return nil, err
		}
	}
	if !t.TLS {
		return conn, nil
	}
	tc := tls.Client(conn, conf)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tc.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return tc, nil
}
