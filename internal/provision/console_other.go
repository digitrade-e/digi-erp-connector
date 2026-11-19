//go:build !windows

package provision

// newConsole off Windows: none. provision refuses every command there but version anyway.
func newConsole() Console { return nil }
