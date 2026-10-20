package provision

import (
	"fmt"
	"strings"
)

// psRule is a rule as Get-NetFirewallRule and its filters report it.
type psRule struct {
	Enabled       bool     `json:"enabled"`
	Direction     string   `json:"direction"`
	Action        string   `json:"action"`
	Protocol      string   `json:"protocol"`
	LocalPort     []string `json:"localPort"`
	RemoteAddress []string `json:"remoteAddress"`
}

// ruleFromPS reads the rules of one display name as FirewallRule: nil when
// there is none, and a rule that matches nothing (port 0) when it is not one
// inbound TCP allow on one port — SetFirewallRule then replaces it.
func ruleFromPS(name string, rules []psRule) *FirewallRule {
	if len(rules) == 0 {
		return nil
	}
	odd := &FirewallRule{Name: name}
	if len(rules) != 1 {
		return odd
	}
	r := rules[0]
	if !strings.EqualFold(r.Direction, "Inbound") || !strings.EqualFold(r.Action, "Allow") ||
		!strings.EqualFold(r.Protocol, "TCP") || len(r.LocalPort) != 1 {
		return odd
	}
	var port int
	if _, err := fmt.Sscanf(r.LocalPort[0], "%d", &port); err != nil {
		return odd
	}
	var remote []string
	for _, a := range r.RemoteAddress {
		if !strings.EqualFold(a, "Any") {
			remote = append(remote, a)
		}
	}
	return &FirewallRule{Name: name, Enabled: r.Enabled, Port: port, RemoteAddress: canonicalSources(remote)}
}
