package paths

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

func UILogFilePath() (string, error) {
	switch runtime.GOOS {
	case "windows":
		programData := os.Getenv("PROGRAMDATA")
		if programData == "" {
			programData = `C:\ProgramData`
		}
		return filepath.Join(programData, AppName, "ui.log"), nil
	case "linux":
		return filepath.Join("/etc", AppName, "ui.log"), nil
	default:
		return "", errors.New("unsupported OS for UI logger")
	}
}
