package link

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// Plan 086 B1's commands: each one's argv, pinned here so adding one is a
// reviewed change (TestTheFixedCommandSet counts them).
func TestPlan086ProvisionCommands(t *testing.T) {
	tests := []struct {
		command string
		args    []string
		argv    string // "" = refused (malformed)
	}{
		{"auth-check", nil, "auth --check --spec -"},
		{"auth", nil, "auth --spec -"},
		{"auth", []string{"--check"}, ""},
		{"daemon-restart", nil, "daemon restart"},
		{"daemon-restart", []string{"--assume-idle"}, "daemon restart --assume-idle"},
		{"daemon-stop", nil, "daemon stop"},
		{"daemon-stop", []string{"--assume-idle"}, "daemon stop --assume-idle"},
		{"daemon-start", nil, "daemon start"},
		{"daemon-start", []string{"--assume-idle"}, "daemon start --assume-idle"},
		{"daemon-stop", []string{"--force"}, ""},
		{"daemon-stop", []string{"--assume-idle", "--assume-idle"}, ""},
		{"daemon-restart", []string{"stop"}, ""},
		{"service-check", nil, "service --check --spec -"},
		{"service", nil, "service --spec -"},
		{"service", []string{"MSSQL$X"}, ""},
		{"db-check", nil, "db --check --spec -"},
		{"db", nil, "db --spec -"},
		{"firewall-remove-check", nil, "firewall-remove --check --spec -"},
		{"firewall-remove", nil, "firewall-remove --spec -"},
		{"firewall-remove", []string{"digi-erp-connector"}, ""},
		{"receipt-seed", []string{"500000"}, "receipt-seed 500000"},
		{"receipt-seed", []string{"999999999"}, "receipt-seed 999999999"},
		{"receipt-seed", nil, ""},
		{"receipt-seed", []string{"0"}, ""},
		{"receipt-seed", []string{"0500"}, ""},
		{"receipt-seed", []string{"-1"}, ""},
		{"receipt-seed", []string{"1000000000"}, ""},
		{"receipt-seed", []string{"5 & calc"}, ""},
		{"receipt-seed", []string{"5", "6"}, ""},
		{"receipts-check", nil, "receipts --check --spec -"},
		{"receipts", nil, "receipts --spec -"},
		{"orders-check", nil, "orders --check --spec -"},
		{"orders", nil, "orders --spec -"},
	}
	for _, tc := range tests {
		argv, err := Argv(linkproto.Envelope{Command: tc.command, Args: tc.args})
		switch {
		case tc.argv == "" && err == nil:
			t.Errorf("%s %v: ran %v, want refused", tc.command, tc.args, argv)
		case tc.argv != "" && (err != nil || strings.Join(argv, " ") != tc.argv):
			t.Errorf("%s %v: %v %v, want %q", tc.command, tc.args, argv, err, tc.argv)
		}
	}
	for name := range provisionCommands086 {
		if _, ok := commands[name]; !ok {
			t.Errorf("%s is not in the command set", name)
		}
	}
	for name := range sealedOnly086 {
		if _, ok := provisionCommands086[name]; !ok {
			t.Errorf("sealedOnly086 names %s, which is not a plan 086 command", name)
		}
	}
}

// A command that carries the API password is refused unless its stdin is
// sealed to the box, and provision never runs; sealed, it runs with the
// opened stdin.
func TestASecretCarryingCommandMustBeSealed(t *testing.T) {
	r := newRelay(t)
	priv, kr := team(t, 1)
	b, rc := pairedBox(t, r, "bfl-read", kr, "127.0.0.1:1", nil)
	plain := []byte(`{"version":1,"changeSet":"cs-auth-0001","auth":{"password":"correct-horse-battery-staple"}}`)

	for _, cmd := range []string{"auth", "auth-check", "db", "db-check"} {
		raw, _ := envelope(priv, "team-1", "bfl-read", cmd, nil, plain, time.Now())
		stdout, sa := send(t, rc, raw, plain)
		if a := answerOf(sa); a.Refusal != CodeInputNotSealed || len(stdout) != 0 {
			t.Errorf("%s in clear: refusal %q, stdout %q; want %s", cmd, a.Refusal, stdout, CodeInputNotSealed)
		}
	}
	if b.runner.count() != 0 {
		t.Fatalf("provision ran for an unsealed secret: %v", b.runner.calls)
	}

	sealed, err := linkproto.Seal(b.keys.Public().Seal, plain)
	if err != nil {
		t.Fatal(err)
	}
	e := linkproto.Envelope{Box: "bfl-read", Command: "auth", StdinSHA256: linkproto.SHA256Hex(sealed), Sealed: true,
		Nonce: linkproto.RandomNonce(16), IssuedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(time.Minute).Unix()}
	s, _ := linkproto.SignEnvelope(priv, "team-1", e)
	raw, _ := json.Marshal(s)
	_, sa := send(t, rc, raw, sealed)
	if a := answerOf(sa); a.Refusal != "" {
		t.Fatalf("sealed auth refused: %+v", a)
	}
	if b.runner.count() != 1 || strings.Join(b.runner.calls[0], " ") != "auth --spec -" || !bytes.Equal(b.runner.stdin[0], plain) {
		t.Fatalf("provision ran %v with %q", b.runner.calls, b.runner.stdin)
	}
	if b.log.has("correct-horse-battery-staple") {
		t.Fatal("the password reached the log")
	}
}
