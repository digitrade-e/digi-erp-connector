package paths

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

func LoggerFilePath() (string, error) {
	switch runtime.GOOS {
	case "windows":
		programData := os.Getenv("PROGRAMDATA")
		if programData == "" {
			programData = `C:\ProgramData`
		}
		return filepath.Join(programData, AppName, "server.log"), nil
	case "linux":
		return filepath.Join("/etc", AppName, "server.log"), nil
	default:
		return "", errors.New("unsupported OS for machine-wide logger")
	}
}
