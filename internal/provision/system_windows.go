//go:build windows

package provision

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/autostart"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/paths"
	"github.com/digitrade-e/digi-erp-connector/internal/secrets"
)

// winSystem is System on Windows: the service manager through autostart,
// DPAPI through secrets, the DACL through dataacl, the registry and the TCP
// table natively, and the firewall and host through PowerShell — fixed
// scripts whose only inputs are environment variables, never text spliced
// into the script.
type winSystem struct{}

// NewSystem is this machine.
func NewSystem() System { return winSystem{} }

func (winSystem) DataDir() string { return paths.DataDir() }

func (winSystem) ProtectDataDir() error {
	dir := paths.DataDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	_, err := dataacl.ProtectTree(dir, dataacl.TreeOptions{ClaimForAdministrators: true})
	return err
}

func (winSystem) SecretGet(key string) ([]byte, error)     { return secrets.Get(key) }
func (winSystem) SecretSet(key string, value []byte) error { return secrets.Set(key, value) }

func (winSystem) Service(name string) (autostart.ServiceInfo, error) {
	return autostart.WindowsServiceInfo(name)
}

func (winSystem) SetServiceDependencies(name string, deps []string) error {
	return autostart.SetWindowsServiceDependencies(name, deps)
}

func (winSystem) EnsureServiceRecovery(name string) (bool, error) {
	return autostart.EnsureWindowsServiceRecovery(name)
}

func (winSystem) StopService(name string) error {
	return autostart.StopWindowsService(name, autostart.ServiceStopWait)
}

func (winSystem) StartService(name string) error { return autostart.StartWindowsService(name) }

func (winSystem) FirewallProfiles() (FirewallProfiles, error) {
	out, err := powershell(`
$o = [ordered]@{}
foreach ($p in Get-NetFirewallProfile -PolicyStore ActiveStore) { $o[[string]$p.Name] = ([string]$p.Enabled -eq 'True') }
ConvertTo-Json -Compress -InputObject $o`, nil)
	if err != nil {
		return FirewallProfiles{}, err
	}
	var m map[string]bool
	if err := json.Unmarshal(out, &m); err != nil {
		return FirewallProfiles{}, fmt.Errorf("the firewall profiles do not decode: %w", err)
	}
	return FirewallProfiles{Domain: m["Domain"], Private: m["Private"], Public: m["Public"]}, nil
}

func (winSystem) FirewallRule(name string) (*FirewallRule, error) {
	out, err := powershell(`
$out = foreach ($r in @(Get-NetFirewallRule -DisplayName $env:DIGI_RULE -ErrorAction SilentlyContinue)) {
  $pf = $r | Get-NetFirewallPortFilter
  $af = $r | Get-NetFirewallAddressFilter
  [ordered]@{
    enabled = ([string]$r.Enabled -eq 'True'); direction = [string]$r.Direction; action = [string]$r.Action
    protocol = [string]$pf.Protocol
    localPort = @($pf.LocalPort | ForEach-Object { [string]$_ })
    remoteAddress = @($af.RemoteAddress | ForEach-Object { [string]$_ })
  }
}
ConvertTo-Json -Compress -Depth 4 -InputObject @($out)`, map[string]string{"DIGI_RULE": name})
	if err != nil {
		return nil, err
	}
	var rules []psRule
	if err := json.Unmarshal(out, &rules); err != nil {
		return nil, fmt.Errorf("the firewall rule does not decode: %w", err)
	}
	return ruleFromPS(name, rules), nil
}

func (winSystem) SetFirewallRule(rule FirewallRule) error {
	enabled := "0"
	if rule.Enabled {
		enabled = "1"
	}
	_, err := powershell(`
Remove-NetFirewallRule -DisplayName $env:DIGI_RULE -ErrorAction SilentlyContinue
$remote = @($env:DIGI_REMOTE -split ',' | Where-Object { $_ -ne '' })
if ($remote.Count -eq 0) { $remote = @('Any') }
$state = 'False'; if ($env:DIGI_ENABLED -eq '1') { $state = 'True' }
New-NetFirewallRule -DisplayName $env:DIGI_RULE -Direction Inbound -Action Allow -Protocol TCP -LocalPort ([int]$env:DIGI_PORT) -RemoteAddress $remote -Profile Any -Enabled $state | Out-Null
'{}'`, map[string]string{
		"DIGI_RULE":    rule.Name,
		"DIGI_PORT":    fmt.Sprint(rule.Port),
		"DIGI_REMOTE":  strings.Join(canonicalSources(rule.RemoteAddress), ","),
		"DIGI_ENABLED": enabled,
	})
	return err
}

func (winSystem) RemoveFirewallRule(name string) error {
	_, err := powershell(`
Remove-NetFirewallRule -DisplayName $env:DIGI_RULE -ErrorAction SilentlyContinue
'{}'`, map[string]string{"DIGI_RULE": name})
	return err
}

func (winSystem) Host() (HostInfo, error) {
	out, err := powershell(`
$cs = Get-CimInstance -ClassName Win32_ComputerSystem
$os = Get-CimInstance -ClassName Win32_OperatingSystem
ConvertTo-Json -Compress -InputObject ([ordered]@{
  name = [string]$cs.Name; domain = [string]$cs.Domain; partOfDomain = [bool]$cs.PartOfDomain
  domainRole = [int]$cs.DomainRole; os = [string]$os.Caption; osVersion = [string]$os.Version
  powershell = $PSVersionTable.PSVersion.ToString()
})`, nil)
	var h HostInfo
	if err == nil {
		err = json.Unmarshal(out, &h)
	}
	h.Elevated = windows.GetCurrentProcessToken().IsElevated()
	h.Addresses = hostAddresses()
	if h.Name == "" {
		h.Name, _ = os.Hostname()
	}
	return h, err
}

func hostAddresses() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, ipn.IP.String())
	}
	return out
}

func (winSystem) ImporterCandidates() ([]string, error) {
	const max = 20
	var out []string
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil, err
	}
	for i := 0; i < 26 && len(out) < max; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		p, err := windows.UTF16PtrFromString(root)
		if err != nil || windows.GetDriveType(p) != windows.DRIVE_FIXED {
			continue
		}
		for _, pattern := range []string{`Hash*\has.exe`, `Hash*\*\has.exe`, `Hash*\*.bat`} {
			matches, _ := filepath.Glob(root + pattern)
			for _, m := range matches {
				if len(out) < max {
					out = append(out, m)
				}
			}
		}
	}
	return out, nil
}

// powershell runs script — fixed text, sent encoded so nothing is quoted —
// with env as its only inputs, read in the script as $env:NAME. It returns
// stdout, which the script writes as one JSON value.
func powershell(script string, env map[string]string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	full := "$ErrorActionPreference = 'Stop'\n" +
		"[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)\n" + script
	u := utf16.Encode([]rune(full))
	b := make([]byte, 0, 2*len(u))
	for _, c := range u {
		b = append(b, byte(c), byte(c>>8))
	}
	exe := filepath.Join(os.Getenv("SystemRoot"), `System32\WindowsPowerShell\v1.0\powershell.exe`)
	cmd := exec.CommandContext(ctx, exe, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-EncodedCommand", base64.StdEncoding.EncodeToString(b))
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 400 {
			msg = msg[:400]
		}
		return nil, fmt.Errorf("powershell: %w: %s", err, msg)
	}
	out = []byte(strings.TrimSpace(string(out)))
	if len(out) == 0 {
		return nil, errors.New("powershell wrote nothing")
	}
	return out, nil
}
