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
		Queries: QueriesConfig{
			TimeoutSeconds: 30,
			MaxRows:        10000,
		},
	}
}
