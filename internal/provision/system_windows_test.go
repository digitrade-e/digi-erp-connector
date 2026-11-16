//go:build windows

package provision

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// Plan 087 review T3 on this machine: a junction on the way to a folder is
// found — the one Windows ships (C:\Documents and Settings) and the All
// Users link included — and the machine's rules refuse ProgramData, the
// profiles and the Windows folder but take folders like BFL's.
func TestFolderRules_ThisMachine(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "real", "herp")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	j := filepath.Join(root, "j")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", j, filepath.Join(root, "real")).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v %s", err, out)
	}
	sys := winSystem{}
	if at, err := sys.ReparsePointOn(filepath.Join(j, "herp")); err != nil || at != j {
		t.Errorf("through a junction: %q, %v", at, err)
	}
	if at, err := sys.ReparsePointOn(target); err != nil || at != "" {
		t.Errorf("a plain folder: %q, %v", at, err)
	}
	drive := os.Getenv("SystemDrive")
	for _, link := range []string{drive + `\Users\All Users`, drive + `\Documents and Settings`} {
		if _, err := os.Lstat(link); err != nil {
			continue // not on this edition
		}
		if at, err := sys.ReparsePointOn(filepath.Join(link, "x", "y")); err != nil || !strings.EqualFold(at, link) {
			t.Errorf("%s: %q, %v", link, at, err)
		}
	}
	r := sys.FolderRules()
	for _, bad := range []string{os.Getenv("ProgramData") + `\Vendor\plugins`, drive + `\Users\Public\Documents`, drive + `\Users\Default`,
		os.Getenv("SystemRoot") + `\Temp`, os.Getenv("ProgramFiles") + `\Hash7`, drive + `\`, `D:\work\AppData\x`} {
		if documentFolderRefusal(bad, r) == "" {
			t.Errorf("%s accepted (rules %+v)", bad, r)
		}
	}
	for _, ok := range []string{drive + `\xampp\htdocs\herp`, drive + `\xampp\htdocs\herp\receipts`} {
		if why := documentFolderRefusal(ok, r); why != "" {
			t.Errorf("%s refused: %s", ok, why)
		}
	}
}
