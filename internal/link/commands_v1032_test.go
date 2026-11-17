package link

import (
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// Connector v1.0.32's commands (plan 087 review R6): fixed argv, no
// argument taken; a reviewed change (TestTheFixedCommandSet counts them).
// Not sealed: the input carries no secret.
func TestV1032Commands(t *testing.T) {
	tests := []struct {
		command string
		args    []string
		argv    string // "" = refused (malformed)
	}{
		{"inflight-check", nil, "inflight --check --spec -"},
		{"inflight", nil, "inflight --spec -"},
		{"inflight", []string{"--check"}, ""},
		{"inflight", []string{"H-1"}, ""},
	}
	for _, tc := range tests {
		argv, err := Argv(linkproto.Envelope{Command: tc.command, Args: tc.args})
		switch {
		case tc.argv == "" && err == nil:
			t.Errorf("%s %v: ran %v, want refused", tc.command, tc.args, argv)
		case tc.argv != "" && (err != nil || strings.Join(argv, " ") != tc.argv):
			t.Errorf("%s %v = %v %v, want %q", tc.command, tc.args, argv, err, tc.argv)
		}
	}
	for name := range provisionCommandsV1032 {
		if MustBeSealed(name) {
			t.Errorf("%s must not require sealing: it carries no secret", name)
		}
	}
}
