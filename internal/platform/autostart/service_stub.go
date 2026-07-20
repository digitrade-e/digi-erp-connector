//go:build !windows

package autostart

import "errors"

func IsWindowsService() (bool, error) {
	return false, nil
}

func RunService(_ string, _ ServiceApp) error {
	return errors.New("windows services are not supported on this OS")
}
