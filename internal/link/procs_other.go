//go:build !windows

package link

import "errors"

// listProcesses: off Windows there is no connector window to close (the setup is a Windows one).
func listProcesses(string) ([]RunningProcess, error) { return nil, nil }

func killProcess(uint32) error { return errors.New("ending a process is done on Windows only") }
