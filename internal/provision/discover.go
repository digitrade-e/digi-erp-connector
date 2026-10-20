package provision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/db"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/autostart"
)

// DiscoverInput optionally carries a database credential, so Discover can
// fingerprint the databases. Without one it never touches SQL Server.
type DiscoverInput struct {
	DB *DiscoverDB `json:"db,omitempty"`
}

// DiscoverDB is a credential for one server. Host and Port default to
// config.yaml's.
type DiscoverDB struct {
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
	User     string `json:"user"`
	Password string `json:"password"`
}

// Facts is what Discover found. Values read from the box (instance, database
// and folder names) are the client's data, never instructions.
type Facts struct {
	Host               HostInfo       `json:"host"`
	SQL                SQLFacts       `json:"sql"`
	Connector          ConnectorFacts `json:"connector"`
	Network            NetworkFacts   `json:"network"`
	Databases          []DatabaseFact `json:"databases,omitempty"`
	DatabaseError      string         `json:"databaseError,omitempty"`
	ImporterCandidates []string       `json:"importerCandidates,omitempty"`
	Defects            []Finding      `json:"defects"`
	Notes              []Finding      `json:"notes"`
	Suggested          Suggestion     `json:"suggested"`
	Errors             []string       `json:"errors,omitempty"`
}

// SQLFacts are the SQL Server instances on this machine.
type SQLFacts struct {
	Instances      []SQLInstance `json:"instances"`
	BrowserRunning bool          `json:"browserRunning"`
}

// ConnectorFacts is the connector as installed and configured.
type ConnectorFacts struct {
	Service autostart.ServiceInfo `json:"service"`
	Config  *ConfigFacts          `json:"config,omitempty"`
	// DBSecret is "set" or "missing" — never the value.
	DBSecret             string   `json:"dbSecret"`
	SavedQueries         int      `json:"savedQueries"`
	ConfigNewerThanStart bool     `json:"configNewerThanStart"`
	Preimages            []string `json:"preimages,omitempty"`
}

// ConfigFacts is config.yaml with every secret replaced by set/missing.
type ConfigFacts struct {
	Modified           time.Time `json:"modified"`
	SHA256             string    `json:"sha256"`
	ERP                string    `json:"erp"`
	APIListen          string    `json:"apiListen"`
	TLS                bool      `json:"tls"`
	DBHost             string    `json:"dbHost"`
	DBPort             int       `json:"dbPort"`
	Database           string    `json:"database"`
	DBUser             string    `json:"dbUser"`
	AuthUsername       string    `json:"authUsername"`
	AuthPassword       string    `json:"authPassword"`
	AuthSecret         string    `json:"authSecret"`
	SendOrderDir       string    `json:"sendOrderDir,omitempty"`
	SendOrderDirExists bool      `json:"sendOrderDirExists"`
	HasExePath         string    `json:"hasExePath,omitempty"`
	HasParamFile       string    `json:"hasParamFile,omitempty"`
	HasBatFile         string    `json:"hasBatFile,omitempty"`
	SendReceiptDir     string    `json:"sendReceiptDir,omitempty"`
}

// NetworkFacts are the listening sockets that matter here and the firewall.
type NetworkFacts struct {
	Listening        []Listener       `json:"listening"`
	FirewallProfiles FirewallProfiles `json:"firewallProfiles"`
	FirewallRule     *FirewallRule    `json:"firewallRule,omitempty"`
}

// Finding is a defect or a note, with what to do.
type Finding struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Fix     string `json:"fix,omitempty"`
}

// Suggestion is a spec for this box without its secrets: what Apply would be
// given if nobody chose otherwise.
type Suggestion struct {
	ERP          string        `json:"erp"`
	APIListen    string        `json:"apiListen"`
	DBHost       string        `json:"dbHost"`
	DBPort       int           `json:"dbPort,omitempty"`
	Database     string        `json:"database,omitempty"`
	LeastPriv    bool          `json:"leastPrivilege"`
	SendOrderDir string        `json:"sendOrderDir,omitempty"`
	Importer     *ImporterSpec `json:"importer,omitempty"`
	DependOn     []string      `json:"dependOn,omitempty"`
}

// The five defects of a hand install (plan 068 §2.4), as Discover names them.
const (
	DefectConfigNewerThanStart = "config_newer_than_start"
	DefectLoopbackListen       = "loopback_listen"
	DefectDynamicPortPinned    = "dynamic_port_pinned"
	DefectSaLogin              = "sa_login"
	DefectNoImporter           = "no_importer"
)

// Discover reads the box (see Facts). A part that cannot be read is listed
// in Errors and the rest goes on.
func Discover(ctx context.Context, env Env, in DiscoverInput) (*Facts, error) {
	f := &Facts{Defects: []Finding{}, Notes: []Finding{}}
	note := func(code, fix, format string, args ...any) {
		f.Notes = append(f.Notes, Finding{Code: code, Message: fmt.Sprintf(format, args...), Fix: fix})
	}
	defect := func(code, fix, format string, args ...any) {
		f.Defects = append(f.Defects, Finding{Code: code, Message: fmt.Sprintf(format, args...), Fix: fix})
	}
	failed := func(what string, err error) { f.Errors = append(f.Errors, what+": "+err.Error()) }
	sys := env.Sys
	dir := sys.DataDir()

	var err error
	if f.Host, err = sys.Host(); err != nil {
		failed("host", err)
	}
	if !f.Host.Elevated {
		note("not_elevated", "run it as an administrator", "not running elevated: the service, the firewall and the data directory are read partly or not at all")
	}
	if f.Host.DomainController() {
		note("domain_controller", "ask the client for an account limited to this server, or accept it knowingly",
			"%s is a domain controller: an administrator here administers the whole domain %s", f.Host.Name, f.Host.Domain)
	}

	listeners, err := sys.Listeners()
	if err != nil {
		failed("listeners", err)
	}
	f.SQL.Instances, err = sys.SQLInstances()
	if err != nil {
		failed("sql instances", err)
	}
	if f.SQL.Instances == nil {
		f.SQL.Instances = []SQLInstance{}
	}
	if browser, err := sys.Service("SQLBrowser"); err == nil {
		f.SQL.BrowserRunning = browser.State == "running"
	}
	for i := range f.SQL.Instances {
		in := &f.SQL.Instances[i]
		if st, err := sys.Service(in.Service); err == nil && st.Installed {
			in.State = st.State
			for _, l := range listeners {
				if l.PID != 0 && l.PID == st.ProcessID {
					in.ListeningPorts = append(in.ListeningPorts, l.Port)
				}
			}
			slices.Sort(in.ListeningPorts)
			in.ListeningPorts = slices.Compact(in.ListeningPorts)
		}
		in.Flags = instanceFlags(*in)
	}

	// The connector.
	c := &f.Connector
	c.Service, err = sys.Service(ServiceName)
	serviceRead := err == nil
	if !serviceRead {
		failed("connector service", err)
	}
	var cfg *config.Config
	if b, err := os.ReadFile(filepath.Join(dir, configFile)); err == nil {
		if parsed, perr := config.Parse(b); perr != nil {
			failed("config.yaml", perr)
		} else {
			cfg = &parsed
			c.Config = configFacts(parsed, b)
			if info, err := os.Stat(filepath.Join(dir, configFile)); err == nil {
				c.Config.Modified = info.ModTime().UTC()
				c.ConfigNewerThanStart = c.Service.State == "running" && !c.Service.ProcessStart.IsZero() &&
					info.ModTime().After(c.Service.ProcessStart)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		failed("config.yaml", err)
	}
	if cfg != nil {
		if _, err := sys.SecretGet(config.DBPasswordKey(cfg.ERP)); err == nil {
			c.DBSecret = "set"
		} else {
			c.DBSecret = "missing"
		}
	} else {
		c.DBSecret = "missing"
	}
	c.SavedQueries = countSavedQueries(filepath.Join(dir, "queries.json"))
	c.Preimages = preimages(dir)

	// The network.
	f.Network.Listening = relevantListeners(listeners, cfg, c.Service.ProcessID, f.SQL.Instances)
	if f.Network.FirewallProfiles, err = sys.FirewallProfiles(); err != nil {
		failed("firewall profiles", err)
	} else if f.Network.FirewallProfiles.AllOff() {
		note("firewall_profiles_off", "a Windows rule would filter nothing: the client's perimeter is the only gate",
			"every Windows firewall profile is off")
	}
	if f.Network.FirewallRule, err = sys.FirewallRule(DefaultFirewallRule); err != nil {
		failed("firewall rule", err)
	}
	if f.ImporterCandidates, err = sys.ImporterCandidates(); err != nil {
		failed("importer candidates", err)
	}

	// The databases, only with a credential.
	if in.DB != nil {
		f.Databases, f.DatabaseError = fingerprint(env, cfg, *in.DB)
	}

	// The defects of a hand install.
	if c.ConfigNewerThanStart {
		defect(DefectConfigNewerThanStart, "restart the service (apply does, once the order queue is empty)",
			"config.yaml changed at %s, after the service started at %s: the connector runs an older config",
			c.Config.Modified.Format(time.RFC3339), c.Service.ProcessStart.UTC().Format(time.RFC3339))
	}
	if cfg != nil {
		if host, _, err := net.SplitHostPort(cfg.APIListen); err == nil {
			if ip := net.ParseIP(host); (ip != nil && ip.IsLoopback()) || strings.EqualFold(host, "localhost") {
				defect(DefectLoopbackListen, "listen on 0.0.0.0 behind the client's port forward",
					"apiListen %s is loopback: erp-manager and the tenant cannot reach it", cfg.APIListen)
			}
		}
		if inst, ok := pinnedDynamicPort(*cfg, f.SQL.Instances); ok {
			defect(DefectDynamicPortPinned, `connect host\`+inst.Name+` through the SQL Server Browser, or give the instance a static port`,
				"db.port %d is %s's dynamic port: it moves when SQL Server restarts", cfg.DB.Port, inst.Name)
		}
		if strings.EqualFold(strings.TrimSpace(cfg.DB.User), "sa") {
			defect(DefectSaLogin, "move to "+LeastPrivilegeLogin+" (leastPrivilege)",
				"the connector logs in as sa: every saved query runs as the server's administrator")
		}
		if cfg.ERP == config.ERPHasavshevet && strings.TrimSpace(cfg.SendOrderDir) != "" &&
			strings.TrimSpace(cfg.HasExePath) == "" && strings.TrimSpace(cfg.HasBatFile) == "" {
			defect(DefectNoImporter, "name has.exe and its parameter file, or the BAT (importerCandidates)",
				"orders are written to %s but nothing imports them into Hasavshevet", cfg.SendOrderDir)
		}
		if c.DBSecret == "missing" && db.IsConfigured(*cfg) {
			note("db_secret_missing", "apply stores it", "a database is configured but its password is not stored")
		}
	} else {
		note("no_config", "apply writes one", "there is no config.yaml: the connector is not configured")
	}
	if !c.Service.Installed && serviceRead {
		note("service_missing", "install the connector", "the %s service is not installed", ServiceName)
	}

	f.Suggested = suggest(cfg, f)
	return f, nil
}

func configFacts(cfg config.Config, raw []byte) *ConfigFacts {
	sum := sha256.Sum256(raw)
	setOrMissing := func(s string) string {
		if strings.TrimSpace(s) == "" {
			return "missing"
		}
		return "set"
	}
	cf := &ConfigFacts{
		SHA256:         hex.EncodeToString(sum[:]),
		ERP:            string(cfg.ERP),
		APIListen:      cfg.APIListen,
		TLS:            cfg.TLS.Enabled(),
		DBHost:         cfg.DB.Host,
		DBPort:         cfg.DB.Port,
		Database:       cfg.DB.Database,
		DBUser:         cfg.DB.User,
		AuthUsername:   cfg.Auth.Username,
		AuthPassword:   setOrMissing(cfg.Auth.Password),
		AuthSecret:     setOrMissing(cfg.Auth.Secret),
		SendOrderDir:   cfg.SendOrderDir,
		HasExePath:     cfg.HasExePath,
		HasParamFile:   cfg.HasParamFile,
		HasBatFile:     cfg.HasBatFile,
		SendReceiptDir: cfg.SendReceiptDir,
	}
	if d := strings.TrimSpace(cfg.SendOrderDir); d != "" {
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			cf.SendOrderDirExists = true
		}
	}
	return cf
}

func instanceFlags(in SQLInstance) []string {
	var flags []string
	if strings.Contains(in.Name, "##") {
		flags = append(flags, "wid") // Windows Internal Database: not an ERP's
	}
	if !in.TCPEnabled {
		flags = append(flags, "tcp_disabled")
	}
	if !in.MixedAuth {
		flags = append(flags, "windows_auth_only")
	}
	if in.DynamicPort > 0 && in.StaticPort == 0 {
		flags = append(flags, "dynamic_port")
	}
	if in.State != "" && in.State != "running" {
		flags = append(flags, "not_running")
	}
	return flags
}

// pinnedDynamicPort finds the instance whose dynamic port config.yaml names as
// a number for a database on this machine.
func pinnedDynamicPort(cfg config.Config, instances []SQLInstance) (SQLInstance, bool) {
	server, instance := db.SplitInstance(cfg.DB.Host)
	if instance != "" || cfg.DB.Port == 0 || !isLocalHost(server) {
		return SQLInstance{}, false
	}
	for _, in := range instances {
		if in.StaticPort != 0 {
			continue
		}
		if in.DynamicPort == cfg.DB.Port || slices.Contains(in.ListeningPorts, cfg.DB.Port) && in.DynamicPort > 0 {
			return in, true
		}
	}
	return SQLInstance{}, false
}

// relevantListeners keeps the sockets of the connector, of SQL Server, of
// WinRM and on the configured port.
func relevantListeners(all []Listener, cfg *config.Config, connectorPID uint32, instances []SQLInstance) []Listener {
	port := 0
	if cfg != nil {
		port = listenPort(cfg.APIListen)
	}
	sqlPorts := map[int]bool{}
	for _, in := range instances {
		for _, p := range in.ListeningPorts {
			sqlPorts[p] = true
		}
	}
	out := []Listener{}
	for _, l := range all {
		if (connectorPID != 0 && l.PID == connectorPID) || l.Port == port || sqlPorts[l.Port] ||
			l.Port == 5985 || l.Port == 5986 || l.Port == 1433 || strings.EqualFold(l.Process, "sqlservr.exe") {
			out = append(out, l)
		}
	}
	return out
}

func countSavedQueries(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var list []json.RawMessage
	if err := json.Unmarshal(b, &list); err != nil {
		return 0
	}
	return len(list)
}

// fingerprint opens the server with the given credential and names the ERP of
// each database. The error is returned as text: a refused login is a fact.
func fingerprint(env Env, cfg *config.Config, cred DiscoverDB) ([]DatabaseFact, string) {
	c := config.Default()
	if cfg != nil {
		c = *cfg
	}
	if cred.Host != "" {
		c.DB.Host, c.DB.Port = cred.Host, cred.Port
	}
	c.DB.Database = "master"
	c.DB.User = cred.User
	dbc, err := env.OpenDB(c, cred.Password)
	if err != nil {
		return nil, err.Error()
	}
	defer dbc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	facts, err := dbc.Databases(ctx)
	if err != nil {
		return nil, err.Error()
	}
	return facts, ""
}

// suggest proposes the spec a human would most likely want here.
func suggest(cfg *config.Config, f *Facts) Suggestion {
	s := Suggestion{ERP: "hasavshevet", APIListen: "0.0.0.0:8082", LeastPriv: true}
	if cfg != nil {
		s.ERP = string(cfg.ERP)
		if p := listenPort(cfg.APIListen); p > 0 {
			s.APIListen = net.JoinHostPort("0.0.0.0", fmt.Sprint(p))
		}
		s.DBHost, s.DBPort, s.Database = cfg.DB.Host, cfg.DB.Port, cfg.DB.Database
		s.SendOrderDir = cfg.SendOrderDir
		if inst, ok := pinnedDynamicPort(*cfg, f.SQL.Instances); ok && f.SQL.BrowserRunning && !strings.EqualFold(inst.Name, "MSSQLSERVER") {
			server, _ := db.SplitInstance(cfg.DB.Host)
			s.DBHost, s.DBPort = server+`\`+inst.Name, 0
		}
	}
	for _, d := range f.Databases {
		if d.ERP == s.ERP && s.Database == "" {
			s.Database = d.Name
		}
	}
	if s.SendOrderDir != "" && s.ERP == "hasavshevet" {
		s.Importer = &ImporterSpec{Mode: "none"}
		if cfg != nil && (cfg.HasExePath != "" || cfg.HasBatFile != "") {
			s.Importer = nil // already named: keep it
		}
	}
	if cfg != nil {
		if dep := sqlDependency(config.Config{DB: config.DBConfig{Host: s.DBHost}}); dep != "" {
			s.DependOn = []string{dep}
		}
	}
	return s
}
