//go:build windows

package main

// Control of the digi-erp-connectord Windows service from the GUI, plus the
// discovery of the daemon binary the service should point at.
//
// The GUI must run elevated for these to succeed (the desktop shortcut goes
// through launch-admin.vbs); service errors are surfaced verbatim in the status
// label because the operator needs the reason.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/autostart"
)

const connectordWindowsServiceName = "digi-erp-connectord"

// onStartServer saves the current form, then creates/updates and starts the
// service. Saving first means the daemon reads exactly what is on screen.
func (f *mainForm) onStartServer() {
	if f.busy {
		return
	}
	cfg, password, err := f.readFormConfig()
	if err != nil {
		f.setStatus(err.Error())
		return
	}

	f.busy = true
	f.setStatus("Saving and starting server...")
	go func() {
		warning, err := persistConfig(cfg, password, f.logSvc)
		if err != nil {
			f.Synchronize(func() { f.finish(err.Error()) })
			return
		}
		if warning != "" {
			f.Synchronize(func() { f.setStatus(warning) })
		}

		daemonPath, err := findConnectordBinary()
		if err != nil {
			f.Synchronize(func() { f.finish("Start failed: " + err.Error()) })
			return
		}

		created, err := autostart.EnsureWindowsServiceAutoStart(connectordWindowsServiceName, daemonPath)
		if err != nil {
			f.Synchronize(func() { f.finish("Failed to create/update server service: " + err.Error()) })
			return
		}
		if err := autostart.StartWindowsService(connectordWindowsServiceName); err != nil {
			f.Synchronize(func() { f.finish("Failed to start server service: " + err.Error()) })
			return
		}

		msg := "Server service started."
		if created {
			msg = "Server service created and started."
		}
		f.Synchronize(func() {
			f.cfg = cfg
			f.finish(msg)
		})
	}()
}

func (f *mainForm) onStopServer() {
	if f.busy {
		return
	}
	f.busy = true
	f.setStatus(stoppingStatus())
	go func() {
		msg := stopServiceMessage(autostart.StopWindowsService(connectordWindowsServiceName, serviceStopTimeout))
		f.Synchronize(func() { f.finish(msg) })
	}()
}

// onRestartServer stops, then starts only if the stop completed (see
// restartService).
func (f *mainForm) onRestartServer() {
	if f.busy {
		return
	}
	f.busy = true
	f.setStatus(restartingStatus())
	go func() {
		msg := restartService(
			func() error { return autostart.StopWindowsService(connectordWindowsServiceName, serviceStopTimeout) },
			func() error { return autostart.StartWindowsService(connectordWindowsServiceName) },
		)
		f.Synchronize(func() { f.finish(msg) })
	}()
}

// findConnectordBinary locates digi-erp-connectord next to the GUI, in the
// working directory, or on PATH. As a last resort it takes the newest
// digi-erp-connectord* match in those directories, which covers version-suffixed
// builds left behind by an upgrade.
func findConnectordBinary() (string, error) {
	const stem = "digi-erp-connectord"

	var searchDirs []string
	if exePath, err := os.Executable(); err == nil {
		searchDirs = append(searchDirs, filepath.Dir(exePath))
	}
	if wd, err := os.Getwd(); err == nil {
		searchDirs = append(searchDirs, wd)
	}

	// Exact names first.
	for _, dir := range searchDirs {
		for _, name := range []string{stem, stem + ".exe"} {
			candidate := filepath.Join(dir, name)
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate, nil
			}
		}
	}

	for _, name := range []string{stem, stem + ".exe"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}

	// Fall back to the most recently modified suffixed build.
	for _, dir := range searchDirs {
		if dir == "" {
			continue
		}
		if best, ok := newestMatch(dir, stem); ok {
			return best, nil
		}
	}

	return "", fmt.Errorf("%s binary not found", stem)
}

// newestMatch returns the most recently modified file in dir matching stem*.
func newestMatch(dir, stem string) (string, bool) {
	matches, _ := filepath.Glob(filepath.Join(dir, stem+"*.exe"))
	if len(matches) == 0 {
		matches, _ = filepath.Glob(filepath.Join(dir, stem+"*"))
	}
	if len(matches) == 0 {
		return "", false
	}

	best := matches[0]
	var bestTime time.Time
	if info, err := os.Stat(best); err == nil {
		bestTime = info.ModTime()
	}
	for _, candidate := range matches[1:] {
		if info, err := os.Stat(candidate); err == nil && info.ModTime().After(bestTime) {
			best, bestTime = candidate, info.ModTime()
		}
	}
	return best, true
}
