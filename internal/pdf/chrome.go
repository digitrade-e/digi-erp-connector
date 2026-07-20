package pdf

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// DetectChrome searches for Chrome/Chromium on the system.
// Returns the absolute path or empty string if not found.
func DetectChrome() string {
	if runtime.GOOS == "windows" {
		return detectChromeWindows()
	}
	return detectChromeLinux()
}

func detectChromeWindows() string {
	localAppData := os.Getenv("LOCALAPPDATA")
	programFiles := os.Getenv("ProgramFiles")
	programFilesX86 := os.Getenv("ProgramFiles(x86)")

	candidates := []string{
		filepath.Join(programFiles, `Google\Chrome\Application\chrome.exe`),
		filepath.Join(programFilesX86, `Google\Chrome\Application\chrome.exe`),
		filepath.Join(localAppData, `Google\Chrome\Application\chrome.exe`),
		filepath.Join(programFiles, `Chromium\Application\chrome.exe`),
		filepath.Join(programFilesX86, `Chromium\Application\chrome.exe`),
	}

	for _, p := range candidates {
		if p == "" {
			continue
		}
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

func detectChromeLinux() string {
	candidates := []string{
		"/usr/bin/google-chrome-stable",
		"/usr/bin/google-chrome",
		"/opt/google/chrome/chrome",
		"/usr/bin/chromium-browser",
		"/usr/bin/chromium",
		"/snap/bin/chromium",
	}

	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}

	// Fallback: PATH lookup
	for _, name := range []string{"google-chrome", "chromium-browser", "chromium", "chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}

	return ""
}
