//go:build windows

package autostart

import (
	"errors"
	"fmt"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const servicePollInterval = 300 * time.Millisecond

func EnsureWindowsServiceAutoStart(name, exePath string) (bool, error) {
	if name == "" {
		return false, errors.New("service name is required")
	}
	if exePath == "" {
		return false, errors.New("service executable path is required")
	}

	absPath, err := filepath.Abs(exePath)
	if err != nil {
		return false, err
	}

	m, err := mgr.Connect()
	if err != nil {
		return false, err
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err != nil {
		if !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return false, err
		}

		s, err = m.CreateService(name, absPath, mgr.Config{
			StartType:   mgr.StartAutomatic,
			DisplayName: name,
			Description: "ERP Connector REST API daemon",
		})
		if err != nil {
			return false, err
		}
		defer s.Close()
		return true, nil
	}
	defer s.Close()

	binaryPath, err := syscall.UTF16PtrFromString(syscall.EscapeArg(absPath))
	if err != nil {
		return false, err
	}
	if err := windows.ChangeServiceConfig(
		s.Handle,
		windows.SERVICE_NO_CHANGE,
		mgr.StartAutomatic,
		windows.SERVICE_NO_CHANGE,
		binaryPath,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	); err != nil {
		return false, err
	}

	return false, nil
}

func StartWindowsService(name string) error {
	if name == "" {
		return errors.New("service name is required")
	}

	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err != nil {
		return err
	}
	defer s.Close()

	status, err := s.Query()
	if err == nil {
		switch status.State {
		case svc.Running:
			return nil
		case svc.StartPending:
			return waitForServiceState(s, svc.Running, 30*time.Second)
		}
	}

	if err := s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return err
	}
	return waitForServiceState(s, svc.Running, 30*time.Second)
}

func StopWindowsService(name string, timeout time.Duration) error {
	if name == "" {
		return errors.New("service name is required")
	}
	if timeout <= 0 {
		timeout = 20 * time.Second
	}

	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err != nil {
		return err
	}
	defer s.Close()

	status, err := s.Query()
	if err == nil {
		switch status.State {
		case svc.Stopped:
			return nil
		case svc.StopPending:
			return waitForServiceState(s, svc.Stopped, timeout)
		}
	}

	if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
		return err
	}
	return waitForServiceState(s, svc.Stopped, timeout)
}

func waitForServiceState(s *mgr.Service, want svc.State, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		status, err := s.Query()
		if err != nil {
			return err
		}
		if status.State == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for service state %d (current %d)", want, status.State)
		}
		time.Sleep(servicePollInterval)
	}
}
