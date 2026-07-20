//go:build !windows

package print

type PrinterInfo struct {
	Name       string
	PortName   string
	DriverName string
}

func EnumeratePrinters() ([]PrinterInfo, error) {
	return nil, nil
}

func FindPrinter(_ []PrinterInfo, _ string) *PrinterInfo {
	return nil
}

func IsServiceUnsafePort(_ string) bool {
	return false
}
