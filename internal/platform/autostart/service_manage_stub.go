//go:build !windows

package autostart

import (
	"errors"
	"time"
)

var ErrWindowsServiceUnsupported = errors.New("windows service control is not supported on this OS")

func EnsureWindowsServiceAutoStart(_ string, _ string) (bool, error) {
	return false, ErrWindowsServiceUnsupported
}

func StartWindowsService(_ string) error {
	return ErrWindowsServiceUnsupported
}

func StopWindowsService(_ string, _ time.Duration) error {
	return ErrWindowsServiceUnsupported
}
