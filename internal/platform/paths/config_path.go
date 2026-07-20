package paths

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

const AppName = "digi-erp-connector"

func ConfigFilePath() (string, error) {
	switch runtime.GOOS {
	case "windows":
		programData := os.Getenv("PROGRAMDATA")
		if programData == "" {
			programData = `C:\ProgramData`
		}
		return filepath.Join(programData, AppName, "config.yaml"), nil
	case "linux":
		return filepath.Join("/etc", AppName, "config.yaml"), nil
	default:
		return "", errors.New("unsupported OS for machine-wide config")
	}
}

// DataDir returns the application data directory where config, logs, and
// generated files (e.g. test PDFs) are stored.
func DataDir() string {
	switch runtime.GOOS {
	case "windows":
		programData := os.Getenv("PROGRAMDATA")
		if programData == "" {
			programData = `C:\ProgramData`
		}
		return filepath.Join(programData, AppName)
	default:
		return filepath.Join("/etc", AppName)
	}
}
