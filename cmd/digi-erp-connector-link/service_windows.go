//go:build windows

package main

import (
	"path/filepath"

	"github.com/digitrade-e/digi-erp-connector/internal/link"
	"github.com/digitrade-e/digi-erp-connector/internal/logger"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/autostart"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/paths"
)

func runAsService() bool {
	isService, err := autostart.IsWindowsService()
	if err != nil || !isService {
		return false
	}
	log, err := link.OpenFileLog(filepath.Join(paths.DataDir(), "link.log"))
	if err != nil {
		logger.NewStderr().Error("link: cannot open link.log", err)
		return true
	}
	app := &link.App{Machine: machine(log), Log: log}
	if err := autostart.RunService(link.ServiceName, app); err != nil {
		log.Error("link: windows service failed", err)
	}
	return true
}
