package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type ERPType string
type DBDriver string

const (
	ERPSAP         ERPType = "sap"
	ERPHasavshevet ERPType = "hasavshevet"
)

const (
	DBDriverMSSQL DBDriver = "mssql"
)

type DBConfig struct {
	Driver   DBDriver `yaml:"driver"`
	Host     string   `yaml:"host"`
	Port     int      `yaml:"port"`
	User     string   `yaml:"user"`
	Database string   `yaml:"database"`
	// Encrypt requests TLS for the connection. When false the DSN omits the
	// option entirely and the driver default applies — unchanged behaviour for
	// existing installs. electron-mssql-app set encrypt:true.
	Encrypt bool `yaml:"encrypt,omitempty"`
	// TrustServerCertificate skips certificate validation — required when SQL
	// Server presents a self-signed certificate, as a default local instance
	// does. Only meaningful together with Encrypt.
	TrustServerCertificate bool `yaml:"trustServerCertificate,omitempty"`
}

// TLSConfig serves the API over HTTPS instead of plaintext HTTP.
//
// Without it every request — including the bearer token — crosses the network in
// cleartext, which matters as soon as apiListen is anything but a loopback
// address. Configure it whenever the backend is on another machine.
//
// Both files must be PEM. A certificate chain goes in CertFile, leaf first.
type TLSConfig struct {
	CertFile string `yaml:"certFile,omitempty"`
	KeyFile  string `yaml:"keyFile,omitempty"`
}

// Enabled reports whether TLS was asked for. Setting only one of the two counts
// as enabled on purpose: the server then refuses to start rather than quietly
// serving plaintext when the operator believed they had configured HTTPS.
func (t TLSConfig) Enabled() bool {
	return strings.TrimSpace(t.CertFile) != "" || strings.TrimSpace(t.KeyFile) != ""
}

// AuthConfig holds this installation's only credential.
//
// A caller posts the username and password to /auth/token, gets a short-lived
// HS256 token back, and presents that token on every other route. There is no
// second way in: the static bearer token this connector used to accept was
// removed on 2026-08-09, because two credentials meant two things to rotate and
// two ways in for no benefit — erp-manager, the caller that matters, only ever
// used this one.
//
// Both weaknesses of the scheme it descends from are fixed here:
//   - Secret is generated per installation and never has a default. A constant
//     compiled into the binary would let anyone with the source mint tokens.
//   - Username and Password are set by the operator, not shipped.
type AuthConfig struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// Secret signs issued tokens. Generated on first run if empty; changing it
	// invalidates every token already issued, which callers recover from by
	// re-authenticating on the 401.
	Secret string `yaml:"secret"`
	// TokenTTL is a Go duration ("30m", "1h"). Empty means DefaultTokenTTL.
	TokenTTL string `yaml:"tokenTTL,omitempty"`
}

// Validate reports why this installation has no usable credential, so the daemon
// refuses to start and the GUI refuses to save rather than serving an exchange
// that accepts blanks — which would be an open door, not an inconvenience.
//
// Both callers share this one function: a check that lives in only one of them
// is a check the other can walk past.
//
// Secret is not required here — the daemon generates one on first run — but the
// server checks it separately, because by the time it builds a signer a missing
// secret means generation failed.
func (a AuthConfig) Validate() error {
	if strings.TrimSpace(a.Username) == "" || strings.TrimSpace(a.Password) == "" {
		return errors.New("auth.username and auth.password are required")
	}
	return nil
}

// TokenTTLValid reports whether TokenTTL parses. TTL falls back silently on a
// malformed value so a typo cannot take a running installation offline; this
// exists for the GUI, where the same typo can be pointed at while the operator
// is still in front of the field.
func (a AuthConfig) TokenTTLValid() bool {
	t := strings.TrimSpace(a.TokenTTL)
	if t == "" {
		return true
	}
	d, err := time.ParseDuration(t)
	return err == nil && d > 0
}

// DefaultTokenTTL matches what callers of this exchange have historically
// assumed. Note a caller may cache the token past its expiry and only
// re-authenticate when a request is rejected, so this bounds damage rather than
// controlling refresh timing.
const DefaultTokenTTL = 30 * time.Minute

// TTL returns the configured token lifetime, or the default when unset or
// unparseable. Malformed values fall back rather than failing startup: a typo
// here must not take an installation offline.
func (a AuthConfig) TTL() time.Duration {
	if strings.TrimSpace(a.TokenTTL) == "" {
		return DefaultTokenTTL
	}
	d, err := time.ParseDuration(a.TokenTTL)
	if err != nil || d <= 0 {
		return DefaultTokenTTL
	}
	return d
}

// QueriesConfig tunes execution of saved queries (the /api/sqlqueries runner).
type QueriesConfig struct {
	TimeoutSeconds int `yaml:"timeoutSeconds,omitempty"` // 0 → default (30s)
	MaxRows        int `yaml:"maxRows,omitempty"`        // 0 → default (10000)
}

type Config struct {
	ERP          ERPType  `yaml:"erp"`
	APIListen    string   `yaml:"apiListen"`
	Debug        bool     `yaml:"debug"`
	ERPUser      string   `yaml:"erpUser"`
	ImageFolders []string `yaml:"imageFolders"`
	// SendOrderDir is the working directory for Hasavshevet import files.
	// IMOVEIN.doc/.prm are written here; history/<orderNum>/ subdirs are created beneath it.
	SendOrderDir string `yaml:"sendOrderDir"`
	// HasExePath is the absolute path to has.exe (Hasavshevet importer, Windows only).
	// Leave empty to skip automatic import execution — files will still be written.
	HasExePath string `yaml:"hasExePath"`
	// HasParamFile is the parameter file passed to has.exe (e.g. digi_perm.bat).
	HasParamFile string `yaml:"hasParamFile"`
	// HasBatFile is the absolute path to the Masofon-generated BAT launcher
	// (e.g. C:\Hash7\digi.bat). When set, it is invoked via cmd.exe /C after
	// each order's IMOVEIN files are written. Takes precedence over HasExePath.
	// The BAT is executed from its own directory so relative paths inside it
	// (e.g. -p"digi.bat") resolve correctly.
	HasBatFile string `yaml:"hasBatFile"`

	// SendReceiptDir is the folder Hashavshevet's receipt importer reads
	// (KUPAIN_{n}.doc + KUPAIN.prm) — POST /api/sendReceipt. Unset means this
	// installation does not write receipts: the route answers 503
	// RECEIPTS_NOT_CONFIGURED and touches no disk. It must not be
	// SendOrderDir (ValidateReceipts). The receipt number store, the
	// idempotency index and the history copies live in the data directory,
	// never here. See docs/hasavshevet-send-receipt.md.
	SendReceiptDir string `yaml:"sendReceiptDir,omitempty"`
	// HasReceiptBatFile switches receipts to model (b): this BAT is run (via
	// cmd.exe /C, from its own folder, 90 s cap) once after each receipt file
	// is written. Empty (and no HasReceiptParamFile) is model (a): files only,
	// imported by hand. Absolute.
	HasReceiptBatFile string `yaml:"hasReceiptBatFile,omitempty"`
	// HasReceiptParamFile is model (b) the has.exe way, the mirror of
	// sendOrder's HasExePath + HasParamFile: the orders' has.exe (HasExePath)
	// is run with this parameter file, from SendReceiptDir, 90 s cap, once
	// after each receipt file is written. Absolute; needs an absolute
	// HasExePath; refused together with HasReceiptBatFile (ambiguous).
	HasReceiptParamFile string `yaml:"hasReceiptParamFile,omitempty"`
	// ReceiptCardNames overrides the built-in card-name table used for the
	// receipt's card name (KUPAIN F17), keyed by the raw HYP code.
	ReceiptCardNames ReceiptCardNamesConfig `yaml:"receiptCardNames,omitempty"`

	DB      DBConfig      `yaml:"db"`
	TLS     TLSConfig     `yaml:"tls"`
	Auth    AuthConfig    `yaml:"auth"`
	Queries QueriesConfig `yaml:"queries"`
}

// ReceiptCardNamesConfig is the receiptCardNames block: HYP Issuer and Brand
// codes (as strings) to the card name written in the receipt. An entry
// replaces the built-in name for that code only; an empty name means no
// override. hasavshevet.ValidateReceiptConfig refuses a name that cannot be
// written (not Windows-1255, wider than the field).
type ReceiptCardNamesConfig struct {
	Issuer map[string]string `yaml:"issuer,omitempty"`
	Brand  map[string]string `yaml:"brand,omitempty"`
}

// ReceiptsConfigured reports whether this installation writes receipts: it
// needs erp: hasavshevet and a sendReceiptDir. Derived, like orders, so a
// switch and the folder can never disagree.
func (c Config) ReceiptsConfigured() bool {
	return c.ERP == ERPHasavshevet && strings.TrimSpace(c.SendReceiptDir) != ""
}

// ValidateReceipts refuses an ambiguous or relative receipt import
// (validateReceiptImport, only when receipts are configured), a relative
// sendReceiptDir, and one that is the
// sendOrderDir. Both importers write fixed-name files into their folder and run
// from it, and the receipt folder sits under a web root on a real install;
// sharing one would mix the two imports. The comparison is after Clean and symlink resolution,
// and case-insensitive on Windows, so a trailing slash, a "..", a link or a
// different case cannot sneak the same folder past it.
//
// The daemon (api.NewServer) and the GUI save path both call it, through
// hasavshevet.ValidateReceiptConfig.
func (c Config) ValidateReceipts() error {
	// The import keys are checked only where they are used: a stray line on a
	// connector that writes no receipts (no sendReceiptDir, or erp: sap) must
	// never stop it serving orders.
	if c.ReceiptsConfigured() {
		if err := c.validateReceiptImport(); err != nil {
			return err
		}
	}
	order, receipt := strings.TrimSpace(c.SendOrderDir), strings.TrimSpace(c.SendReceiptDir)
	if receipt == "" {
		return nil
	}
	// A relative folder would resolve against the service's working
	// directory — System32 under the service manager — not where the
	// operator meant.
	if !filepath.IsAbs(receipt) {
		return fmt.Errorf("sendReceiptDir %q must be an absolute path", receipt)
	}
	if order == "" {
		return nil
	}
	if sameDir(order, receipt, runtime.GOOS == "windows") {
		return fmt.Errorf("sendReceiptDir %q is the sendOrderDir: receipts need a folder of their own", receipt)
	}
	return nil
}

// validateReceiptImport checks the model-(b) keys: at most one runner, and
// every path it runs absolute (a relative one would resolve against the
// service's working directory, System32). hasExePath is checked only when the
// receipt import uses it: the order importer has never required it absolute.
func (c Config) validateReceiptImport() error {
	bat := strings.TrimSpace(c.HasReceiptBatFile)
	prm := strings.TrimSpace(c.HasReceiptParamFile)
	exe := strings.TrimSpace(c.HasExePath)
	if bat != "" && prm != "" {
		return errors.New("hasReceiptBatFile and hasReceiptParamFile are both set: the receipt import is ambiguous — keep one")
	}
	if bat != "" && !filepath.IsAbs(bat) {
		return fmt.Errorf("hasReceiptBatFile %q must be an absolute path", bat)
	}
	if prm == "" {
		return nil
	}
	if !filepath.IsAbs(prm) {
		return fmt.Errorf("hasReceiptParamFile %q must be an absolute path", prm)
	}
	if exe == "" {
		return errors.New("hasReceiptParamFile is set but hasExePath is not: the receipt import runs the orders' has.exe")
	}
	if !filepath.IsAbs(exe) {
		return fmt.Errorf("hasExePath %q must be an absolute path when hasReceiptParamFile is set", exe)
	}
	return nil
}

// sameDir reports whether a and b name the same folder after Abs, Clean and
// (when the path exists) symlink resolution; foldCase compares ignoring case.
func sameDir(a, b string, foldCase bool) bool {
	ca, cb := canonicalDir(a), canonicalDir(b)
	if foldCase {
		return strings.EqualFold(ca, cb)
	}
	return ca == cb
}

func canonicalDir(p string) string {
	p = filepath.Clean(strings.TrimSpace(p))
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	// A folder that does not exist yet has nothing to resolve; its cleaned
	// absolute form is what it will be.
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return p
}

func ErpValues() []ERPType {
	return []ERPType{ERPSAP, ERPHasavshevet}
}

func ErpOption() []string {
	vals := ErpValues()
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		out = append(out, string(v))
	}
	return out
}

func DBDriverValues() []DBDriver {
	return []DBDriver{DBDriverMSSQL}
}

func DBDriverOptions() []string {
	vals := DBDriverValues()
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		out = append(out, string(v))
	}
	return out
}

func Default() Config {
	return Config{
		ERP:          ERPHasavshevet,
		APIListen:    "127.0.0.1:8080",
		Debug:        false,
		ImageFolders: []string{},
		SendOrderDir: "",
		HasExePath:   "",
		HasParamFile: "",
		HasBatFile:   "",
		DB: DBConfig{
			Driver:   "mssql",
			Host:     "localhost",
			Port:     1433,
			Database: "",
			User:     "",
		},
		Queries: QueriesConfig{
			TimeoutSeconds: 30,
			MaxRows:        10000,
		},
	}
}
