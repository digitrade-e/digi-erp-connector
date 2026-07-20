//go:build windows

package print

import (
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// PrinterInfo is a minimal projection of the Win32 PRINTER_INFO_2 struct.
type PrinterInfo struct {
	Name       string
	PortName   string
	DriverName string
}

const (
	printerEnumLocal       = 0x00000002
	printerEnumConnections = 0x00000004
)

// printerInfo2W mirrors PRINTER_INFO_2W. Layout must match exactly — the
// EnumPrintersW buffer is reinterpreted as []printerInfo2W.
type printerInfo2W struct {
	pServerName         *uint16
	pPrinterName        *uint16
	pShareName          *uint16
	pPortName           *uint16
	pDriverName         *uint16
	pComment            *uint16
	pLocation           *uint16
	pDevMode            uintptr
	pSepFile            *uint16
	pPrintProcessor     *uint16
	pDatatype           *uint16
	pParameters         *uint16
	pSecurityDescriptor uintptr
	Attributes          uint32
	Priority            uint32
	DefaultPriority     uint32
	StartTime           uint32
	UntilTime           uint32
	Status              uint32
	CJobs               uint32
	AveragePPM          uint32
}

var (
	modWinspool            = windows.NewLazySystemDLL("winspool.drv")
	procEnumPrintersW      = modWinspool.NewProc("EnumPrintersW")
	procGetDefaultPrinterW = modWinspool.NewProc("GetDefaultPrinterW")
)

// EnumeratePrinters returns the printers visible to the calling process —
// which, for a Windows service running as LocalSystem, is *only* machine-wide
// printers. Per-user printers from the interactive user's session are not
// included.
func EnumeratePrinters() ([]PrinterInfo, error) {
	flags := uint32(printerEnumLocal | printerEnumConnections)
	var needed, returned uint32

	r1, _, e := procEnumPrintersW.Call(
		uintptr(flags),
		0,
		2,
		0,
		0,
		uintptr(unsafe.Pointer(&needed)),
		uintptr(unsafe.Pointer(&returned)),
	)
	if r1 == 0 && e != windows.ERROR_INSUFFICIENT_BUFFER {
		return nil, fmt.Errorf("EnumPrintersW size probe: %w", e)
	}
	if needed == 0 {
		return nil, nil
	}

	buf := make([]byte, needed)
	r1, _, e = procEnumPrintersW.Call(
		uintptr(flags),
		0,
		2,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(needed),
		uintptr(unsafe.Pointer(&needed)),
		uintptr(unsafe.Pointer(&returned)),
	)
	if r1 == 0 {
		return nil, fmt.Errorf("EnumPrintersW: %w", e)
	}

	out := make([]PrinterInfo, 0, returned)
	infos := unsafe.Slice((*printerInfo2W)(unsafe.Pointer(&buf[0])), returned)
	for i := range infos {
		out = append(out, PrinterInfo{
			Name:       windows.UTF16PtrToString(infos[i].pPrinterName),
			PortName:   windows.UTF16PtrToString(infos[i].pPortName),
			DriverName: windows.UTF16PtrToString(infos[i].pDriverName),
		})
	}
	return out, nil
}

// FindPrinter returns the first printer whose name matches name (case-insensitive)
// or nil if none match.
func FindPrinter(printers []PrinterInfo, name string) *PrinterInfo {
	if name == "" {
		return nil
	}
	for i := range printers {
		if strings.EqualFold(printers[i].Name, name) {
			return &printers[i]
		}
	}
	return nil
}

// IsServiceUnsafePort returns true for printer ports that don't work
// reliably from a service running as LocalSystem. WSD-* ports depend on
// the user-session Function Discovery / PNP-X services and silently fail
// to deliver jobs from a non-interactive context. Operators should use a
// Standard TCP/IP Port equivalent of the same physical printer instead.
func IsServiceUnsafePort(port string) bool {
	return strings.HasPrefix(strings.ToUpper(port), "WSD-")
}

// defaultPrinterName returns the user/account-default printer via
// GetDefaultPrinterW. Empty string + error if no default is set.
func defaultPrinterName() (string, error) {
	var size uint32
	procGetDefaultPrinterW.Call(0, uintptr(unsafe.Pointer(&size)))
	if size == 0 {
		return "", fmt.Errorf("no default printer set for this account")
	}
	buf := make([]uint16, size)
	r, _, e := procGetDefaultPrinterW.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if r == 0 {
		return "", fmt.Errorf("GetDefaultPrinterW: %w", e)
	}
	return windows.UTF16ToString(buf), nil
}
