package installer

import (
	"os"
	"strings"
	"testing"
)

// The upgrade over the link (plan 086 D2) runs a setup only when the release's SHA256SUMS lists it: the release
// workflow adds the installer's line after the installer is built and before the release is published, under the
// name the link composes.
func TestTheReleaseListsTheInstallerInSHA256SUMS(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/release-windows.yml")
	if err != nil {
		t.Fatal(err)
	}
	wf := string(b)
	build := strings.Index(wf, "- name: Build installer")
	list := strings.Index(wf, "sha256sum digi-erp-connector-setup-*.exe >> SHA256SUMS")
	publish := strings.Index(wf, "- name: Create GitHub Release")
	if build < 0 || list < 0 || publish < 0 || !(build < list && list < publish) {
		t.Fatalf("the installer is not listed in SHA256SUMS between its build and the release (%d, %d, %d)", build, list, publish)
	}
	var named bool
	for _, l := range issLines(t) {
		if strings.TrimSpace(l) == "OutputBaseFilename=digi-erp-connector-setup-{#AppVersion}" {
			named = true
		}
	}
	if !named {
		t.Error("the installer's file name is not digi-erp-connector-setup-<version>.exe, which the link downloads")
	}
}
