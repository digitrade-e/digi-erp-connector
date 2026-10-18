//go:build windows

package autostart

import (
	"errors"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

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
		setRecovery(s)
		return true, nil
	}
	defer s.Close()
	setRecovery(s)

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

// setRecovery makes the service manager restart the daemon after a failure
// (recoveryDelays), also on a non-crash failure (a non-zero exit). Best
// effort: the service works without it.
func setRecovery(s *mgr.Service) {
	actions := make([]mgr.RecoveryAction, 0, len(recoveryDelays))
	for _, d := range recoveryDelays {
		actions = append(actions, mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: d})
	}
	_ = s.SetRecoveryActions(actions, uint32(recoveryResetPeriod.Seconds()))
	_ = s.SetRecoveryActionsOnNonCrashFailures(true)
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
			return waitForStart(s)
		}
	}

	if err := s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return err
	}
	return waitForStart(s)
}

// waitForStart is waitForStartWith on the real service.
func waitForStart(s *mgr.Service) error {
	return waitForStartWith(func() (startStatus, error) {
		st, err := s.Query()
		if err != nil {
			return startStatus{}, err
		}
		out := startStatus{
			checkpoint: st.CheckPoint, waitHint: time.Duration(st.WaitHint) * time.Millisecond,
			win32: st.Win32ExitCode, specific: st.ServiceSpecificExitCode,
		}
		switch st.State {
		case svc.Running:
			out.state = startRunning
		case svc.Stopped:
			out.state = startStopped
		default:
			out.state = startPending
		}
		return out, nil
	}, time.Now, time.Sleep)
}

func StopWindowsService(name string, timeout time.Duration) error {
	if name == "" {
		return errors.New("service name is required")
	}
	if timeout <= 0 {
		timeout = ServiceStopWait
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
			return serviceStateTimeout(uint32(want), uint32(status.State))
		}
		time.Sleep(servicePollInterval)
	}
}
