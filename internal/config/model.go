package config

import "time"

type ERPType string
type DBDriver string

const (
	ERPSAP         ERPType = "sap"
	ERPHasavshevet ERPType = "hasavshevet"
	ERPPriority    ERPType = "priority"
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

// LegacyCompatConfig turns on the electron-mssql-app compatibility surface so
// backends written against the old Node connector keep working unchanged:
// the POST /auth/token JWT exchange, GET /api/ping, POST /api/test-connection,
// the sample /api/customers + /api/orders/{id} routes, and POST /api/query.
//
// It exists for cutover only. Every legacy route logs when hit, so you can see
// what the backend still relies on and switch this off once nothing does.
// Disabled by default: a fresh install has none of this surface.
type LegacyCompatConfig struct {
	Enabled bool `yaml:"enabled"`
	// JWTSecret signs and verifies legacy tokens. Must match what the old
	// connector used, otherwise already-issued backend tokens stop verifying.
	JWTSecret string `yaml:"jwtSecret"`
	// JWTUser / JWTPassword are the credentials POST /auth/token accepts.
	JWTUser     string `yaml:"jwtUser"`
	JWTPassword string `yaml:"jwtPassword"`
	// JWTExpiryMinutes is the issued token lifetime (electron used 30).
	JWTExpiryMinutes int `yaml:"jwtExpiryMinutes,omitempty"`
	// AllowRawSQL enables POST /api/query (SELECT/WITH only, validated). Kept
	// as its own switch so the raw-SQL route can be retired independently of
	// the JWT exchange.
	AllowRawSQL bool `yaml:"allowRawSQL"`
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
	BearerToken  string   `yaml:"bearerToken"`
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
	HasBatFile   string             `yaml:"hasBatFile"`
	DB           DBConfig           `yaml:"db"`
	Queries      QueriesConfig      `yaml:"queries"`
	LegacyCompat LegacyCompatConfig `yaml:"legacyCompat"`
}

// LegacyJWTExpiry returns the configured legacy token lifetime, defaulting to
// electron-mssql-app's 30 minutes.
func (c LegacyCompatConfig) LegacyJWTExpiry() time.Duration {
	if c.JWTExpiryMinutes <= 0 {
		return 30 * time.Minute
	}
	return time.Duration(c.JWTExpiryMinutes) * time.Minute
}

func ErpValues() []ERPType {
	return []ERPType{ERPSAP, ERPHasavshevet, ERPPriority}
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
		BearerToken:  "",
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
		// Off by default: a fresh install exposes no legacy surface and no
		// credentials live in the binary. Cutover installs replacing an
		// electron-mssql-app deployment enable it in config.yaml and supply
		// the old connector's secret/credentials there.
		LegacyCompat: LegacyCompatConfig{
			Enabled:          false,
			JWTExpiryMinutes: 30,
		},
	}
}
