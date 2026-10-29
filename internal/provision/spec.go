package provision

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/db"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
)

// ServiceName is the daemon's Windows service. The same name is
// cmd/digi-erp-connectord's windowsServiceName, the GUI's
// connectordWindowsServiceName and the installer's ServiceName.
const ServiceName = "digi-erp-connectord"

// DefaultLogin is the SQL login Apply creates when DB.LeastPrivilege is set
// and DB.Login names none — the name scripts/sql/least-privilege-read-node.sql
// uses. A server that serves two connectors needs one name per connector:
// rotating a shared login locks the other connector out.
const DefaultLogin = "erp_connector_read"

// DefaultFirewallRule is the inbound rule's display name when the spec names
// none.
const DefaultFirewallRule = "digi-erp-connector"

// MinAuthPassword is the shortest connector credential Apply accepts. digi-mcp
// generates 43 characters; this only stops a typed placeholder.
const MinAuthPassword = 16

// Spec is what Apply makes true. It arrives as JSON on stdin and carries two
// secrets, DB.Password and Auth.Password, which never leave this process.
//
// Fields left out keep what the box has: a nil TLS, Firewall or Service is
// "leave it alone", not "remove it".
type Spec struct {
	Version int `json:"version"` // 1
	// ChangeSet names the pre-image folder (provision\<ChangeSet>) that
	// Restore reads back. digi-mcp passes its change-set id.
	ChangeSet string `json:"changeSet"`
	ERP       string `json:"erp"` // hasavshevet | sap
	// APIListen must be an IP literal that is not loopback: the Hub and the
	// tenant call this node from outside.
	APIListen string   `json:"apiListen"`
	TLS       *TLSSpec `json:"tls,omitempty"`
	DB        DBSpec   `json:"db"`
	// SendOrderDir: nil keeps the box's; "" turns orders off. Moving it moves
	// lastOrderNumber.json too (Apply carries it), or order numbers would
	// start again at 1.
	SendOrderDir *string `json:"sendOrderDir,omitempty"`
	// Importer: nil keeps the box's. Orders with no importer must say
	// {"mode":"none"}: Apply refuses to leave it unstated.
	Importer *ImporterSpec `json:"importer,omitempty"`
	Auth     AuthSpec      `json:"auth"`
	Firewall *FirewallSpec `json:"firewall,omitempty"`
	Service  *ServiceSpec  `json:"service,omitempty"`
	// AssumeIdle lets Apply restart a running service whose order queue it
	// cannot read (a daemon older than GET /api/sendOrder, or one still on a
	// config whose credential is gone). digi-mcp sets it only when nothing on
	// the Hub points at this node yet.
	AssumeIdle bool    `json:"assumeIdle,omitempty"`
	Expect     *Expect `json:"expect,omitempty"`
	// Options are the GUI's remaining settings. nil, or a nil field inside,
	// keeps the box's value.
	Options *OptionsSpec `json:"options,omitempty"`
}

// OptionsSpec is every config.yaml setting the GUI edits that the fields
// above do not cover. Each field is a pointer: nil keeps the box's value.
type OptionsSpec struct {
	// ImageFolders are the folders /api/folders/list and /api/file serve:
	// absolute paths of folders on this machine. An empty list serves none.
	ImageFolders *[]string `json:"imageFolders,omitempty"`
	// ERPUser is the Hasavshevet user the GUI's "Test user" checks; "" clears it.
	ERPUser *string `json:"erpUser,omitempty"`
	Debug   *bool   `json:"debug,omitempty"`
	// QueryTimeoutSeconds and QueryMaxRows bound every saved query's run.
	QueryTimeoutSeconds *int `json:"queryTimeoutSeconds,omitempty"`
	QueryMaxRows        *int `json:"queryMaxRows,omitempty"`
	// TokenTTL is the issued token's lifetime, a Go duration from 5m to 24h;
	// "" is the default (30m).
	TokenTTL *string `json:"tokenTTL,omitempty"`
}

// Bounds of OptionsSpec.
const (
	MaxImageFolders     = 16
	MaxERPUser          = 64
	MaxQueryTimeoutSecs = 600
	MaxQueryRows        = 100000
	MinTokenTTL         = 5 * time.Minute
	MaxTokenTTL         = 24 * time.Hour
)

// DBSpec is the database and the credential the client gave (plan 068 D2).
type DBSpec struct {
	// Host is a host or host\instance. With an instance and no Port the
	// connector finds the instance through the SQL Server Browser.
	Host     string `json:"host"`
	Port     int    `json:"port,omitempty"`
	Database string `json:"database"`
	User     string `json:"user"`
	Password string `json:"password"`
	// Encrypt and TrustServerCertificate: nil keeps the box's (BFL needs
	// both on against its self-signed SQL certificate).
	Encrypt                *bool `json:"encrypt,omitempty"`
	TrustServerCertificate *bool `json:"trustServerCertificate,omitempty"`
	// AllowSa keeps sa as the connector's own login. Without it (and without
	// LeastPrivilege) sa is refused.
	AllowSa bool `json:"allowSa,omitempty"`
	// LeastPrivilege: User/Password are used once, to create the login Login
	// names, with a password born here; the connector then logs in as that
	// login and User's password is not stored.
	LeastPrivilege bool `json:"leastPrivilege,omitempty"`
	// Login is the least-privilege login's name, erp_connector_<name>; empty
	// is DefaultLogin.
	Login string `json:"login,omitempty"`
	// RotateLogin lets Apply give the login a new password when it exists and
	// this box knows no password that logs in as it: another connector or a
	// person may use it, or its password was changed. Without it Apply only
	// creates the login, or keeps one that works.
	RotateLogin bool `json:"rotateLogin,omitempty"`
}

// login is the least-privilege login's name.
func (d DBSpec) login() string {
	if d.Login == "" {
		return DefaultLogin
	}
	return d.Login
}

// ImporterSpec is how Hasavshevet imports the order files: has.exe with a
// parameter file, a Masofon BAT, or nothing (files written, imported by hand).
type ImporterSpec struct {
	Mode         string `json:"mode"` // none | hasExe | bat
	HasExePath   string `json:"hasExePath,omitempty"`
	HasParamFile string `json:"hasParamFile,omitempty"`
	HasBatFile   string `json:"hasBatFile,omitempty"`
	// AllowWritable accepts an importer that accounts besides the
	// administrators can change (System.Writers): the connector runs it as
	// LocalSystem, so it is their code run as SYSTEM. Set it only once a
	// human has accepted that.
	AllowWritable bool `json:"allowWritable,omitempty"`
}

// AuthSpec is the connector's one credential — what erp-manager and the
// tenant log in with. digi-mcp generates the password.
type AuthSpec struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// FirewallSpec is the inbound port rule for apiListen's port.
type FirewallSpec struct {
	RuleName  string   `json:"ruleName,omitempty"`
	AllowFrom []string `json:"allowFrom"` // IPv4 addresses or CIDRs
}

// ServiceSpec is what the daemon's service depends on, e.g. MSSQL$WIZSOFT:
// the database then starts first after a reboot.
type ServiceSpec struct {
	DependOn []string `json:"dependOn"`
}

// TLSSpec serves the API over HTTPS with a pair Apply generates (self-signed:
// both PHP callers skip peer verification, but erp-manager checks the host
// name, so every name or IP the callers use goes into Hosts), or stops serving
// it (off).
type TLSSpec struct {
	Mode  string   `json:"mode"` // self-signed | off
	Hosts []string `json:"hosts,omitempty"`
}

// Expect guards against a box that changed after digi-mcp read it.
type Expect struct {
	// ConfigSHA256 is the sha256 of config.yaml as the plan saw it ("" for no
	// file). Apply refuses when the file is different now.
	ConfigSHA256 *string `json:"configSha256,omitempty"`
}

// DecodeSpec reads one JSON Spec. Unknown fields and trailing data are
// refused: a misspelt key must not be silently ignored.
func DecodeSpec(r io.Reader) (Spec, *Refusal) {
	var s Spec
	if err := decodeStrict(r, &s); err != nil {
		return Spec{}, refuse(CodeSpecInvalid, "send one JSON object with only the documented fields",
			"the spec does not decode: %v", err)
	}
	return s, nil
}

func decodeStrict(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the JSON object")
	}
	return nil
}

var (
	changeSetRe   = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)
	instanceRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]{0,15}$`)
	serverRe      = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`)
	authUserRe    = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)
	ruleNameRe    = regexp.MustCompile(`^[A-Za-z0-9 ._()-]{1,64}$`)
	dependencyRe  = regexp.MustCompile(`^(MSSQLSERVER|MSSQL\$[A-Za-z_][A-Za-z0-9_$]{0,15})$`)
	sha256HexRe   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	dnsNameRe     = regexp.MustCompile(`^([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)*[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
	importerModes = map[string]bool{"none": true, "hasExe": true, "bat": true}

	// loginNameRe is a least-privilege login this tool creates: the prefix
	// keeps it clear of SQL Server's own principals (public, dbo, guest), and
	// the alphabet lets it stand in T-SQL between brackets.
	loginNameRe = regexp.MustCompile(`^erp_connector_[a-z0-9_]{1,50}$`)
)

// Validate checks the whole spec before anything is touched. The first
// problem found is returned.
func (s Spec) Validate() *Refusal {
	if s.Version != 1 {
		return refuse(CodeSpecInvalid, `send "version": 1`, "version %d is not supported", s.Version)
	}
	if !changeSetRe.MatchString(s.ChangeSet) {
		return refuse(CodeChangeSetID, "use 8 to 64 letters, digits, - or _", "changeSet is not a valid id")
	}
	if s.ERP != "hasavshevet" && s.ERP != "sap" {
		return refuse(CodeSpecInvalid, "use hasavshevet or sap", "erp %q is not supported", s.ERP)
	}
	if r := validateListen(s.APIListen); r != nil {
		return r
	}
	if r := s.validateDB(); r != nil {
		return r
	}
	if r := s.validateOrders(); r != nil {
		return r
	}
	if r := s.validateAuth(); r != nil {
		return r
	}
	if r := validateFirewall(s.Firewall); r != nil {
		return r
	}
	if s.Service != nil {
		for _, d := range s.Service.DependOn {
			if !dependencyRe.MatchString(d) {
				return refuse(CodeSpecInvalid, `name a SQL Server service: MSSQLSERVER or MSSQL$<instance>`,
					"service.dependOn %q is not a SQL Server service name", d)
			}
		}
	}
	if r := validateTLS(s.TLS); r != nil {
		return r
	}
	if s.Expect != nil && s.Expect.ConfigSHA256 != nil && *s.Expect.ConfigSHA256 != "" && !sha256HexRe.MatchString(*s.Expect.ConfigSHA256) {
		return refuse(CodeSpecInvalid, "send 64 lower-case hex digits, or an empty string for no file", "expect.configSha256 is not a sha256")
	}
	return validateOptions(s.Options)
}

func validateOptions(o *OptionsSpec) *Refusal {
	if o == nil {
		return nil
	}
	if o.ImageFolders != nil {
		dirs := *o.ImageFolders
		if len(dirs) > MaxImageFolders {
			return refuse(CodeSpecInvalid, fmt.Sprintf("name at most %d folders", MaxImageFolders), "options.imageFolders has %d entries", len(dirs))
		}
		seen := map[string]bool{}
		for _, d := range dirs {
			d = strings.TrimSpace(d)
			if d == "" || hasControl(d) || !filepath.IsAbs(d) {
				return refuse(CodeSpecInvalid, "use absolute folder paths", "options.imageFolders %q is not an absolute path", d)
			}
			k := strings.ToLower(filepath.Clean(d))
			if seen[k] {
				return refuse(CodeSpecInvalid, "name each folder once", "options.imageFolders names %q twice", d)
			}
			seen[k] = true
		}
	}
	if o.ERPUser != nil && (len(*o.ERPUser) > MaxERPUser || hasControl(*o.ERPUser)) {
		return refuse(CodeSpecInvalid, fmt.Sprintf("use up to %d characters of plain text", MaxERPUser), "options.erpUser is too long or not plain text")
	}
	if p := o.QueryTimeoutSeconds; p != nil && (*p < 1 || *p > MaxQueryTimeoutSecs) {
		return refuse(CodeSpecInvalid, fmt.Sprintf("use 1 to %d seconds", MaxQueryTimeoutSecs), "options.queryTimeoutSeconds %d is out of range", *p)
	}
	if p := o.QueryMaxRows; p != nil && (*p < 1 || *p > MaxQueryRows) {
		return refuse(CodeSpecInvalid, fmt.Sprintf("use 1 to %d rows", MaxQueryRows), "options.queryMaxRows %d is out of range", *p)
	}
	if p := o.TokenTTL; p != nil && strings.TrimSpace(*p) != "" {
		d, err := time.ParseDuration(strings.TrimSpace(*p))
		if err != nil || d < MinTokenTTL || d > MaxTokenTTL {
			return refuse(CodeSpecInvalid, `use a duration from 5m to 24h, e.g. "30m", or "" for the default`,
				"options.tokenTTL %q is not a duration in range", *p)
		}
	}
	return nil
}

func validateListen(addr string) *Refusal {
	host, portStr, err := net.SplitHostPort(strings.TrimSpace(addr))
	port, perr := strconv.Atoi(portStr)
	if err != nil || perr != nil || port < 1 || port > 65535 {
		return refuse(CodeSpecInvalid, "use ip:port, e.g. 0.0.0.0:8082", "apiListen %q is not ip:port", addr)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return refuse(CodeSpecInvalid, "use an IP literal (0.0.0.0 for every IPv4 interface)", "apiListen host %q is not an IP", host)
	}
	if ip.IsLoopback() {
		return refuse(CodeLoopbackListen, "listen on 0.0.0.0 (or the LAN address the perimeter forwards to)",
			"apiListen %s is loopback: the Hub and the tenant cannot reach it", addr)
	}
	return nil
}

func (s Spec) validateDB() *Refusal {
	d := s.DB
	server, instance := db.SplitInstance(d.Host)
	if !serverRe.MatchString(server) {
		return refuse(CodeSpecInvalid, `use a host name or IP, optionally host\instance`, "db.host %q is not a host", d.Host)
	}
	if instance != "" && !instanceRe.MatchString(instance) {
		return refuse(CodeSpecInvalid, "an instance name is up to 16 letters, digits, _ or $", "db.host's instance %q is not an instance name", instance)
	}
	if d.Port < 0 || d.Port > 65535 || (d.Port == 0 && instance == "") {
		return refuse(CodeSpecInvalid, `give the port, or name the instance (host\instance) to use the SQL Server Browser`,
			"db.port %d is not usable", d.Port)
	}
	if !hasavshevet.IsSafeDBName(d.Database) {
		return refuse(CodeSpecInvalid, "use the database's name: letters, digits and _ only",
			"db.database %q is not a database name this connector can use", d.Database)
	}
	if strings.TrimSpace(d.User) == "" || hasControl(d.User) || d.Password == "" || hasControl(d.Password) {
		return refuse(CodeSpecInvalid, "send the login the client gave, and its password", "db.user or db.password is missing or not text")
	}
	if strings.EqualFold(strings.TrimSpace(d.User), "sa") && !d.AllowSa && !d.LeastPrivilege {
		return refuse(CodeSaLogin, "set leastPrivilege (sa creates a least-privilege login once and is not stored), or allowSa",
			"db.user is sa: the connector would run every saved query as the server's administrator")
	}
	if d.Login != "" && !d.LeastPrivilege {
		return refuse(CodeSpecInvalid, "set leastPrivilege, or leave db.login out", "db.login names a login, but leastPrivilege is off")
	}
	if d.Login != "" && !loginNameRe.MatchString(d.Login) {
		return refuse(CodeSpecInvalid, "use erp_connector_ followed by lower-case letters, digits or _",
			"db.login %q is not a login this tool creates", d.Login)
	}
	if d.LeastPrivilege && strings.EqualFold(strings.TrimSpace(d.User), d.login()) {
		return refuse(CodeSpecInvalid, "send an administrator's login: it creates "+d.login(),
			"db.user is %s itself: it cannot create its own login", d.login())
	}
	return nil
}

func (s Spec) validateOrders() *Refusal {
	if s.SendOrderDir != nil {
		dir := strings.TrimSpace(*s.SendOrderDir)
		if dir != "" && s.ERP != "hasavshevet" {
			return refuse(CodeSpecInvalid, "leave sendOrderDir out or empty: only Hasavshevet takes order files", "sendOrderDir is set for erp %s", s.ERP)
		}
		if dir != "" && !filepath.IsAbs(dir) {
			return refuse(CodeSpecInvalid, "use an absolute path", "sendOrderDir %q is not absolute", dir)
		}
	}
	imp := s.Importer
	if imp == nil {
		return nil
	}
	if !importerModes[imp.Mode] {
		return refuse(CodeSpecInvalid, "use none, hasExe or bat", "importer.mode %q is not known", imp.Mode)
	}
	if s.ERP != "hasavshevet" && imp.Mode != "none" {
		return refuse(CodeSpecInvalid, "only Hasavshevet has an importer", "importer %s is set for erp %s", imp.Mode, s.ERP)
	}
	abs := func(field, p string) *Refusal {
		if !filepath.IsAbs(strings.TrimSpace(p)) {
			return refuse(CodeSpecInvalid, "use an absolute path", "importer.%s %q is not absolute", field, p)
		}
		return nil
	}
	switch imp.Mode {
	case "none":
		if imp.HasExePath != "" || imp.HasParamFile != "" || imp.HasBatFile != "" {
			return refuse(CodeSpecInvalid, "leave the paths out with mode none", "importer none carries paths")
		}
	case "hasExe":
		if imp.HasBatFile != "" {
			return refuse(CodeSpecInvalid, "use mode bat for a BAT", "importer hasExe carries hasBatFile")
		}
		if r := abs("hasExePath", imp.HasExePath); r != nil {
			return r
		}
		// hasParamFile goes to has.exe as given, run from sendOrderDir: a
		// bare name like digi_perm.bat is legitimate.
		if hasControl(imp.HasParamFile) {
			return refuse(CodeSpecInvalid, "use a file name or an absolute path", "importer.hasParamFile is not a path")
		}
	case "bat":
		if imp.HasExePath != "" || imp.HasParamFile != "" {
			return refuse(CodeSpecInvalid, "use mode hasExe for has.exe", "importer bat carries hasExePath or hasParamFile")
		}
		if r := abs("hasBatFile", imp.HasBatFile); r != nil {
			return r
		}
	}
	return nil
}

func (s Spec) validateAuth() *Refusal {
	if !authUserRe.MatchString(s.Auth.Username) {
		return refuse(CodeSpecInvalid, "use up to 64 letters, digits, . _ @ or -", "auth.username is not a valid user name")
	}
	if len(s.Auth.Password) < MinAuthPassword || len(s.Auth.Password) > 256 || hasControl(s.Auth.Password) ||
		strings.TrimSpace(s.Auth.Password) != s.Auth.Password {
		return refuse(CodeAuthPasswordWeak, fmt.Sprintf("send a generated password of %d to 256 characters", MinAuthPassword),
			"auth.password is too short, too long or not plain text")
	}
	return nil
}

func validateFirewall(f *FirewallSpec) *Refusal {
	if f == nil {
		return nil
	}
	if f.RuleName != "" && !ruleNameRe.MatchString(f.RuleName) {
		return refuse(CodeSpecInvalid, "use up to 64 letters, digits, spaces, . _ ( ) or -", "firewall.ruleName is not a valid name")
	}
	if len(f.AllowFrom) == 0 || len(f.AllowFrom) > 16 {
		return refuse(CodeSpecInvalid, "name 1 to 16 source addresses", "firewall.allowFrom has %d entries", len(f.AllowFrom))
	}
	for _, a := range f.AllowFrom {
		n, err := parseSource(a)
		if err != nil {
			return refuse(CodeSpecInvalid, "use an IPv4 address or CIDR", "firewall.allowFrom %q: %v", a, err)
		}
		if ones, _ := n.Mask.Size(); ones < 16 {
			return refuse(CodeFirewallAny, "name the callers: the Hub's NAT IP and the tenant's", "firewall.allowFrom %s opens the port to too many addresses", a)
		}
	}
	return nil
}

// parseSource reads an IPv4 address (a /32) or an IPv4 CIDR.
func parseSource(a string) (*net.IPNet, error) {
	a = strings.TrimSpace(a)
	if !strings.Contains(a, "/") {
		ip := net.ParseIP(a)
		if ip == nil || ip.To4() == nil {
			return nil, errors.New("not an IPv4 address")
		}
		return &net.IPNet{IP: ip.To4(), Mask: net.CIDRMask(32, 32)}, nil
	}
	ip, n, err := net.ParseCIDR(a)
	if err != nil || ip.To4() == nil {
		return nil, errors.New("not an IPv4 CIDR")
	}
	return n, nil
}

func validateTLS(t *TLSSpec) *Refusal {
	if t == nil {
		return nil
	}
	switch t.Mode {
	case "off":
		if len(t.Hosts) > 0 {
			return refuse(CodeSpecInvalid, "leave hosts out with mode off", "tls off carries hosts")
		}
	case "self-signed":
		if len(t.Hosts) == 0 || len(t.Hosts) > 8 {
			return refuse(CodeSpecInvalid, "name 1 to 8 hosts the callers use (the public IP first)", "tls.hosts has %d entries", len(t.Hosts))
		}
		for _, h := range t.Hosts {
			if net.ParseIP(h) == nil && !dnsNameRe.MatchString(h) {
				return refuse(CodeSpecInvalid, "use an IP or a DNS name", "tls host %q is neither an IP nor a DNS name", h)
			}
		}
	default:
		return refuse(CodeSpecInvalid, "use self-signed or off", "tls.mode %q is not known", t.Mode)
	}
	return nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// Validate checks a discover credential before it goes anywhere near a DSN:
// a host that is not a host would make the driver's URL parser quote the DSN,
// password included, in its error.
func (d DiscoverDB) Validate() *Refusal {
	if d.Host != "" {
		server, instance := db.SplitInstance(d.Host)
		if !serverRe.MatchString(server) || (instance != "" && !instanceRe.MatchString(instance)) {
			return refuse(CodeSpecInvalid, `use a host name or IP, optionally host\instance`, "db.host is not a host")
		}
	}
	if d.Port < 0 || d.Port > 65535 {
		return refuse(CodeSpecInvalid, "use a port from 1 to 65535, or none", "db.port %d is not a port", d.Port)
	}
	if strings.TrimSpace(d.User) == "" || hasControl(d.User) || d.Password == "" || hasControl(d.Password) {
		return refuse(CodeSpecInvalid, "send the login and its password", "db.user or db.password is missing or not text")
	}
	if d.Database != "" && !hasavshevet.IsSafeDBName(d.Database) {
		return refuse(CodeSpecInvalid, "use the database's name: letters, digits and _ only", "db.database is not a database name")
	}
	return nil
}

// secrets lists the spec's secret values, for Redact.
func (s Spec) secrets() []string {
	return []string{s.DB.Password, s.Auth.Password}
}
