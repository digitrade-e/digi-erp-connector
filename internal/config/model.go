package config

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
}

// PDFConfig holds print/email toggles + remote-template integration. Branding
// (company name, address, logo, footer) lives entirely in the backend's
// AppSettings now — the connector fetches pre-rendered HTML and runs only
// the chromedp HTML→PDF + print/email steps.
type PDFConfig struct {
	PrintAfterOrder bool   `yaml:"printAfterOrder"`
	PrinterName     string `yaml:"printerName"`    // empty = system default
	EmailAfterOrder bool   `yaml:"emailAfterOrder"`

	ChromePath     string `yaml:"chromePath"`     // auto-detected if empty
	SumatraPDFPath string `yaml:"sumatraPdfPath"` // auto-detected if empty

	// Remote template — the connector fetches a pre-rendered HTML document
	// from the backend and converts it to PDF locally via chromedp.
	RemoteTemplateBaseURL string            `yaml:"remoteTemplateBaseURL"`           // e.g. "https://api.example.com" — no path/api suffix
	RemoteTokens          map[string]string `yaml:"remoteTokens,omitempty"`          // documentType → token (32-byte hex)
	UseRemoteTemplate     bool              `yaml:"useRemoteTemplate"`               // master switch; defaults true now that local template is gone
	RemoteTimeoutSeconds  int               `yaml:"remoteTimeoutSeconds,omitempty"` // 0 → use default (15s)
}

// SMTPConfig holds SMTP server settings for sending invoice emails.
// Password is stored separately in secrets/ (never in YAML).
type SMTPConfig struct {
	Host        string `yaml:"host"`
	Port        int    `yaml:"port"` // default: 587
	User        string `yaml:"user"`
	FromAddress string `yaml:"fromAddress"`
	UseTLS      bool   `yaml:"useTLS"` // default: true
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
	HasBatFile string        `yaml:"hasBatFile"`
	DB         DBConfig      `yaml:"db"`
	PDF        PDFConfig     `yaml:"pdf"`
	SMTP       SMTPConfig    `yaml:"smtp"`
	Queries    QueriesConfig `yaml:"queries"`
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
		PDF: PDFConfig{
			PrintAfterOrder:      false,
			EmailAfterOrder:      false,
			UseRemoteTemplate:    true,
			RemoteTimeoutSeconds: 15,
		},
		SMTP: SMTPConfig{
			Port:   587,
			UseTLS: true,
		},
		Queries: QueriesConfig{
			TimeoutSeconds: 30,
			MaxRows:        10000,
		},
	}
}
