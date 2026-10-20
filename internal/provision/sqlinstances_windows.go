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
// IPAll's TcpPort and TcpDynamicPorts). Both registry views: a 32-bit SQL
// Server (2014 and older) is under WOW6432Node. No port is scanned.
func (winSystem) SQLInstances() ([]SQLInstance, error) {
	var out []SQLInstance
	seen := map[string]bool{}
	for _, view := range []uint32{registry.WOW64_64KEY, registry.WOW64_32KEY} {
		found, err := sqlInstances(view)
		if err != nil {
			return nil, err
		}
		for _, in := range found {
			if !seen[strings.ToUpper(in.Name)] {
				seen[strings.ToUpper(in.Name)] = true
				out = append(out, in)
			}
		}
	}
	if out == nil {
		out = []SQLInstance{} // no SQL Server on this machine
	}
	return out, nil
}

func sqlInstances(view uint32) ([]SQLInstance, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, sqlRoot+`\Instance Names\SQL`, registry.QUERY_VALUE|view)
	if errors.Is(err, registry.ErrNotExist) {
		return nil, nil
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
		in.Edition = regString(view, base+`\Setup`, "Edition")
		in.Version = regString(view, base+`\Setup`, "Version")
		in.MixedAuth = regInt(view, base+`\MSSQLServer`, "LoginMode") == 2
		in.TCPEnabled = regInt(view, base+`\MSSQLServer\SuperSocketNetLib\Tcp`, "Enabled") == 1
		in.StaticPort = firstPort(regString(view, base+`\MSSQLServer\SuperSocketNetLib\Tcp\IPAll`, "TcpPort"))
		in.DynamicPort = firstPort(regString(view, base+`\MSSQLServer\SuperSocketNetLib\Tcp\IPAll`, "TcpDynamicPorts"))
		out = append(out, in)
	}
	return out, nil
}

func regString(view uint32, path, value string) string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE|view)
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

func regInt(view uint32, path, value string) uint64 {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE|view)
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
