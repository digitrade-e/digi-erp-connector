//go:build windows

package tcptable

import (
	"errors"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modIphlpapi             = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedTcpTable = modIphlpapi.NewProc("GetExtendedTcpTable")
)

const errInsufficientBuffer = 122 // ERROR_INSUFFICIENT_BUFFER

// Read reads one table (OwnerPIDListener or OwnerPIDConnections) of one family — no netstat to parse, nothing
// localised.
func Read(family, class uint32) ([]Row, error) {
	var size uint32
	var buf []byte
	for attempt := 0; attempt < 5; attempt++ {
		var p uintptr
		if len(buf) > 0 {
			p = uintptr(unsafe.Pointer(&buf[0]))
		}
		r, _, _ := procGetExtendedTcpTable.Call(p, uintptr(unsafe.Pointer(&size)), 0,
			uintptr(family), uintptr(class), 0)
		switch r {
		case 0:
			return Parse(buf, family)
		case errInsufficientBuffer:
			buf = make([]byte, size)
		default:
			return nil, syscall.Errno(r)
		}
	}
	return nil, errors.New("the TCP table kept growing")
}
