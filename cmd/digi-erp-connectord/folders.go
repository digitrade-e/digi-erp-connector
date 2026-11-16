package main

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
)

// folderWritersFn reads who besides the administrators can write to a folder
// (dataacl.FolderWriters; tests replace it): the folder guard's reader
// (hasavshevet.FolderGuard, plan 087 review M1/R3).
var folderWritersFn = dataacl.FolderWriters

// linkedBox reports whether the box is behind the link: link.yaml in the
// data directory names a box or a pairing code — provision.LinkMode's rule
// (internal/link's file, read here without the link package).
func linkedBox(dataDir string) bool {
	b, err := os.ReadFile(filepath.Join(dataDir, "link.yaml"))
	if err != nil {
		return false
	}
	var st struct {
		Box      string `yaml:"box"`
		PairCode string `yaml:"pairCode"`
	}
	if yaml.Unmarshal(b, &st) != nil {
		return false
	}
	return st.Box != "" || st.PairCode != ""
}
