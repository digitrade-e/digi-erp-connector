//go:build windows

package provision

import (
	"errors"
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

// Review R4-1 on this machine: FinalPath reads the path Windows resolves a folder to — the CI's temp folder is
// C:\Users\RUNNER~1\…, an 8.3 spelling, and comes back as its long name, so the judgment refuses the short one — and
// one that does not exist says so.
func TestWinSystem_FinalPath(t *testing.T) {
	dir := t.TempDir()
	long, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	final, err := winSystem{}.FinalPath(dir)
	fp, onDrive := finalDrivePath(final)
	if err != nil || !onDrive || !strings.EqualFold(filepath.Clean(fp), filepath.Clean(long)) {
		t.Fatalf("FinalPath(%s) = %q (%v), want %s", dir, final, err, long)
	}
	t.Logf("%s resolves to %s", dir, fp)
	if _, err := (winSystem{}).FinalPath(filepath.Join(dir, "not-yet")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a folder that does not exist: %v", err)
	}
	file := filepath.Join(dir, "a-file")
	_ = os.WriteFile(file, []byte("x"), 0o600)
	if _, err := (winSystem{}).FinalPath(file); err == nil {
		t.Fatal("a file resolved as a folder")
	}
}

// Review m6's rest (v1.0.32) on this machine: an importer file resolves to
// its long name (the CI's temp folder is 8.3), a folder is no file, and a
// file behind a junction shows the junction on its way.
func TestWinSystem_FinalFilePath(t *testing.T) {
	dir := t.TempDir()
	long, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "has.exe")
	if err := os.WriteFile(exe, []byte("MZ"), 0o600); err != nil {
		t.Fatal(err)
	}
	final, err := winSystem{}.FinalFilePath(exe)
	fp, onDrive := finalDrivePath(final)
	if err != nil || !onDrive || !strings.EqualFold(filepath.Clean(fp), filepath.Join(long, "has.exe")) {
		t.Fatalf("FinalFilePath(%s) = %q (%v)", exe, final, err)
	}
	if _, err := (winSystem{}).FinalFilePath(dir); err == nil {
		t.Error("a folder resolved as a file")
	}
	if _, err := (winSystem{}).FinalFilePath(filepath.Join(dir, "none.exe")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a file not there: %v", err)
	}
	j := filepath.Join(long, "j")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", j, long).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v %s", err, out)
	}
	if at, err := (winSystem{}).ReparsePointOn(filepath.Join(j, "has.exe")); err != nil || at != j {
		t.Errorf("has.exe through a junction: %q %v", at, err)
	}
}
