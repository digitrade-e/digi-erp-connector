package link

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// CommandLinkStatus is answered by the link itself (plan 086 C5): how this box reaches the relay and the daemon,
// what the relay's box list cannot see.
const CommandLinkStatus = "link-status"

// LinkStatus is link-status's answer. Nothing secret is in it: the proxy is named by host, and an error's URL loses
// its user and password.
type LinkStatus struct {
	Build       string `json:"build"`
	Fingerprint string `json:"fingerprint"`
	Box         string `json:"box"`
	Relay       string `json:"relay"` // link.yaml's relay URL, or the default
	Admin       bool   `json:"admin"` // link.yaml's off switch (always true here: a command ran)
	UnknownKeys bool   `json:"unknownKeys"`

	Proxy      ProxyStatus      `json:"proxy"`
	KeyEpoch   KeyEpochStatus   `json:"keyEpoch"`
	Connection ConnectionStatus `json:"connection"`
	// LastConnectError is the last time a connection to the relay failed, nil when none has since the start.
	LastConnectError *TimedError  `json:"lastConnectError"`
	Daemon           DaemonStatus `json:"daemon"`
	Streams          StreamCount  `json:"streams"`

	StartedAt     time.Time `json:"startedAt"`
	UptimeSeconds int64     `json:"uptimeSeconds"`
}

// ProxyStatus is the way to the relay: Via is the proxy's host:port, empty when direct.
type ProxyStatus struct {
	Via    string `json:"via,omitempty"`
	Source string `json:"source"` // link.yaml, HTTPS_PROXY, netsh winhttp, WPAD, direct
	Error  string `json:"error,omitempty"`
}

// KeyEpochStatus is the team-key generation this box accepts: Floor is the highest it has seen (it refuses keys
// below it), Keys the ones this build trusts.
type KeyEpochStatus struct {
	Floor    int          `json:"floor"`
	Compiled int          `json:"compiled"`
	Keys     []KeyIDEpoch `json:"keys"`
	Error    string       `json:"error,omitempty"`
}

// KeyIDEpoch names one trusted team key.
type KeyIDEpoch struct {
	ID    string `json:"id"`
	Epoch int    `json:"epoch"`
}

// ConnectionStatus is the live connection the relay routes to.
type ConnectionStatus struct {
	Connected bool      `json:"connected"`
	Epoch     int64     `json:"epoch,omitempty"`
	Relay     string    `json:"relay,omitempty"` // the relay instance
	Since     time.Time `json:"since,omitzero"`
}

// TimedError is an error and when it happened.
type TimedError struct {
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// StreamCount is what the link is serving now: commands (this one included) and data streams.
type StreamCount struct {
	Commands int `json:"commands"`
	Data     int `json:"data"`
}

func init() {
	selfCommands[CommandLinkStatus] = func(l *Link, _ linkproto.Envelope, _ []byte) ([]byte, int) {
		b, _ := json.Marshal(l.Status())
		return b, 0
	}
}

// Status is link-status's answer.
func (l *Link) Status() LinkStatus {
	now := l.cfg.Now()
	st, _ := l.cfg.Store.LoadState()
	s := LinkStatus{
		Build: l.cfg.Build, Fingerprint: l.cfg.Keys.Public().Fingerprint(), Box: st.Box, Relay: withoutCredentials(st.RelayURL()),
		Admin: st.AdminOn(), UnknownKeys: st.UnknownKeys(),
		Streams: StreamCount{Commands: len(l.cmdSlots), Data: len(l.dataSlots)},
	}
	s.Proxy = l.proxyStatus(st.RelayURL())
	s.KeyEpoch.Compiled = CompiledMinEpoch
	for _, k := range l.cfg.Team {
		s.KeyEpoch.Keys = append(s.KeyEpoch.Keys, KeyIDEpoch{ID: k.ID, Epoch: k.Epoch})
	}
	if floor, err := l.cfg.Store.MinEpochStrict(CompiledMinEpoch); err != nil {
		s.KeyEpoch.Error = err.Error()
	} else {
		s.KeyEpoch.Floor = floor
	}
	l.status.mu.Lock()
	s.Connection = ConnectionStatus{Connected: l.status.connected, Epoch: l.status.epoch, Relay: l.status.relay}
	if l.status.connected {
		s.Connection.Since = l.status.connectedAt.UTC()
	}
	if l.status.lastErr != "" {
		s.LastConnectError = &TimedError{Message: withoutCredentials(l.status.lastErr), At: l.status.lastErrAt.UTC()}
	}
	s.StartedAt = l.status.started.UTC()
	l.status.mu.Unlock()
	s.UptimeSeconds = int64(now.Sub(s.StartedAt).Seconds())
	if l.cfg.Daemon != nil {
		s.Daemon = l.cfg.Daemon.Status()
		s.Daemon.Error = withoutCredentials(s.Daemon.Error)
	} else {
		s.Daemon = DaemonStatus{Address: l.cfg.APIAddr, TLS: l.cfg.APITLS}
	}
	return s
}

func (l *Link) proxyStatus(relay string) ProxyStatus {
	if l.cfg.Proxy == nil {
		return ProxyStatus{Source: "direct"}
	}
	u, err := url.Parse(relay)
	if err != nil {
		return ProxyStatus{Error: "the relay URL does not parse"}
	}
	switch u.Scheme { // the WebSocket dial asks the HTTP client for these
	case "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	}
	c, err := l.cfg.Proxy.Find(u)
	if err != nil {
		return ProxyStatus{Error: withoutCredentials(err.Error())}
	}
	ps := ProxyStatus{Source: c.Source}
	if c.URL != nil {
		ps.Via = c.URL.Host
	}
	return ps
}

// urlCredentials is a URL's userinfo up to the last @ of its authority: an unescaped @ inside a password leaves nothing
// behind (review m12).
var urlCredentials = regexp.MustCompile(`(://)[^/\s]*@`)

// withoutCredentials drops a URL's user and password from a message (a proxy URL in an error, say).
func withoutCredentials(s string) string {
	if !strings.Contains(s, "@") {
		return s
	}
	return urlCredentials.ReplaceAllString(s, "${1}")
}

// WithoutCredentials is withoutCredentials for provision's state (link.yaml's relay URL).
func WithoutCredentials(s string) string { return withoutCredentials(s) }
