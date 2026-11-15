package provision

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func linkedDir(t *testing.T, content string) string {
	t.Helper()
	d := t.TempDir()
	if content != "" {
		if err := os.WriteFile(filepath.Join(d, "link.yaml"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func TestLinkModeIsWhatLinkYamlSays(t *testing.T) {
	for content, want := range map[string]bool{
		"":                              false,
		"relay: wss://x/link\n":         false, // installed, never paired
		"pairCode: ABCDE-FGHJK\n":       true,
		"box: bfl-read\n":               true,
		"box: bfl-read\nadmin: false\n": true,
	} {
		if got := LinkMode(linkedDir(t, content)); got != want {
			t.Errorf("link.yaml %q: LinkMode %v", content, got)
		}
	}
}

func TestLinkModeSpecRules(t *testing.T) {
	// OS-native absolute paths (the base validation checks absoluteness first), but not t.TempDir(): on Windows
	// that is under the user's AppData, which link mode refuses as an image folder. Nothing here is created.
	root := filepath.Join(filepath.VolumeName(os.TempDir())+string(filepath.Separator), "srv")
	win, pf, pd := filepath.Join(root, "Windows"), filepath.Join(root, "Program Files"), filepath.Join(root, "ProgramData")
	sysDirs := []string{win, pf, pd}
	hash, orders := filepath.Join(root, "Hash7"), filepath.Join(root, "digiorders")
	loop := func(s Spec) Spec { s.APIListen = "127.0.0.1:8082"; s.Firewall = nil; return s }
	base := func() Spec { return handInstallSpec(orders) }
	cases := []struct {
		name     string
		spec     Spec
		linked   bool
		wantCode string
	}{
		{"WinRM: loopback stays refused", loop(base()), false, CodeLoopbackListen},
		{"link: loopback is the point", loop(base()), true, ""},
		{"link: a firewall rule for loopback", func() Spec {
			s := loop(base())
			s.Firewall = &FirewallSpec{AllowFrom: []string{"198.51.100.241"}}
			return s
		}(), true, CodeSpecInvalid},
		{"WinRM: allowSa is the operator's call", func() Spec { s := base(); s.DB.LeastPrivilege, s.DB.AllowSa = false, true; return s }(), false, ""},
		{"link: allowSa is refused", func() Spec { s := loop(base()); s.DB.LeastPrivilege, s.DB.AllowSa = false, true; return s }(), true, CodeSaLinkMode},
		{"link: sa once for least privilege", loop(base()), true, ""},
		{"link: has.exe", withImporter(loop(base()), ImporterSpec{Mode: "hasExe", HasExePath: filepath.Join(hash, "has.exe")}), true, ""},
		{"link: cmd.exe as the importer", withImporter(loop(base()), ImporterSpec{Mode: "hasExe", HasExePath: filepath.Join(win, "System32", "cmd.exe")}), true, CodeImporterNotERP},
		{"link: another exe outside the system folders", withImporter(loop(base()), ImporterSpec{Mode: "hasExe", HasExePath: filepath.Join(hash, "powershell.exe")}), true, CodeImporterNotERP},
		{"link: has.exe under Program Files", withImporter(loop(base()), ImporterSpec{Mode: "hasExe", HasExePath: filepath.Join(pf, "x", "HAS.EXE")}), true, CodeImporterNotERP},
		{"link: a BAT", withImporter(loop(base()), ImporterSpec{Mode: "bat", HasBatFile: filepath.Join(hash, "importorders.bat")}), true, ""},
		{"link: a script that is not a BAT", withImporter(loop(base()), ImporterSpec{Mode: "bat", HasBatFile: filepath.Join(hash, "run.ps1")}), true, CodeImporterNotERP},
		{"link: a BAT in Windows", withImporter(loop(base()), ImporterSpec{Mode: "bat", HasBatFile: filepath.Join(win, "Temp", "x.cmd")}), true, CodeImporterNotERP},
		{"WinRM: cmd.exe is not this rule's", withImporter(base(), ImporterSpec{Mode: "hasExe", HasExePath: filepath.Join(win, "System32", "cmd.exe")}), false, ""},
		{"link: a Hebrew image folder", withFolders(loop(base()), filepath.Join(root, "תמונות -Digitrade")), true, ""},
		{"link: the registry hives as images", withFolders(loop(base()), filepath.Join(win, "System32", "config")), true, CodeImageFolderForbidden},
		{"link: a user's AppData", withFolders(loop(base()), filepath.Join(root, "Users", "bob", "AppData", "Roaming", "x")), true, CodeImageFolderForbidden},
		{"link: a temp folder under AppData", withFolders(loop(base()), t.TempDir()), true, wantOnWindows(CodeImageFolderForbidden)},
		{"link: the connector's own data", withFolders(loop(base()), filepath.Join(pd, "digi-erp-connector")), true, CodeImageFolderForbidden},
	}
	for _, c := range cases {
		r := c.spec.ValidateFor(c.linked, sysDirs)
		got := ""
		if r != nil {
			got = r.Code
		}
		if got != c.wantCode {
			t.Errorf("%s: %q, want %q (%v)", c.name, got, c.wantCode, r)
		}
	}
}

// wantOnWindows is code where t.TempDir() is under AppData (Windows), else no refusal.
func wantOnWindows(code string) string {
	if runtime.GOOS == "windows" {
		return code
	}
	return ""
}

func withImporter(s Spec, imp ImporterSpec) Spec { s.Importer = &imp; return s }

func withFolders(s Spec, folders ...string) Spec {
	s.Options = &OptionsSpec{ImageFolders: &folders}
	return s
}

// options on a linked box: a forbidden folder is refused before anything is read or written.
func TestOptionsBehindTheLinkRefuseASystemFolder(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	if err := os.WriteFile(filepath.Join(sys.DataDir(), "link.yaml"), []byte("box: bfl-read\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	win := filepath.Join(t.TempDir(), "Windows")
	hive := filepath.Join(win, "System32", "config")
	if err := os.MkdirAll(hive, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SystemRoot", win)
	in := optionsInput(OptionsSpec{ImageFolders: &[]string{hive}})
	_, err := ApplyOptions(context.Background(), testEnv(sys, fdb), in, false)
	var r *Refusal
	if !asRefusal(err, &r) || r.Code != CodeImageFolderForbidden {
		t.Fatalf("got %v", err)
	}
}

func asRefusal(err error, out **Refusal) bool {
	r, ok := err.(*Refusal)
	if ok {
		*out = r
	}
	return ok
}

// Behind the link a loopback apiListen is not a defect of the install.
func TestDiscoverBehindTheLinkDoesNotCallLoopbackADefect(t *testing.T) {
	sys, fdb, _ := handInstallBox(t) // its config listens on 127.0.0.1:8080
	if err := os.WriteFile(filepath.Join(sys.DataDir(), "link.yaml"), []byte("box: bfl-read\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := Discover(context.Background(), testEnv(sys, fdb), DiscoverInput{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range f.Defects {
		if d.Code == DefectLoopbackListen {
			t.Fatal("loopback_listen named on a box behind the link")
		}
	}
}

// apply's own entry applies link mode: a loopback apiListen gets past validation on a linked box.
func TestApplyCheckBehindTheLinkTakesALoopbackListen(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	s := handInstallSpec(orders)
	s.APIListen, s.Firewall = "127.0.0.1:8082", nil
	if _, err := ApplyCheck(context.Background(), testEnv(sys, fdb), s); err == nil {
		t.Fatal("without link.yaml a loopback apiListen passed")
	} else if r, ok := err.(*Refusal); !ok || r.Code != CodeLoopbackListen {
		t.Fatalf("without link.yaml: %v, want loopback_listen", err)
	}
	_ = os.WriteFile(filepath.Join(sys.DataDir(), "link.yaml"), []byte("box: bfl-read\n"), 0o600)
	s.DB.LeastPrivilege, s.DB.AllowSa = false, true
	_, err := ApplyCheck(context.Background(), testEnv(sys, fdb), s)
	if r, ok := err.(*Refusal); !ok || r.Code != CodeSaLinkMode {
		t.Fatalf("a linked apply with allowSa: %v, want sa_link_mode", err)
	}
}

// The security review's bypasses (2026-11-15), as Windows paths whatever OS the tests run on.
func TestLinkModeRefusesTheReviewsBypasses(t *testing.T) {
	t.Setenv("SystemDrive", "C:")
	sysDirs := []string{`C:\Windows`, `C:\Program Files`, `C:\Program Files (x86)`, `C:\ProgramData`}
	orders := filepath.Join(t.TempDir(), "orders")
	loop := func(s Spec) Spec { s.APIListen = "127.0.0.1:8082"; s.Firewall = nil; return s }
	base := func() Spec { return loop(handInstallSpec(orders)) }
	bat := func(p string) Spec { return withImporter(base(), ImporterSpec{Mode: "bat", HasBatFile: p}) }
	exe := func(p string) Spec { return withImporter(base(), ImporterSpec{Mode: "hasExe", HasExePath: p}) }
	for name, c := range map[string]struct {
		spec Spec
		code string
	}{
		"a command after & in the BAT path":    {bat(`C:\x&powershell -ec AAAA&\..\Windows\System32\winrm.cmd`), CodeImporterNotERP},
		"a Masofon BAT with & cmd &":           {bat(`C:\Hash7\x.bat&cmd&\..\x.bat`), CodeImporterNotERP},
		"a BAT path that climbs":               {bat(`C:\Hash7\..\Windows\System32\x.cmd`), CodeImporterNotERP},
		"has.exe on a share":                   {exe(`\\attacker\s\has.exe`), CodeImporterNotERP},
		"has.exe through the long-path prefix": {exe(`\\?\C:\Hash7\has.exe`), CodeImporterNotERP},
		"allowWritable": {withImporter(base(), ImporterSpec{Mode: "bat", HasBatFile: `C:\Hash7\importorders.bat`, AllowWritable: true}),
			CodeImporterNotERP},
		"the drive as the image folder":       {withFolders(base(), `C:\`), CodeImageFolderForbidden},
		"the profiles as the image folder":    {withFolders(base(), `C:\Users`), CodeImageFolderForbidden},
		"an administrator's desktop":          {withFolders(base(), `C:\Users\Administrator\Desktop`), CodeImageFolderForbidden},
		"a folder that climbs to ProgramData": {withFolders(base(), `C:\x\..\ProgramData\digi-erp-connector`), CodeImageFolderForbidden},
		"a sysadmin login of another name": {func() Spec {
			s := base()
			s.DB.User, s.DB.LeastPrivilege = "erp_admin", false
			return s
		}(), CodeSaLinkMode},
		"the API on every address":        {func() Spec { s := handInstallSpec(orders); s.Firewall = nil; return s }(), CodeSpecInvalid},
		"a firewall rule behind the link": {func() Spec { s := base(); s.Firewall = &FirewallSpec{AllowFrom: []string{"198.51.100.241"}}; return s }(), CodeSpecInvalid},
	} {
		r := c.spec.ValidateFor(true, sysDirs)
		if r == nil || r.Code != c.code {
			t.Errorf("%s: %v, want %s", name, r, c.code)
		}
	}
	// what link mode still takes — Windows paths are absolute only on Windows (CI's windows-latest runs these)
	if runtime.GOOS != "windows" {
		return
	}
	for name, spec := range map[string]Spec{
		"has.exe in its own folder": exe(`C:\Hash7\has.exe`),
		"a BAT with a space":        bat(`C:\Masofon BAT\importorders.bat`),
		"images on another drive":   withFolders(base(), `D:\Images`),
		"images in a Hebrew folder": withFolders(base(), `C:\תמונות -Digitrade`),
	} {
		if r := spec.ValidateFor(true, sysDirs); r != nil {
			t.Errorf("%s: %v", name, r)
		}
	}
}
