// Package installer holds only this test: nothing in CI compiles the Inno
// Setup script before a release (iscc runs only in release-windows on main), so
// its structure is checked here, where `go test ./...` runs on every push.
package installer

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

const issPath = "digi-erp-connector.iss"

func issLines(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(issPath)
	if err != nil {
		t.Fatalf("read %s: %v", issPath, err)
	}
	for i, c := range b {
		if c > 0x7f {
			t.Fatalf("%s has a non-ASCII byte at offset %d", issPath, i)
		}
	}
	if !strings.Contains(string(b), "\r\n") || strings.Count(string(b), "\n") != strings.Count(string(b), "\r\n") {
		t.Errorf("%s must be CRLF throughout (.gitattributes pins *.iss to eol=crlf)", issPath)
	}
	return strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
}

var sectionHeader = regexp.MustCompile(`^\[([A-Za-z]+)\]\s*$`)

// The script's sections are all there, [Code] is one of them, and the Pascal
// the installer depends on sits inside it. A rewrite once dropped the [Code]
// line, leaving PrepareToInstall inside [Run]: iscc would refuse the script,
// and nothing before the release workflow would have noticed.
func TestInstallerScriptStructure(t *testing.T) {
	lines := issLines(t)

	at := map[string]int{}
	var order []string
	for i, l := range lines {
		if m := sectionHeader.FindStringSubmatch(l); m != nil {
			if _, dup := at[m[1]]; dup {
				t.Errorf("section [%s] appears twice", m[1])
			}
			at[m[1]] = i
			order = append(order, m[1])
		}
	}
	for _, want := range []string{"Setup", "Files", "Icons", "Run", "Code"} {
		if _, ok := at[want]; !ok {
			t.Errorf("section [%s] is missing; sections found: %v", want, order)
		}
	}
	code, ok := at["Code"]
	if !ok {
		return
	}
	if order[len(order)-1] != "Code" {
		t.Errorf("[Code] must be the last section; order is %v", order)
	}

	pascal := regexp.MustCompile(`^\s*(function|procedure|begin|end;|const|var|//)`)
	for i, l := range lines[:code] {
		if pascal.MatchString(l) {
			t.Errorf("line %d is Pascal outside [Code]: %q", i+1, l)
		}
	}
	for _, fn := range []string{"function PrepareToInstall(", "procedure CurUninstallStepChanged(", "function StopServiceAndWait("} {
		found := false
		for _, l := range lines[code:] {
			if strings.HasPrefix(strings.TrimSpace(l), fn) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s… is not declared inside [Code]", fn)
		}
	}
	// The uninstall order lives in [Code] now; an [UninstallRun] "sc delete"
	// would run before the stop has finished.
	if _, ok := at["UninstallRun"]; ok {
		t.Error("[UninstallRun] is back; stop/wait/delete belongs to CurUninstallStepChanged")
	}
}

// R4-2b: `sc failure` restarts the service after every failure — 1 min,
// 5 min, then every 30 min — and failureflag makes a non-zero exit (the data
// directory refused) count as one. An empty or "none" action would leave the
// node down after the second failure.
func TestInstallerSetsRestartOnFailure(t *testing.T) {
	var failure, flag string
	for _, l := range issLines(t) {
		if strings.Contains(l, "sc.exe failure ") {
			failure = l
		}
		if strings.Contains(l, "sc.exe failureflag ") {
			flag = l
		}
	}
	m := regexp.MustCompile(`reset= (\d+) actions= (\S+?) >nul`).FindStringSubmatch(failure)
	if m == nil {
		t.Fatalf("no parseable sc failure line: %q", failure)
	}
	if m[1] != "86400" {
		t.Errorf("reset= %s; want 86400", m[1])
	}
	parts := strings.Split(m[2], "/")
	want := []string{"restart", "60000", "restart", "300000", "restart", "1800000"}
	if strings.Join(parts, "/") != strings.Join(want, "/") {
		t.Errorf("actions= %s; want %s", m[2], strings.Join(want, "/"))
	}
	if !strings.Contains(flag, "failureflag {#ServiceName} 1 ") {
		t.Errorf("failureflag line %q; want the flag set to 1", flag)
	}
}

// Plan 068: digi-mcp runs digi-erp-connector-provision.exe from the install
// folder over WinRM, so the installer must put it there next to the daemon.
func TestInstallerShipsTheProvisionTool(t *testing.T) {
	var define, file bool
	for _, l := range issLines(t) {
		if strings.TrimSpace(l) == `#define ProvisionExe "digi-erp-connector-provision.exe"` {
			define = true
		}
		if strings.HasPrefix(l, `Source: "{#BuildDir}\{#ProvisionExe}"; DestDir: "{app}"`) {
			file = true
		}
	}
	if !define || !file {
		t.Errorf("the provision tool is not installed: define %v, [Files] line %v", define, file)
	}
}
