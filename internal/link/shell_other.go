//go:build !windows

package link

// defaultShellExec: no shell off Windows (plan 094); the shell is refused not_supported.
var defaultShellExec ShellExecFunc
