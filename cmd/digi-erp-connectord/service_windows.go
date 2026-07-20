//go:build windows

package main

import (
	"github.com/digitrade-e/digi-erp-connector/internal/logger"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/autostart"
)

func runAsService() bool {
	isService, err := autostart.IsWindowsService()
	if err != nil || !isService {
		return false
	}

	app := &serverApp{}
	if err := autostart.RunService(windowsServiceName, app); err != nil {
		logger.NewStderr().Error("windows service failed", err)
	}
	return true
}
