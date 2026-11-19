package link

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
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
	HostFacts  func() (host, domain string, role int, roleKnown bool)
}

// Protect gives the data directory the connector's protected DACL (SYSTEM and Administrators only) before the link
// writes its keys or link.yaml there, and refuses when it cannot — never keys in a folder a local user can read or
// owns (review 2026-11-15: on a fresh install setup's pair ran before the daemon's first start had protected it).
func (m Machine) Protect() error {
	if runtime.GOOS != "windows" {
		return nil
	}
	if err := os.MkdirAll(m.DataDir, 0o700); err != nil {
		return err
	}
	if _, err := dataacl.ProtectTree(m.DataDir, dataacl.TreeOptions{}); err != nil {
		return fmt.Errorf("the data directory %s could not be protected (its owner or ACL is not the connector's): %w", m.DataDir, err)
	}
	return nil
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
	// The daemon's address is read per data stream (plan 086 C5), not here: a config.yaml written later — by an apply
	// over this very link on a fresh install, or one that moves the port — reaches the data path without a restart.
	// On Windows a stream goes only to a socket the connector's service owns (review B1), and the certificate is
	// trusted only where users cannot change it (review m8).
	watch := &DaemonWatch{Load: m.LoadDaemon, ConfigPath: filepath.Join(m.DataDir, "config.yaml"), Log: m.Log,
		Owner: DaemonOwnerCheck(), CertSafe: CertWriters}
	if _, err := m.LoadDaemon(); err != nil && m.Log != nil {
		m.Log.Warn("link: the connector's config.yaml cannot be read yet (" + err.Error() + "); commands work, data streams wait for it")
	}
	proxy := &ProxyFinder{Explicit: st.Proxy, Log: m.Log}
	hc, err := HTTPClientVia(proxy)
	if err != nil {
		return Config{}, err
	}
	host, domain, role, roleKnown := "", "", 0, false
	if m.HostFacts != nil {
		host, domain, role, roleKnown = m.HostFacts()
	}
	return Config{
		Store: store, Keys: keys, Team: team, Build: m.Build, Host: host, Domain: domain,
		DomainRole: role, DomainRoleKnown: roleKnown, APICredentialSet: m.apiCredentialSet,
		Daemon: watch, Proxy: proxy, Upgrade: UpgradeConfig{DataDir: m.DataDir, Protect: m.Protect},
		HTTPClient: hc, Runner: ExecRunner{Path: filepath.Join(m.InstallDir, "digi-erp-connector-provision.exe")},
		InstallDir: m.InstallDir, Log: m.Log,
	}, nil
}

// apiCredentialSet reads config.yaml for an API credential (plan 090 D3). No file is none; a file that cannot be read
// or parsed counts as one, so an installer is refused rather than let in on a doubt.
func (m Machine) apiCredentialSet() (bool, error) {
	if m.LoadDaemon == nil {
		return true, errors.New("link: config.yaml cannot be read here")
	}
	cfg, err := m.LoadDaemon()
	if errors.Is(err, config.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	return strings.TrimSpace(cfg.Auth.Username) != "" || cfg.Auth.Password != "", nil
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

// HTTPClient reaches the relay through proxy (link.yaml's) when set, else the
// machine's (proxy.go: HTTPS_PROXY, netsh winhttp, WPAD — plan 083 §9.2 O5),
// with the system's roots, so a TLS-inspecting proxy whose CA the machine
// trusts works (plan 083 §10 run D).
func HTTPClient(proxy string) (*http.Client, error) {
	return HTTPClientVia(&ProxyFinder{Explicit: proxy})
}

// HTTPClientVia is HTTPClient with a ProxyFinder of the caller's.
func HTTPClientVia(f *ProxyFinder) (*http.Client, error) {
	if f.Explicit != "" {
		if _, err := parseProxyURL(f.Explicit); err != nil {
			return nil, err
		}
	}
	return &http.Client{Transport: &http.Transport{
		Proxy:               f.ProxyFunc,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 15 * time.Second,
		ForceAttemptHTTP2:   false, // a WebSocket upgrade is HTTP/1.1
	}}, nil
}

// CertWriters refuses a certificate or key file that accounts besides the administrators can change, or whose folders
// they can rename away (dataacl.Writers, every folder up to the root; review m8). Off Windows nobody is named.
func CertWriters(path string) error {
	who, err := dataacl.Writers(path)
	if err != nil {
		return fmt.Errorf("who can change %s cannot be read: %w", path, err)
	}
	if len(who) > 0 {
		return fmt.Errorf("%s can be changed, or a folder above it swapped, by %s: keep the pair where only the administrators can write (the data directory)",
			path, strings.Join(who, ", "))
	}
	return nil
}

// readCert is the daemon's certificate file, nil when unset or unreadable (the data path then refuses: APITLSConfig).
func readCert(path string) []byte {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return b
}

// APITLSConfig trusts exactly the daemon's own certificate: it is the root, and the name checked is one it names.
func APITLSConfig(certPEM []byte) (*tls.Config, error) {
	blk, _ := pem.Decode(certPEM)
	if blk == nil {
		return nil, errors.New("the daemon serves TLS but its certFile could not be read as PEM")
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("the daemon's certificate: %w", err)
	}
	name := ""
	switch {
	case len(cert.IPAddresses) > 0:
		name = cert.IPAddresses[0].String()
	case len(cert.DNSNames) > 0:
		name = cert.DNSNames[0]
	default:
		return nil, errors.New("the daemon's certificate names no host")
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &tls.Config{RootCAs: pool, ServerName: name, MinVersion: tls.VersionTLS12}, nil
}
