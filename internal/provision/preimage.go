package provision

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/buildinfo"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/atomicfile"
)

// keepPreimages is how many change sets' pre-images stay on the box. Each
// holds a copy of config.yaml, whose credential is in clear, under the data
// directory's DACL.
const keepPreimages = 5

// manifest describes one change set's pre-image: what was there before it
// wrote anything. The first capture of a change set wins — a re-run must not
// overwrite the state from before the change set began.
type manifest struct {
	ChangeSet string    `json:"changeSet"`
	Created   time.Time `json:"created"`
	Tool      string    `json:"tool"`
	// Files are paths relative to the data directory. A file that was absent
	// is restored by removing it.
	Files    []preFile    `json:"files"`
	Service  *preService  `json:"service,omitempty"`
	Firewall *preFirewall `json:"firewall,omitempty"`
}

type preFile struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
	SHA256  string `json:"sha256,omitempty"`
}

type preService struct {
	Dependencies []string `json:"dependencies"`
}

type preFirewall struct {
	Name string        `json:"name"`
	Rule *FirewallRule `json:"rule"` // nil: there was no rule of that name
}

func provisionDir(dataDir string) string { return filepath.Join(dataDir, "provision") }

func preimageDir(dataDir, changeSet string) string {
	return filepath.Join(provisionDir(dataDir), changeSet)
}

// capturePreimage records files (relative to dataDir), the service's
// dependencies and the firewall rule, unless this change set already has a
// pre-image. It reports whether it wrote one.
func capturePreimage(dataDir, changeSet string, files []string, svc *preService, fw *preFirewall, now time.Time) (bool, error) {
	dir := preimageDir(dataDir, changeSet)
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err == nil {
		return false, nil
	}
	m := manifest{ChangeSet: changeSet, Created: now.UTC(), Tool: buildinfo.Version, Service: svc, Firewall: fw}
	for _, rel := range dedupe(files) {
		b, err := os.ReadFile(filepath.Join(dataDir, rel))
		switch {
		case errors.Is(err, os.ErrNotExist):
			m.Files = append(m.Files, preFile{Path: rel})
		case err != nil:
			return false, fmt.Errorf("read %s for the pre-image: %w", rel, err)
		default:
			if err := atomicfile.Write(filepath.Join(dir, "files", rel), b, 0o600); err != nil {
				return false, err
			}
			sum := sha256.Sum256(b)
			m.Files = append(m.Files, preFile{Path: rel, Present: true, SHA256: hex.EncodeToString(sum[:])})
		}
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return false, err
	}
	// The manifest goes last: a capture that stopped half way has none, and
	// the next run captures again.
	return true, atomicfile.Write(filepath.Join(dir, "manifest.json"), out, 0o600)
}

func loadPreimage(dataDir, changeSet string) (manifest, error) {
	var m manifest
	b, err := os.ReadFile(filepath.Join(preimageDir(dataDir, changeSet), "manifest.json"))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("the pre-image manifest does not decode: %w", err)
	}
	return m, nil
}

// restoreFiles puts the pre-image's files back, and removes the ones that
// were not there. It reports whether anything changed.
func restoreFiles(dataDir string, m manifest) (bool, error) {
	changed := false
	for _, f := range m.Files {
		dst := filepath.Join(dataDir, f.Path)
		cur, curErr := os.ReadFile(dst)
		if !f.Present {
			if curErr == nil {
				if err := os.Remove(dst); err != nil {
					return changed, fmt.Errorf("remove %s: %w", f.Path, err)
				}
				changed = true
			}
			continue
		}
		b, err := os.ReadFile(filepath.Join(preimageDir(dataDir, m.ChangeSet), "files", f.Path))
		if err != nil {
			return changed, fmt.Errorf("read the pre-image of %s: %w", f.Path, err)
		}
		if curErr == nil && slices.Equal(cur, b) {
			continue
		}
		if err := atomicfile.Write(dst, b, 0o600); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

// prunePreimages keeps the keepPreimages newest pre-images (and always
// current's) and removes the rest.
func prunePreimages(dataDir, current string) error {
	entries, err := os.ReadDir(provisionDir(dataDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	type dated struct {
		name string
		at   time.Time
	}
	var all []dated
	for _, e := range entries {
		if !e.IsDir() || e.Name() == current {
			continue
		}
		m, err := loadPreimage(dataDir, e.Name())
		at := m.Created
		if err != nil {
			if info, ierr := e.Info(); ierr == nil {
				at = info.ModTime()
			}
		}
		all = append(all, dated{e.Name(), at})
	}
	slices.SortFunc(all, func(a, b dated) int { return b.at.Compare(a.at) })
	for i, d := range all {
		if i < keepPreimages-1 {
			continue
		}
		if err := os.RemoveAll(filepath.Join(provisionDir(dataDir), d.name)); err != nil {
			return err
		}
	}
	return nil
}

// preimages lists the change sets with a pre-image, newest first.
func preimages(dataDir string) []string {
	entries, err := os.ReadDir(provisionDir(dataDir))
	if err != nil {
		return nil
	}
	type dated struct {
		name string
		at   time.Time
	}
	var all []dated
	for _, e := range entries {
		if m, err := loadPreimage(dataDir, e.Name()); err == nil && e.IsDir() {
			all = append(all, dated{e.Name(), m.Created})
		}
	}
	slices.SortFunc(all, func(a, b dated) int { return b.at.Compare(a.at) })
	out := make([]string, 0, len(all))
	for _, d := range all {
		out = append(out, d.name)
	}
	return out
}

func dedupe(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}
