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

// A pre-image is what was there before something wrote: files (relative to
// the data directory; an absent one is restored by removing it), the
// service's dependencies and the firewall rule.
//
// There are two per change set:
//   - provision\<changeSet>\ is the state before the change set's FIRST run.
//     A re-run never overwrites it: it is what Restore puts back.
//   - provision\<changeSet>\run\ is the state before the CURRENT run, taken
//     afresh each time: it is what a failing run rolls back to. Rolling a
//     re-run back to the first run's state would leave the files and the
//     running service apart.
type manifest struct {
	ChangeSet string       `json:"changeSet"`
	Created   time.Time    `json:"created"`
	Tool      string       `json:"tool"`
	Files     []preFile    `json:"files"`
	Service   *preService  `json:"service,omitempty"`
	Firewall  *preFirewall `json:"firewall,omitempty"`
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

// preimage is a manifest and the folder its file copies are in.
type preimage struct {
	dir string
	m   manifest
}

func provisionDir(dataDir string) string { return filepath.Join(dataDir, "provision") }

func preimageDir(dataDir, changeSet string) string {
	return filepath.Join(provisionDir(dataDir), changeSet)
}

func runDir(dataDir, changeSet string) string {
	return filepath.Join(preimageDir(dataDir, changeSet), "run")
}

// capture writes a pre-image into dir.
func capture(dataDir, dir, changeSet string, files []string, svc *preService, fw *preFirewall, now time.Time) error {
	m := manifest{ChangeSet: changeSet, Created: now.UTC(), Tool: buildinfo.Version, Service: svc, Firewall: fw}
	for _, rel := range dedupe(files) {
		b, err := os.ReadFile(filepath.Join(dataDir, rel))
		switch {
		case errors.Is(err, os.ErrNotExist):
			m.Files = append(m.Files, preFile{Path: rel})
		case err != nil:
			return fmt.Errorf("read %s for the pre-image: %w", rel, err)
		default:
			if err := atomicfile.Write(filepath.Join(dir, "files", rel), b, 0o600); err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			m.Files = append(m.Files, preFile{Path: rel, Present: true, SHA256: hex.EncodeToString(sum[:])})
		}
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	// The manifest goes last: a capture that stopped half way has none.
	return atomicfile.Write(filepath.Join(dir, "manifest.json"), out, 0o600)
}

// capturePreimage takes the change set's pre-image unless it has one, and
// always the run's. It reports whether it took the change set's.
func capturePreimage(dataDir, changeSet string, files []string, svc *preService, fw *preFirewall, now time.Time) (bool, error) {
	first := false
	dir := preimageDir(dataDir, changeSet)
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); errors.Is(err, os.ErrNotExist) {
		if err := os.RemoveAll(dir); err != nil { // a capture that stopped half way
			return false, err
		}
		if err := capture(dataDir, dir, changeSet, files, svc, fw, now); err != nil {
			return false, err
		}
		first = true
	} else if err != nil {
		return false, err
	}
	run := runDir(dataDir, changeSet)
	if err := os.RemoveAll(run); err != nil {
		return first, err
	}
	return first, capture(dataDir, run, changeSet, files, svc, fw, now)
}

func loadPreimage(dir string) (preimage, error) {
	p := preimage{dir: dir}
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(b, &p.m); err != nil {
		return p, fmt.Errorf("the pre-image manifest does not decode: %w", err)
	}
	return p, nil
}

// restoreFiles puts a pre-image's files back, and removes the ones that were
// not there. It reports whether anything changed.
func restoreFiles(dataDir string, p preimage) (bool, error) {
	changed := false
	for _, f := range p.m.Files {
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
		b, err := os.ReadFile(filepath.Join(p.dir, "files", f.Path))
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

// prunePreimages keeps the keepPreimages newest change sets' pre-images (and
// always current's) and removes the rest.
func prunePreimages(dataDir, current string) error {
	all, err := listPreimages(dataDir)
	if err != nil {
		return err
	}
	kept := 0
	for _, name := range all {
		if name == current {
			continue
		}
		if kept < keepPreimages-1 {
			kept++
			continue
		}
		if err := os.RemoveAll(preimageDir(dataDir, name)); err != nil {
			return err
		}
	}
	return nil
}

// listPreimages lists the change sets with a pre-image folder, newest first;
// a folder whose manifest cannot be read sorts by its own time.
func listPreimages(dataDir string) ([]string, error) {
	entries, err := os.ReadDir(provisionDir(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	type dated struct {
		name string
		at   time.Time
	}
	var all []dated
	for _, e := range entries {
		if !e.IsDir() || !changeSetRe.MatchString(e.Name()) {
			continue
		}
		at := time.Time{}
		if p, err := loadPreimage(preimageDir(dataDir, e.Name())); err == nil {
			at = p.m.Created
		} else if info, ierr := e.Info(); ierr == nil {
			at = info.ModTime()
		}
		all = append(all, dated{e.Name(), at})
	}
	slices.SortFunc(all, func(a, b dated) int { return b.at.Compare(a.at) })
	out := make([]string, 0, len(all))
	for _, d := range all {
		out = append(out, d.name)
	}
	return out, nil
}

// preimages lists the change sets Restore can put back, newest first.
func preimages(dataDir string) []string {
	all, _ := listPreimages(dataDir)
	var out []string
	for _, name := range all {
		if _, err := loadPreimage(preimageDir(dataDir, name)); err == nil {
			out = append(out, name)
		}
	}
	return out
}

func dedupe(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}
