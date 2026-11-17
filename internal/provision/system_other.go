//go:build !windows

package provision

import (
	"errors"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/autostart"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/paths"
)

// errUnsupported: provisioning drives a Windows service, firewall and
// registry. Elsewhere it refuses before touching anything.
var errUnsupported = errors.New("provisioning runs on Windows only")

type otherSystem struct{}

// Unsupported marks a System that cannot provision: Run refuses at once.
func (otherSystem) Unsupported() error { return errUnsupported }

// NewSystem is this machine; off Windows every operation that needs the OS
// refuses.
func NewSystem() System { return otherSystem{} }

func (otherSystem) DataDir() string                               { return paths.DataDir() }
func (otherSystem) ProtectDataDir() error                         { return errUnsupported }
func (otherSystem) SecretGet(string) ([]byte, error)              { return nil, errUnsupported }
func (otherSystem) SecretSet(string, []byte) error                { return errUnsupported }
func (otherSystem) SetServiceDependencies(string, []string) error { return errUnsupported }
func (otherSystem) EnsureServiceRecovery(string) (bool, error)    { return false, errUnsupported }
func (otherSystem) StopService(string) error                      { return errUnsupported }
func (otherSystem) StartService(string) error                     { return errUnsupported }
func (otherSystem) FirewallProfiles() (FirewallProfiles, error) {
	return FirewallProfiles{}, errUnsupported
}
func (otherSystem) FirewallRule(string) (*FirewallRule, error)     { return nil, errUnsupported }
func (otherSystem) SetFirewallRule(FirewallRule) error             { return errUnsupported }
func (otherSystem) RemoveFirewallRule(string) error                { return errUnsupported }
func (otherSystem) Listeners() ([]Listener, error)                 { return nil, errUnsupported }
func (otherSystem) SQLInstances() ([]SQLInstance, error)           { return nil, errUnsupported }
func (otherSystem) Host() (HostInfo, error)                        { return HostInfo{}, errUnsupported }
func (otherSystem) ImporterCandidates() ([]string, error)          { return nil, errUnsupported }
func (otherSystem) Writers(string) ([]string, error)               { return nil, errUnsupported }
func (otherSystem) FolderWriters(string) ([]dataacl.Writer, error) { return nil, errUnsupported }
func (otherSystem) LookupAccount(string) (dataacl.Account, error) {
	return dataacl.Account{}, errUnsupported
}

func (otherSystem) Service(string) (autostart.ServiceInfo, error) {
	return autostart.ServiceInfo{}, errUnsupported
}

func (otherSystem) FolderRules() FolderRules                   { return machineFolderRules("") }
func (otherSystem) ReparsePointOn(path string) (string, error) { return reparseAlong(path) }
func (otherSystem) FinalPath(dir string) (string, error)       { return dataacl.FinalDirPath(dir) }
func (otherSystem) FinalFilePath(path string) (string, error)  { return dataacl.FinalFilePath(path) }

func (otherSystem) PlanFolderLockdown(string, []string, map[string]bool, bool) (dataacl.LockdownPlan, error) {
	return dataacl.LockdownPlan{}, errUnsupported
}
func (otherSystem) ApplyFolderLockdown(string, []string, dataacl.LockdownPlan) (int, error) {
	return 0, errUnsupported
}
func (otherSystem) RestoreFolderACL(string, []dataacl.FolderEntry) (int, int, error) {
	return 0, 0, errUnsupported
}
