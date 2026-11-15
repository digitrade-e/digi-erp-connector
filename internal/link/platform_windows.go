//go:build windows

package link

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc/eventlog"
)

type winEvents struct{ l *eventlog.Log }

// openEventSink writes to the Application log under source; the installer
// registers the source, and without it Windows still records the text.
func openEventSink(source string) eventSink {
	l, err := eventlog.Open(source)
	if err != nil {
		return nil
	}
	return winEvents{l: l}
}

func (e winEvents) write(level, msg string) {
	switch level {
	case "ERROR":
		_ = e.l.Error(3, msg)
	case "WARN":
		_ = e.l.Warning(2, msg)
	default:
		_ = e.l.Info(1, msg)
	}
}

func (e winEvents) close() { _ = e.l.Close() }

// MachineFacts are what a human sees before confirming a pairing: the host,
// its DNS domain, and its role as Windows itself reports it — the same numbers
// as Win32_ComputerSystem.DomainRole, which discover reads: 2 a standalone
// server, 3 a member, 4 and 5 a domain controller. The NTDS service key is not
// a DC's sign: BFL's member server SQLSRV has one (2026-11-15).
func MachineFacts() (string, string, int) {
	host, _ := os.Hostname()
	domain := ""
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\Tcpip\Parameters`, registry.QUERY_VALUE); err == nil {
		domain, _, _ = k.GetStringValue("Domain")
		_ = k.Close()
	}
	if role, ok := dsRole(); ok {
		return host, domain, role
	}
	role := 2 // DsRole unavailable: a member when joined, never a guess of DC
	if domain != "" {
		role = 3
	}
	return host, domain, role
}

var (
	modNetapi32                 = windows.NewLazySystemDLL("netapi32.dll")
	procDsRoleGetPrimaryDomInfo = modNetapi32.NewProc("DsRoleGetPrimaryDomainInformation")
	procDsRoleFreeMemory        = modNetapi32.NewProc("DsRoleFreeMemory")
)

// DSROLE_PRIMARY_DOMAIN_INFO_BASIC's first field: DSROLE_MACHINE_ROLE, 0-5,
// the numbering Win32_ComputerSystem.DomainRole uses.
type dsrolePrimaryDomainInfoBasic struct {
	MachineRole uint32
}

func dsRole() (int, bool) {
	var info *dsrolePrimaryDomainInfoBasic
	const dsRolePrimaryDomainInfoBasic = 1
	r, _, _ := procDsRoleGetPrimaryDomInfo.Call(0, dsRolePrimaryDomainInfoBasic, uintptr(unsafe.Pointer(&info)))
	if r != 0 || info == nil {
		return 0, false
	}
	defer func() { _, _, _ = procDsRoleFreeMemory.Call(uintptr(unsafe.Pointer(info))) }()
	if info.MachineRole > 5 {
		return 0, false
	}
	return int(info.MachineRole), true
}

// PlatformPreflight adds the administrator and Windows version checks.
func PlatformPreflight(env *PreflightEnv) {
	env.Elevated = func() (bool, error) { return windows.GetCurrentProcessToken().IsElevated(), nil }
	env.OS = func() (string, bool) {
		v := windows.RtlGetVersion()
		// Windows Server 2016 is 10.0.14393; Go's runtime needs at least that.
		return fmt.Sprintf("Windows %d.%d.%d", v.MajorVersion, v.MinorVersion, v.BuildNumber),
			v.MajorVersion > 10 || (v.MajorVersion == 10 && v.BuildNumber >= 14393)
	}
}
