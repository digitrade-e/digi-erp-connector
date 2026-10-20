package provision

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/autostart"
)

// System is everything provisioning touches outside the data directory's
// files and the database: the service manager, the firewall, the registry's
// SQL Server entries, the TCP table and the host. NewSystem is the Windows
// one; tests pass a fake. Files under DataDir are read and written directly.
type System interface {
	DataDir() string
	// ProtectDataDir gives the data directory the daemon's DACL (dataacl),
	// before anything is written in it and again after.
	ProtectDataDir() error

	SecretGet(key string) ([]byte, error) // wraps secrets.ErrNotFound when absent
	SecretSet(key string, value []byte) error

	Service(name string) (autostart.ServiceInfo, error)
	SetServiceDependencies(name string, deps []string) error
	EnsureServiceRecovery(name string) (changed bool, err error)
	StopService(name string) error
	StartService(name string) error

	FirewallProfiles() (FirewallProfiles, error)
	FirewallRule(name string) (*FirewallRule, error) // nil, nil when there is none
	SetFirewallRule(rule FirewallRule) error         // creates, or replaces the rule of that name
	RemoveFirewallRule(name string) error

	Listeners() ([]Listener, error)
	SQLInstances() ([]SQLInstance, error)
	Host() (HostInfo, error)
	// ImporterCandidates lists has.exe files and BATs where Hasavshevet
	// usually lives (C:\Hash*), for a human to choose from.
	ImporterCandidates() ([]string, error)
}

// Database is the SQL Server work provisioning does, as fixed operations.
type Database interface {
	Ping(ctx context.Context) error
	EnsureProcedures(ctx context.Context) ([]hasavshevet.ProcedureResult, error)
	// EnsureLeastPrivilegeLogin creates LeastPrivilegeLogin (or sets its
	// password), its user in database, and the read-only grants of
	// scripts/sql/least-privilege-read-node.sql.
	EnsureLeastPrivilegeLogin(ctx context.Context, database, password string) error
	// Databases fingerprints the databases this login can open.
	Databases(ctx context.Context) ([]DatabaseFact, error)
	Close() error
}

// FirewallProfiles is whether each Windows firewall profile is on.
type FirewallProfiles struct {
	Domain  bool `json:"domain"`
	Private bool `json:"private"`
	Public  bool `json:"public"`
}

// AllOff: no profile is on, so no rule filters anything — the client's
// perimeter is the only gate.
func (p FirewallProfiles) AllOff() bool { return !p.Domain && !p.Private && !p.Public }

// FirewallRule is an inbound TCP allow rule on one port.
type FirewallRule struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Port    int    `json:"port"`
	// RemoteAddress is canonical (a.b.c.d or a.b.c.d/nn) and sorted; empty
	// means any source.
	RemoteAddress []string `json:"remoteAddress"`
}

// Same reports whether two rules let the same traffic in.
func (r FirewallRule) Same(o FirewallRule) bool {
	return r.Name == o.Name && r.Enabled == o.Enabled && r.Port == o.Port &&
		slices.Equal(canonicalSources(r.RemoteAddress), canonicalSources(o.RemoteAddress))
}

// canonicalSources normalises addresses the way the firewall reports them:
// "10.0.0.0/255.255.255.0" and "10.0.0.0/24" are the same; a /32 is the bare
// address. Unparseable entries are kept as they are, so they never match.
func canonicalSources(in []string) []string {
	out := make([]string, 0, len(in))
	for _, a := range in {
		out = append(out, canonicalSource(a))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func canonicalSource(a string) string {
	if ip, mask, ok := cutMask(a); ok {
		if n, err := parseSource(ip + "/" + mask); err == nil {
			ones, _ := n.Mask.Size()
			if ones == 32 {
				return n.IP.String()
			}
			return n.String()
		}
	}
	if n, err := parseSource(a); err == nil {
		if ones, _ := n.Mask.Size(); ones == 32 {
			return n.IP.String()
		}
		return n.String()
	}
	return a
}

// cutMask turns "a.b.c.d/255.255.255.0" into ("a.b.c.d", "24").
func cutMask(a string) (ip, prefix string, ok bool) {
	i := strings.LastIndex(a, "/")
	if i < 0 {
		return "", "", false
	}
	m := net.ParseIP(a[i+1:])
	if m == nil || m.To4() == nil {
		return "", "", false
	}
	ones, bits := net.IPMask(m.To4()).Size()
	if bits == 0 {
		return "", "", false
	}
	return a[:i], strconv.Itoa(ones), true
}

// Listener is a listening TCP socket.
type Listener struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
	PID     uint32 `json:"pid"`
	Process string `json:"process,omitempty"`
}

// SQLInstance is one SQL Server instance from the registry.
type SQLInstance struct {
	Name        string `json:"name"` // WIZSOFT, MSSQLSERVER, SQLEXPRESS
	ID          string `json:"id"`   // MSSQL15.WIZSOFT
	Edition     string `json:"edition,omitempty"`
	Version     string `json:"version,omitempty"`
	TCPEnabled  bool   `json:"tcpEnabled"`
	StaticPort  int    `json:"staticPort,omitempty"`  // IPAll TcpPort
	DynamicPort int    `json:"dynamicPort,omitempty"` // IPAll TcpDynamicPorts
	MixedAuth   bool   `json:"mixedAuth"`
	Service     string `json:"service"` // MSSQLSERVER or MSSQL$<name>
	State       string `json:"state,omitempty"`
	// ListeningPorts and Flags are Discover's.
	ListeningPorts []int    `json:"listeningPorts,omitempty"`
	Flags          []string `json:"flags,omitempty"`
}

// HostInfo is the machine.
type HostInfo struct {
	Name         string `json:"name"`
	Domain       string `json:"domain,omitempty"`
	PartOfDomain bool   `json:"partOfDomain"`
	// DomainRole is Win32_ComputerSystem.DomainRole: 4 and 5 are domain
	// controllers, where a local administrator is an administrator of the
	// whole domain.
	DomainRole int      `json:"domainRole"`
	OS         string   `json:"os,omitempty"`
	OSVersion  string   `json:"osVersion,omitempty"`
	PowerShell string   `json:"powershell,omitempty"`
	Elevated   bool     `json:"elevated"`
	Addresses  []string `json:"addresses,omitempty"`
}

// DomainController reports DomainRole 4 or 5.
func (h HostInfo) DomainController() bool { return h.DomainRole >= 4 }

// DatabaseFact is one database and what its tables say it is.
type DatabaseFact struct {
	Name string `json:"name"`
	// ERP is hasavshevet (Accounts with Stock or Rates), sap (OITM and OCRD)
	// or "" when neither.
	ERP string `json:"erp,omitempty"`
}

// Env is what the operations run against.
type Env struct {
	Sys System
	// OpenDB opens cfg's database with password and checks it answers.
	OpenDB func(cfg config.Config, password string) (Database, error)
	// HTTP talks to the connector on this machine. TLS is not verified: the
	// address is local and the pair is the one Apply wrote.
	HTTP *http.Client
	// LocalAddr is the address this machine reaches apiListen on
	// (0.0.0.0:8082 is 127.0.0.1:8082). Tests point it at a test server.
	LocalAddr func(listen string) string
	Now       func() time.Time
	Sleep     func(time.Duration)
	// ProofTimeout bounds the port proof after a restart.
	ProofTimeout time.Duration
}

// DefaultProofTimeout is how long a restarted connector has to listen and
// answer /api/ping before Apply rolls back.
const DefaultProofTimeout = 30 * time.Second

// NewEnv is the environment of a real box around sys.
func NewEnv(sys System) Env {
	return Env{
		Sys:    sys,
		OpenDB: OpenDatabase,
		HTTP: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				// Local only (LocalAddr) and our own self-signed pair.
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12},
				Proxy:           nil,
			},
		},
		LocalAddr:    LocalAddr,
		Now:          time.Now,
		Sleep:        time.Sleep,
		ProofTimeout: DefaultProofTimeout,
	}
}

// LocalAddr turns a listen address into one this machine can dial: an
// unspecified host (0.0.0.0, ::) is reached on 127.0.0.1 — Go listens
// dual-stack on [::].
func LocalAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
