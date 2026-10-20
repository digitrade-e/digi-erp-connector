//go:build windows

package provision

import (
	"os"
	"path/filepath"
	"testing"
)

// The host script runs on this machine's PowerShell and decodes, the plan 068
// panel's two facts included.
func TestWinSystem_Host(t *testing.T) {
	h, err := winSystem{}.Host()
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if h.Name == "" || h.OS == "" || h.PowerShell == "" {
		t.Errorf("host %+v", h)
	}
	t.Logf("domainRole %d, winrmAnalytic %v, localAccountTokenFilterPolicy %d", h.DomainRole, h.WinRMAnalytic, h.LocalAccountTokenFilterPolicy)
}

func TestWinSystem_Writers(t *testing.T) {
	p := filepath.Join(t.TempDir(), "importorders.bat")
	if err := os.WriteFile(p, []byte("@echo off\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (winSystem{}).Writers(p); err != nil {
		t.Fatalf("Writers: %v", err)
	}
}
