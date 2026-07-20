//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/autostart"
	"golang.org/x/sys/windows"
)

func uiStartupGuard() error {
	isService, err := autostart.IsWindowsService()
	if err == nil && isService {
		return errors.New("digi-erp-connector UI cannot run as a Windows Service or in a non-interactive session. Launch digi-erp-connector.exe from the desktop instead")
	}
	return nil
}

func uiStartupAlert(err error) {
	if err == nil {
		return
	}
	msg := err.Error()
	_, _ = windows.MessageBox(0, windows.StringToUTF16Ptr(msg), windows.StringToUTF16Ptr("ERP Connector"), windows.MB_ICONERROR)
	_, _ = fmt.Fprintln(os.Stderr, msg)
}
