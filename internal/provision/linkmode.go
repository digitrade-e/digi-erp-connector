package provision

import (
	"net"
	"os"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/link"
)

// Link mode (Digitrade plan 083 §8 A16): a box that dials out to the relay
// through digi-erp-connector-link. Its API may listen on loopback (nothing
// needs to reach it from the network, so no firewall rule either), and what a
// signed command can make it do is bounded tighter than over WinRM, where the
// credential is an administrator's shell anyway:
//   - never sa as the connector's own login (allowSa refused): whoever holds
//     the connector's credential can register and run SQL through custom_sql;
//   - the order importer only Hasavshevet's has.exe or a .bat/.cmd, outside
//     the system folders (it runs as LocalSystem once per order);
//   - no image folder in the system folders or a user's AppData (/api/file
//     serves what is in them).
//
// A box is in link mode when link.yaml in the data directory names a box or a
// pairing code. The WinRM path (plan 068) is unchanged.

// LinkMode reports whether the box at dataDir is behind the link.
func LinkMode(dataDir string) bool {
	st, err := (&link.Store{Dir: dataDir}).LoadState()
	return err == nil && (st.Box != "" || st.PairCode != "")
}

// SystemDirs are the folders a box in link mode may not name: from this
// machine's environment.
func SystemDirs() []string {
	var out []string
	for _, v := range []string{"SystemRoot", "ProgramFiles", "ProgramFiles(x86)", "ProgramData"} {
		if d := strings.TrimSpace(os.Getenv(v)); d != "" {
			out = append(out, d)
		}
	}
	return out
}

// ValidateFor is Validate with link mode's rules when linked.
func (s Spec) ValidateFor(linked bool, systemDirs []string) *Refusal {
	if !linked {
		return s.Validate()
	}
	// Validate refuses a loopback apiListen; behind the link it is the point.
	probe := s
	if loopbackAddr(s.APIListen) {
		probe.APIListen = "0.0.0.0" + s.APIListen[strings.LastIndex(s.APIListen, ":"):]
	}
	if r := probe.Validate(); r != nil {
		return r
	}
	if s.DB.AllowSa {
		return refuse(CodeSaLinkMode, "set leastPrivilege: a box behind the link never keeps sa",
			"allowSa is refused behind the link: whoever holds the connector's credential could run SQL as sa")
	}
	if s.Firewall != nil && loopbackAddr(s.APIListen) {
		return refuse(CodeSpecInvalid, "leave firewall out: nothing listens on the network behind the link",
			"a firewall rule for a loopback apiListen opens nothing anyone needs")
	}
	if r := validateImporterLinked(s.Importer, systemDirs); r != nil {
		return r
	}
	if s.Options != nil {
		return validateImageFoldersLinked(s.Options.ImageFolders, systemDirs)
	}
	return nil
}

func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	ip := net.ParseIP(host)
	return err == nil && ip != nil && ip.IsLoopback()
}

func validateImporterLinked(imp *ImporterSpec, systemDirs []string) *Refusal {
	if imp == nil {
		return nil
	}
	switch imp.Mode {
	case "hasExe":
		if !strings.EqualFold(winBase(imp.HasExePath), "has.exe") || winUnder(imp.HasExePath, systemDirs) {
			return refuse(CodeImporterNotERP, `name Hasavshevet's has.exe, outside the Windows and Program Files folders`,
				"importer %s is not Hasavshevet's has.exe outside the system folders: behind the link the importer may be nothing else", imp.HasExePath)
		}
	case "bat":
		ext := strings.ToLower(winExt(imp.HasBatFile))
		if (ext != ".bat" && ext != ".cmd") || winUnder(imp.HasBatFile, systemDirs) {
			return refuse(CodeImporterNotERP, "name a .bat or .cmd outside the Windows and Program Files folders",
				"importer %s is not a batch file outside the system folders", imp.HasBatFile)
		}
	}
	return nil
}

func validateImageFoldersLinked(folders *[]string, systemDirs []string) *Refusal {
	if folders == nil {
		return nil
	}
	for _, f := range *folders {
		if winUnder(f, systemDirs) || strings.Contains(winNorm(f)+`\`, `\appdata\`) {
			return refuse(CodeImageFolderForbidden, "name the folder that holds the product images, outside the system folders and AppData",
				"image folder %s is a system or profile folder: /api/file would serve it", f)
		}
	}
	return nil
}

// Windows paths, read the same way whatever OS the tests run on.
func winNorm(p string) string {
	p = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(p), "/", `\`))
	return strings.TrimRight(p, `\`)
}

func winBase(p string) string {
	n := winNorm(p)
	return n[strings.LastIndex(n, `\`)+1:]
}

func winExt(p string) string {
	b := winBase(p)
	if i := strings.LastIndex(b, "."); i >= 0 {
		return b[i:]
	}
	return ""
}

func winUnder(p string, dirs []string) bool {
	n := winNorm(p)
	for _, d := range dirs {
		dn := winNorm(d)
		if dn != "" && (n == dn || strings.HasPrefix(n, dn+`\`)) {
			return true
		}
	}
	return false
}
