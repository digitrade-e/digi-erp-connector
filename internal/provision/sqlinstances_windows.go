//go:build windows

package provision

import (
	"errors"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const sqlRoot = `SOFTWARE\Microsoft\Microsoft SQL Server`

// SQLInstances reads the instances from the registry — the same keys SQL
// Server Configuration Manager shows: Instance Names\SQL, then each one's
// Setup, MSSQLServer (LoginMode) and SuperSocketNetLib\Tcp (Enabled, and
// IPAll's TcpPort and TcpDynamicPorts). No port is scanned.
func (winSystem) SQLInstances() ([]SQLInstance, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, sqlRoot+`\Instance Names\SQL`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if errors.Is(err, registry.ErrNotExist) {
		return []SQLInstance{}, nil // no SQL Server on this machine
	}
	if err != nil {
		return nil, err
	}
	defer k.Close()
	names, err := k.ReadValueNames(0)
	if err != nil {
		return nil, err
	}
	out := make([]SQLInstance, 0, len(names))
	for _, name := range names {
		id, _, err := k.GetStringValue(name)
		if err != nil {
			continue
		}
		in := SQLInstance{Name: name, ID: id, Service: "MSSQL$" + name}
		if strings.EqualFold(name, "MSSQLSERVER") {
			in.Service = "MSSQLSERVER"
		}
		base := sqlRoot + `\` + id
		in.Edition = regString(base+`\Setup`, "Edition")
		in.Version = regString(base+`\Setup`, "Version")
		in.MixedAuth = regInt(base+`\MSSQLServer`, "LoginMode") == 2
		in.TCPEnabled = regInt(base+`\MSSQLServer\SuperSocketNetLib\Tcp`, "Enabled") == 1
		in.StaticPort = firstPort(regString(base+`\MSSQLServer\SuperSocketNetLib\Tcp\IPAll`, "TcpPort"))
		in.DynamicPort = firstPort(regString(base+`\MSSQLServer\SuperSocketNetLib\Tcp\IPAll`, "TcpDynamicPorts"))
		out = append(out, in)
	}
	return out, nil
}

func regString(path, value string) string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return ""
	}
	defer k.Close()
	s, _, err := k.GetStringValue(value)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

func regInt(path, value string) uint64 {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return 0
	}
	defer k.Close()
	n, _, err := k.GetIntegerValue(value)
	if err != nil {
		return 0
	}
	return n
}
