//go:build windows

package autostart

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// WindowsServiceInfo reads the service: state, start type, account,
// dependencies, its process and whether recovery is the plan. A service that
// does not exist is Installed: false, not an error.
func WindowsServiceInfo(name string) (ServiceInfo, error) {
	if name == "" {
		return ServiceInfo{}, errors.New("service name is required")
	}
	m, err := mgr.Connect()
	if err != nil {
		return ServiceInfo{}, err
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return ServiceInfo{}, nil
		}
		return ServiceInfo{}, err
	}
	defer s.Close()

	cfg, err := s.Config()
	if err != nil {
		return ServiceInfo{}, fmt.Errorf("read the service config: %w", err)
	}
	st, err := s.Query()
	if err != nil {
		return ServiceInfo{}, fmt.Errorf("query the service: %w", err)
	}
	info := ServiceInfo{
		Installed:    true,
		State:        stateName(st.State),
		StartType:    startTypeName(cfg.StartType),
		Account:      cfg.ServiceStartName,
		Dependencies: cfg.Dependencies,
		ProcessID:    st.ProcessId,
	}
	if st.ProcessId != 0 {
		// Best effort: the process may have just exited.
		info.ProcessStart, _ = processStart(st.ProcessId)
	}
	info.RecoveryPlanned, err = recoveryIsPlanned(s)
	if err != nil {
		return ServiceInfo{}, fmt.Errorf("read the recovery actions: %w", err)
	}
	return info, nil
}

// SetWindowsServiceDependencies replaces the services name depends on (an
// empty list clears them). Only the dependencies change: mgr's UpdateConfig
// would also rewrite the account's password and cannot clear the list.
func SetWindowsServiceDependencies(name string, deps []string) error {
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

	block, err := dependencyBlock(deps)
	if err != nil {
		return err
	}
	return windows.ChangeServiceConfig(s.Handle,
		windows.SERVICE_NO_CHANGE, windows.SERVICE_NO_CHANGE, windows.SERVICE_NO_CHANGE,
		nil, nil, nil, block, nil, nil, nil)
}

// EnsureWindowsServiceRecovery gives the service the planned failure actions
// (recoveryActions, on non-crash failures too) and reports whether it had to.
// Unlike the installer's best-effort setRecovery, a failure is an error.
func EnsureWindowsServiceRecovery(name string) (changed bool, err error) {
	if name == "" {
		return false, errors.New("service name is required")
	}
	m, err := mgr.Connect()
	if err != nil {
		return false, err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return false, err
	}
	defer s.Close()

	planned, err := recoveryIsPlanned(s)
	if err != nil || planned {
		return false, err
	}
	var actions []mgr.RecoveryAction
	for _, a := range recoveryActions() {
		t := mgr.NoAction
		if a.restart {
			t = mgr.ServiceRestart
		}
		actions = append(actions, mgr.RecoveryAction{Type: t, Delay: a.delay})
	}
	if err := s.SetRecoveryActions(actions, uint32(recoveryResetPeriod.Seconds())); err != nil {
		return false, err
	}
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return false, err
	}
	return true, nil
}

func recoveryIsPlanned(s *mgr.Service) (bool, error) {
	acts, err := s.RecoveryActions()
	if err != nil {
		return false, err
	}
	reset, err := s.ResetPeriod()
	if err != nil {
		return false, err
	}
	nonCrash, err := s.RecoveryActionsOnNonCrashFailures()
	if err != nil {
		return false, err
	}
	got := make([]recoveryAction, 0, len(acts))
	for _, a := range acts {
		got = append(got, recoveryAction{restart: a.Type == mgr.ServiceRestart, delay: a.Delay})
	}
	return recoveryMatches(got, time.Duration(reset)*time.Second, nonCrash), nil
}

// dependencyBlock is lpDependencies: each name NUL-terminated, the block
// NUL-terminated again. An empty list is the empty block, which clears them
// (a nil pointer would mean "no change").
func dependencyBlock(deps []string) (*uint16, error) {
	var u []uint16
	for _, d := range deps {
		if d == "" {
			continue
		}
		s, err := windows.UTF16FromString(d) // NUL-terminated
		if err != nil {
			return nil, err
		}
		u = append(u, s...)
	}
	if len(u) == 0 {
		u = append(u, 0)
	}
	u = append(u, 0)
	return &u[0], nil
}

func processStart(pid uint32) (time.Time, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return time.Time{}, err
	}
	defer windows.CloseHandle(h)
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return time.Time{}, err
	}
	return time.Unix(0, creation.Nanoseconds()), nil
}

func stateName(s svc.State) string {
	switch s {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "start_pending"
	case svc.StopPending:
		return "stop_pending"
	case svc.Running:
		return "running"
	case svc.ContinuePending:
		return "continue_pending"
	case svc.PausePending:
		return "pause_pending"
	case svc.Paused:
		return "paused"
	}
	return fmt.Sprintf("state_%d", s)
}

func startTypeName(t uint32) string {
	switch t {
	case mgr.StartAutomatic:
		return "auto"
	case mgr.StartManual:
		return "manual"
	case mgr.StartDisabled:
		return "disabled"
	case windows.SERVICE_BOOT_START:
		return "boot"
	case windows.SERVICE_SYSTEM_START:
		return "system"
	}
	return fmt.Sprintf("start_%d", t)
}
