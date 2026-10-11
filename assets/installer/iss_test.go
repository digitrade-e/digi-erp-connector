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
