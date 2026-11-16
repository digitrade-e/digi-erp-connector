package provision

import (
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
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
	// Validate refuses a loopback apiListen; behind the link it is the rule: the link reaches the API there, and
	// nothing listens on the network (review 2026-11-15: 0.0.0.0 plus a firewall rule was accepted).
	if !loopbackAddr(s.APIListen) {
		return refuse(CodeSpecInvalid, "set apiListen to 127.0.0.1:<port>: behind the link the API listens on loopback",
			"apiListen %s is not loopback: behind the link nothing listens on the network", s.APIListen)
	}
	if s.Firewall != nil {
		return refuse(CodeSpecInvalid, "leave firewall out: nothing listens on the network behind the link",
			"a firewall rule behind the link opens a port nobody needs")
	}
	// link mode's own rules first: they are stricter than Validate's, and name what is wrong precisely
	if r := validateImporterLinked(s.Importer, systemDirs); r != nil {
		return r
	}
	if s.Options != nil {
		if r := validateImageFoldersLinked(s.Options.ImageFolders, systemDirs, usersDirs()); r != nil {
			return r
		}
	}
	probe := s
	probe.APIListen = "0.0.0.0" + s.APIListen[strings.LastIndex(s.APIListen, ":"):]
	if r := probe.Validate(); r != nil {
		return r
	}
	// A login of the connector's own, never sa or any login the client gave as it is: whoever holds the connector's
	// credential can run SQL through custom_sql, and a sysadmin login of another name is sa by another name.
	if !s.DB.None() && (s.DB.AllowSa || !s.DB.LeastPrivilege) {
		return refuse(CodeSaLinkMode, "set leastPrivilege: true — behind the link the connector keeps a login of its own",
			"behind the link the connector never keeps the login the client gave (allowSa %v, leastPrivilege %v)", s.DB.AllowSa, s.DB.LeastPrivilege)
	}
	return nil
}

func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	ip := net.ParseIP(host)
	return err == nil && ip != nil && ip.IsLoopback()
}

// linkPathRefusal is why p is not a path link mode takes, or "": an absolute path on a local drive (X:\…, never
// UNC or \\?\), with no ".", ".." or empty part — os.Stat and the ACL check resolve ".." by text, so a path that
// climbs is approved for the file it ends in — and none of the characters cmd.exe reads (the importer's BAT runs as
// cmd.exe /C <path>, and Go does not escape &) (review 2026-11-15). The system-folder rules compare text, so no
// spelling Windows resolves to another folder either (plan 087 review m6): no "~" (an 8.3 short name, C:\PROGRA~1),
// no ":" after the drive letter (a stream, C:\Windows::$INDEX_ALLOCATION), no part ending in "." or " " (Windows
// drops it: C:\Windows.\System32 is C:\Windows\System32).
func linkPathRefusal(p string) string {
	if strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f || strings.ContainsRune(`&|<>^%!()"*?;,`, r) }) {
		return "it has a character cmd.exe reads"
	}
	if strings.Contains(p, "~") {
		return "it has a ~ (an 8.3 short name names a folder by another spelling): use the long name"
	}
	if winDrivePath.MatchString(p) && strings.Contains(p[2:], ":") {
		return "it has a : after the drive letter (a file stream)"
	}
	var parts []string
	switch {
	case runtime.GOOS == "windows" || winDrivePath.MatchString(p):
		if !winDrivePath.MatchString(p) {
			return "it is not a path on a local drive (X:\\...)"
		}
		parts = strings.Split(p[3:], `\`)
	default: // the tests' OS-native paths
		if !filepath.IsAbs(p) {
			return "it is not absolute"
		}
		parts = strings.Split(strings.TrimPrefix(p, "/"), "/")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.Contains(part, "/") {
			return "it has an empty, . or .. part"
		}
		if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return "a part of it ends in a dot or a space (Windows drops them, so it names another folder)"
		}
	}
	return ""
}

var winDrivePath = regexp.MustCompile(`^[A-Za-z]:\\[^\\]`)

func validateImporterLinked(imp *ImporterSpec, systemDirs []string) *Refusal {
	if imp == nil {
		return nil
	}
	if imp.AllowWritable {
		return refuse(CodeImporterNotERP, "leave allowWritable out: behind the link the importer is one only administrators can change",
			"allowWritable is refused behind the link: the importer runs as LocalSystem once per order")
	}
	switch imp.Mode {
	case "hasExe":
		if why := linkPathRefusal(imp.HasExePath); why != "" {
			return refuse(CodeImporterNotERP, `name Hasavshevet's has.exe by its plain path on a local drive`, "importer %q: %s", imp.HasExePath, why)
		}
		if !strings.EqualFold(winBase(imp.HasExePath), "has.exe") || winUnder(imp.HasExePath, systemDirs) {
			return refuse(CodeImporterNotERP, `name Hasavshevet's has.exe, outside the Windows and Program Files folders`,
				"importer %s is not Hasavshevet's has.exe outside the system folders: behind the link the importer may be nothing else", imp.HasExePath)
		}
	case "bat":
		if why := linkPathRefusal(imp.HasBatFile); why != "" {
			return refuse(CodeImporterNotERP, "name the .bat or .cmd by its plain path on a local drive", "importer %q: %s", imp.HasBatFile, why)
		}
		ext := strings.ToLower(winExt(imp.HasBatFile))
		if (ext != ".bat" && ext != ".cmd") || winUnder(imp.HasBatFile, systemDirs) {
			return refuse(CodeImporterNotERP, "name a .bat or .cmd outside the Windows and Program Files folders",
				"importer %s is not a batch file outside the system folders", imp.HasBatFile)
		}
	}
	return nil
}

// usersDirs are the profile roots: %SystemDrive%\Users and the profiles' parent the machine names.
func usersDirs() []string {
	var out []string
	if d := strings.TrimSpace(os.Getenv("SystemDrive")); d != "" {
		out = append(out, d+`\Users`)
	}
	if d := strings.TrimSpace(os.Getenv("PUBLIC")); d != "" {
		out = append(out, winParent(d))
	}
	return out
}

func validateImageFoldersLinked(folders *[]string, systemDirs, users []string) *Refusal {
	if folders == nil {
		return nil
	}
	for _, f := range *folders {
		if why := linkPathRefusal(f); why != "" {
			return refuse(CodeImageFolderForbidden, "name the folder that holds the product images by its plain path on a local drive",
				"image folder %q: %s", f, why)
		}
		// /api/file serves everything under a folder: never a drive root, a system folder or a profile, nor a folder
		// that holds one (C:\, C:\Users) — review 2026-11-15
		root := winDrivePath.MatchString(f) && len(winNorm(f)) <= 3
		holds := false
		for _, d := range append(append([]string{}, systemDirs...), users...) {
			holds = holds || winUnder(d, []string{f})
		}
		if root || holds || winUnder(f, systemDirs) || winUnder(f, users) || strings.Contains(winNorm(f)+`\`, `\appdata\`) {
			return refuse(CodeImageFolderForbidden, "name the folder that holds the product images, outside the system folders and the profiles",
				"image folder %s is, or holds, a system or profile folder: /api/file would serve it", f)
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

func winParent(p string) string {
	n := strings.TrimRight(strings.ReplaceAll(strings.TrimSpace(p), "/", `\`), `\`)
	if i := strings.LastIndex(n, `\`); i > 0 {
		return n[:i]
	}
	return n
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
