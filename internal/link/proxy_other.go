//go:build !windows

package link

// machineProxy off Windows: no machine setting beyond HTTPS_PROXY (development only).
func machineProxy(string) (ProxyChoice, error) { return ProxyChoice{Source: "no proxy configured"}, nil }

// UserProxy is the logged-in user's own proxy setting; none off Windows.
func UserProxy() string { return "" }
