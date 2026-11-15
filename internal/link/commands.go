package link

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// The fixed command set (plan 083 §3.2, digi-mcp's L25): an envelope names one
// of these, and the link runs digi-erp-connector-provision.exe with exactly
// these arguments, its stdin from the envelope. There is no shell and no other
// program. A test pins the set.
var commands = map[string]func(args []string) ([]string, bool){
	"version":       fixed("version"),
	"discover":      fixed("discover", "--spec", "-"),
	"apply-check":   fixed("apply", "--check", "--spec", "-"),
	"apply":         fixed("apply", "--spec", "-"),
	"options-check": fixed("options", "--check", "--spec", "-"),
	"options":       fixed("options", "--spec", "-"),
	"verify":        fixed("verify", "--spec", "-"),
	"restore":       restore,
}

// CommandHashes is answered by the link itself, not provision.exe: the SHA-256
// of the installed executables, which digi-mcp checks against its pinned builds
// before anything else (L25 item 4).
const CommandHashes = "hashes"

var changeSetShape = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

func fixed(argv ...string) func([]string) ([]string, bool) {
	return func(args []string) ([]string, bool) {
		if len(args) != 0 {
			return nil, false
		}
		return append([]string(nil), argv...), true
	}
}

// restore takes a change-set id and, optionally, --assume-idle.
func restore(args []string) ([]string, bool) {
	switch {
	case len(args) == 1 && changeSetShape.MatchString(args[0]):
		return []string{"restore", args[0]}, true
	case len(args) == 2 && changeSetShape.MatchString(args[0]) && args[1] == "--assume-idle":
		return []string{"restore", args[0], "--assume-idle"}, true
	}
	return nil, false
}

// Argv maps an envelope's command and args to provision.exe's arguments, or
// refuses.
func Argv(e linkproto.Envelope) ([]string, error) {
	f, ok := commands[e.Command]
	if !ok {
		return nil, &linkproto.Refusal{Code: linkproto.CodeCommandUnknown}
	}
	argv, ok := f(e.Args)
	if !ok {
		return nil, &linkproto.Refusal{Code: linkproto.CodeMalformed}
	}
	return argv, nil
}

// Executables are the installed files whose digests "hashes" reports.
var Executables = []string{
	"digi-erp-connector-provision.exe",
	"digi-erp-connectord.exe",
	"digi-erp-connector.exe",
	"digi-erp-connector-link.exe",
}

// Hashes answers "hashes": {"files":[{"name","sha256"}]}, a missing file
// with an empty digest.
func Hashes(installDir string) []byte {
	type file struct {
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
	}
	var out struct {
		Files []file `json:"files"`
	}
	for _, name := range Executables {
		out.Files = append(out.Files, file{Name: name, SHA256: digestFile(filepath.Join(installDir, name))})
	}
	b, _ := json.Marshal(out)
	return b
}

func digestFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}
