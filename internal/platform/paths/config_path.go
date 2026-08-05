package paths

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

const AppName = "digi-erp-connector"

// defaultProgramData is used only if the environment does not define
// PROGRAMDATA, which should not happen on a healthy Windows install.
const defaultProgramData = `C:\ProgramData`

// ErrUnsupportedOS is returned for platforms with no machine-wide config
// location. The daemon supports Windows and Linux; macOS is not a target.
var ErrUnsupportedOS = errors.New("unsupported OS for machine-wide config")

// DataDir returns the machine-wide data directory holding config.yaml,
// queries.json, the logs and the secrets subdirectory.
//
// Windows: %PROGRAMDATA%\digi-erp-connector (read from the environment, so
// tests can redirect it). Linux: /etc/digi-erp-connector.
func DataDir() string {
	if runtime.GOOS == "windows" {
		programData := os.Getenv("PROGRAMDATA")
		if programData == "" {
			programData = defaultProgramData
		}
		return filepath.Join(programData, AppName)
	}
	return filepath.Join("/etc", AppName)
}

// ConfigFilePath returns the path of config.yaml inside DataDir.
func ConfigFilePath() (string, error) {
	switch runtime.GOOS {
	case "windows", "linux":
		return filepath.Join(DataDir(), "config.yaml"), nil
	default:
		return "", ErrUnsupportedOS
	}
}
