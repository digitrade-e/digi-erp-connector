package link

import (
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// ServiceName is the link's Windows service (plan 083 D2: its own service, so
// it survives the daemon's restarts during options, upgrade and restore).
const ServiceName = "digi-erp-connector-link"

// Machine builds a Config from this machine: the data directory, the daemon's
// config.yaml and the secret store.
type Machine struct {
	DataDir    string
	InstallDir string
	Secrets    Secrets
	LoadDaemon func() (config.Config, error)
	Build      string
	Log        Logger
	HostFacts  func() (host, domain string, role int)
}

// Store is the link's state under the data directory.
func (m Machine) Store() *Store { return &Store{Dir: m.DataDir, Secrets: m.Secrets} }

// Keys returns the box's keys, making them on first use.
func (m Machine) Keys() (linkproto.BoxKeys, error) {
	k, created, err := m.Store().Keys(func() (linkproto.BoxKeys, error) { return linkproto.NewBoxKeys(rand.Reader) })
	if err == nil && created && m.Log != nil {
		m.Log.Info("link: made this box's keys; fingerprint " + k.Public().Fingerprint())
	}
	return k, err
}

// Config is the link's configuration on this machine.
func (m Machine) Config() (Config, error) {
	store := m.Store()
	st, err := store.LoadState()
	if err != nil {
		return Config{}, err
	}
	keys, err := m.Keys()
	if err != nil {
		return Config{}, fmt.Errorf("the box's keys: %w", err)
	}
	team, err := TeamKeyring()
	if err != nil {
		return Config{}, err
	}
	daemon, err := m.LoadDaemon()
	if err != nil {
		return Config{}, fmt.Errorf("the connector's config.yaml: %w", err)
	}
	hc, err := HTTPClient(st.Proxy)
	if err != nil {
		return Config{}, err
	}
	host, domain, role := "", "", 0
	if m.HostFacts != nil {
		host, domain, role = m.HostFacts()
	}
	return Config{
		Store: store, Keys: keys, Team: team, Build: m.Build, Host: host, Domain: domain, DomainRole: role,
		APIAddr: LocalAddr(daemon.APIListen), APITLS: daemon.TLS.Enabled(),
		HTTPClient: hc, Runner: ExecRunner{Path: filepath.Join(m.InstallDir, "digi-erp-connector-provision.exe")},
		InstallDir: m.InstallDir, Log: m.Log,
	}, nil
}

// Pair stores a pairing code for the service to redeem and returns the
// fingerprint the person at the server reads to us (plan 083 §8 A8).
func (m Machine) Pair(code string) (string, error) {
	norm, err := linkproto.NormalizePairCode(code)
	if err != nil {
		return "", errors.New("that is not a pairing code: ten letters and digits, like ABCDE-FGHJK")
	}
	store := m.Store()
	st, err := store.LoadState()
	if err != nil {
		return "", err
	}
	if st.Box != "" {
		return "", fmt.Errorf("this server is already paired as %s; remove link.yaml's box only on Digitrade's request", st.Box)
	}
	keys, err := m.Keys()
	if err != nil {
		return "", err
	}
	st.PairCode = norm[:5] + "-" + norm[5:]
	if err := store.SaveState(st); err != nil {
		return "", err
	}
	return keys.Public().Fingerprint(), nil
}

// LocalAddr is where the link reaches the daemon: its apiListen, with an
// unspecified host ([::], 0.0.0.0) as 127.0.0.1.
func LocalAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// HTTPClient reaches the relay: through link.yaml's proxy when set, else the
// environment's (the machine's WinHTTP setting is a later slice — plan 083
// §9.2 O5), with the system's roots, so a TLS-inspecting proxy whose CA the
// machine trusts works (plan 083 §10 run D).
func HTTPClient(proxy string) (*http.Client, error) {
	p := http.ProxyFromEnvironment
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("link.yaml proxy %q is not a URL", proxy)
		}
		p = http.ProxyURL(u)
	}
	return &http.Client{Transport: &http.Transport{
		Proxy:               p,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 15 * time.Second,
		ForceAttemptHTTP2:   false, // a WebSocket upgrade is HTTP/1.1
	}}, nil
}
