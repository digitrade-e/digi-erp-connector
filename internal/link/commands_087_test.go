package link

import (
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// Plan 087's commands: each one's argv, pinned so adding one is a reviewed
// change (TestTheFixedCommandSet counts them). None is sealed-only.
func TestPlan087Commands(t *testing.T) {
	tests := []struct {
		command string
		args    []string
		argv    string // "" = refused (malformed)
	}{
		{"counters-check", nil, "counters --check --spec -"},
		{"counters", nil, "counters --spec -"},
		{"counters", []string{"--check"}, ""},
		{"counters", []string{"start-from"}, ""},
		{"documents-check", nil, "documents --check --spec -"},
		{"documents", nil, "documents --spec -"},
		{"documents", []string{`C:\herp`}, ""},
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
	for name := range provisionCommands087 {
		if MustBeSealed(name) {
			t.Errorf("%s must not require sealing: it carries no secret", name)
		}
	}
	if len(provisionCommands087) != 4 {
		t.Errorf("plan 087 has %d commands", len(provisionCommands087))
	}
}
