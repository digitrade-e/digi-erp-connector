package link

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// An installer (plan 090): its key, certified by the test team key.
type testInstaller struct {
	id   string
	priv ed25519.PrivateKey
	cert linkproto.Signed
}

func newInstaller(t *testing.T, teamPriv ed25519.PrivateKey, teamKeyID, id string, life time.Duration) testInstaller {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now()
	iat := now
	if life < 0 { // an expired certificate: born a day ago, dead since -life
		iat = now.Add(-24 * time.Hour)
	}
	cert, err := linkproto.SignInstallerCert(teamPriv, teamKeyID, linkproto.InstallerCert{ID: id, Name: strings.TrimPrefix(id, "inst-"),
		Pub: base64.StdEncoding.EncodeToString(pub), IssuedAt: iat.Unix(), ExpiresAt: now.Add(life).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return testInstaller{id: id, priv: priv, cert: cert}
}

func (in testInstaller) envelope(box, command string, args []string, stdin []byte, sealed bool) ([]byte, linkproto.Envelope) {
	now := time.Now()
	e := linkproto.Envelope{Box: box, Command: command, Args: args, StdinSHA256: linkproto.SHA256Hex(stdin), Sealed: sealed,
		Nonce: linkproto.RandomNonce(16), IssuedAt: now.Unix(), ExpiresAt: now.Add(4 * time.Minute).Unix(), By: "someone-else"}
	s, err := linkproto.SignInstallerEnvelope(in.priv, in.cert, e)
	if err != nil {
		panic(err)
	}
	raw, _ := json.Marshal(s)
	return raw, e
}

// installerPairedBox is pairedBox for a code an installer made (madeBy; "" = the team), on a member server whose role
// Windows reported unless tune says otherwise.
func installerPairedBox(t *testing.T, r *fakeRelay, id, madeBy string, kr linkproto.Keyring, tune func(*Config)) (*box, *relayConn) {
	t.Helper()
	n, _ := linkproto.NormalizePairCode("ABCDE-FGHJK")
	r.mu.Lock()
	r.madeBy[n] = madeBy
	r.mu.Unlock()
	return pairedBox(t, r, id, kr, "127.0.0.1:1", func(c *Config) {
		c.Host, c.Domain, c.DomainRole, c.DomainRoleKnown = "ERP01", "example.local", 3, true
		if tune != nil {
			tune(c)
		}
	})
}

func refusalOf(sa linkproto.SignedAnswer) string { return answerOf(sa).Refusal }

func TestRedemptionBindsTheInstallerWhoMadeTheCode(t *testing.T) {
	_, kr := team(t, 1)
	for _, c := range []struct{ name, madeBy, want, log string }{
		{"an installer's code", "inst-dana", "inst-dana", "code made by installer inst-dana"},
		{"the team's code", "", "", "code made by the team"},
		{"an id of the wrong shape", "inst-../x", "", "an installer id of the wrong shape"},
	} {
		r := newRelay(t)
		b, _ := installerPairedBox(t, r, "acme-erp", c.madeBy, kr, nil)
		got, err := b.store.Installer()
		if err != nil || got != c.want {
			t.Errorf("%s: bound to %q (%v), want %q", c.name, got, err, c.want)
		}
		if raw, err := b.store.Secrets.Get(secretInstaller); err != nil || len(raw) == 0 {
			t.Errorf("%s: no binding written (%v): a box must record the team too, so an older binding cannot survive", c.name, err)
		}
		if !b.log.has(c.log) {
			t.Errorf("%s: the log does not say %q", c.name, c.log)
		}
	}
}

func TestARedemptionReplacesAnOlderBinding(t *testing.T) {
	store := &Store{Dir: t.TempDir(), Secrets: &memSecrets{}}
	if err := store.BindInstaller("inst-dana"); err != nil {
		t.Fatal(err)
	}
	if err := store.BindInstaller(""); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Installer(); err != nil || got != "" {
		t.Fatalf("after a team pairing the box is still bound to %q (%v)", got, err)
	}
	if err := store.BindInstaller("not an id"); err == nil {
		t.Fatal("a malformed id was bound")
	}
	if err := store.Secrets.Set(secretInstaller, []byte("garbage")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Installer(); err == nil {
		t.Fatal("an unparseable binding read as none")
	}
}

func TestAnInstallerReadsItsOwnBox(t *testing.T) {
	r := newRelay(t)
	teamPriv, kr := team(t, 1)
	dana := newInstaller(t, teamPriv, "team-1", "inst-dana", 30*24*time.Hour)
	b, rc := installerPairedBox(t, r, "acme-erp", "inst-dana", kr, nil)
	for _, c := range []struct {
		command string
		stdin   []byte
		argv    string // provision's argv; "" for one the link answers itself
	}{
		{"state", []byte(`{}`), "state --spec -"},
		{"discover", nil, "discover --spec -"},
		{"verify", []byte(" { } "), "verify --spec -"},
		{"version", nil, "version"},
		{CommandHashes, nil, ""},
	} {
		before := b.runner.count()
		raw, e := dana.envelope("acme-erp", c.command, nil, c.stdin, false)
		stdout, sa := send(t, rc, raw, c.stdin)
		a, err := linkproto.VerifyAnswer(b.keys.Public(), sa, "acme-erp", e.Nonce, digestOf(raw), stdout)
		if err != nil || a.Refusal != "" {
			t.Errorf("%s: answer %+v, %v", c.command, a, err)
			continue
		}
		if c.argv == "" {
			if b.runner.count() != before {
				t.Errorf("%s ran provision", c.command)
			}
			continue
		}
		if b.runner.count() != before+1 || strings.Join(b.runner.calls[before], " ") != c.argv {
			t.Errorf("%s: provision ran %v", c.command, b.runner.calls[before:])
		}
	}
	if !b.log.has(`by "installer dana" (key inst-dana)`) {
		t.Error("the log does not name the installer from its certificate (the envelope's own by is not authenticated)")
	}
	if b.log.has("someone-else") {
		t.Error("the log took the envelope's unauthenticated by")
	}
}

func TestAnInstallerCannotWriteOrSendInput(t *testing.T) {
	r := newRelay(t)
	teamPriv, kr := team(t, 1)
	dana := newInstaller(t, teamPriv, "team-1", "inst-dana", 30*24*time.Hour)
	b, rc := installerPairedBox(t, r, "acme-erp", "inst-dana", kr, nil)
	sealedApply, _ := linkproto.Seal(b.keys.Public().Seal, []byte(`{"auth":{"password":"x"}}`))
	sealedDiscover, _ := linkproto.Seal(b.keys.Public().Seal, []byte(`{"db":{"user":"sa","password":"x"}}`))
	cases := []struct {
		name    string
		command string
		args    []string
		stdin   []byte
		sealed  bool
		want    string
	}{
		{"apply, sealed as it must be", "apply", nil, sealedApply, true, linkproto.CodeInstallerNotAllowed},
		{"counters", "counters", nil, []byte(`{}`), false, linkproto.CodeInstallerNotAllowed},
		{"options", "options", nil, []byte(`{}`), false, linkproto.CodeInstallerNotAllowed},
		{"restore", "restore", []string{"cs-20261118-0001"}, nil, false, linkproto.CodeInstallerNotAllowed},
		{"daemon-stop", "daemon-stop", nil, nil, false, linkproto.CodeInstallerNotAllowed},
		{"logs", "logs", nil, nil, false, linkproto.CodeInstallerNotAllowed},
		{"upgrade", "upgrade", nil, []byte(`{}`), false, linkproto.CodeInstallerNotAllowed},
		{"folder-acl", "folder-acl", nil, []byte(`{}`), false, linkproto.CodeInstallerNotAllowed},
		{"discover naming a host (no password)", "discover", nil, []byte(`{"db":{"host":"10.0.0.5","user":"sa"}}`), false, linkproto.CodeInstallerInput},
		{"discover with a sealed credential", "discover", nil, sealedDiscover, true, linkproto.CodeInstallerInput},
		{"state declared sealed with nothing in it", "state", nil, nil, true, linkproto.CodeInstallerInput},
		{"verify with expectations", "verify", nil, []byte(`{"expectVersion":"v1.0.40"}`), false, linkproto.CodeInstallerInput},
		{"discover with a credential in clear", "discover", nil, []byte(`{"db":{"user":"sa","password":"x"}}`), false, CodeInputNotSealed},
	}
	before := b.runner.count()
	for _, c := range cases {
		raw, e := dana.envelope("acme-erp", c.command, c.args, c.stdin, c.sealed)
		stdout, sa := send(t, rc, raw, c.stdin)
		a, err := linkproto.VerifyAnswer(b.keys.Public(), sa, "acme-erp", e.Nonce, digestOf(raw), stdout)
		if err != nil {
			t.Errorf("%s: the refusal is not bound to the envelope: %v", c.name, err)
			continue
		}
		if a.Refusal != c.want {
			t.Errorf("%s: refusal %q, want %q", c.name, a.Refusal, c.want)
		}
	}
	if n := b.runner.count(); n != before {
		t.Fatalf("provision ran %d time(s) for refused installer commands", n-before)
	}
}

func TestAnInstallerIsRefusedOnAnyOtherBox(t *testing.T) {
	teamPriv, kr := team(t, 1)
	dana := newInstaller(t, teamPriv, "team-1", "inst-dana", 30*24*time.Hour)
	for _, c := range []struct {
		name   string
		madeBy string
		tune   func(*Config)
		want   string
	}{
		{"a box another installer paired", "inst-omer", nil, linkproto.CodeInstallerNotBound},
		{"a box the team paired", "", nil, linkproto.CodeInstallerNotBound},
		{"its own box, a domain controller", "inst-dana", func(c *Config) { c.DomainRole = 5 }, linkproto.CodeInstallerDC},
		{"its own box, a backup domain controller", "inst-dana", func(c *Config) { c.DomainRole = 4 }, linkproto.CodeInstallerDC},
		{"its own box, the role unread (a member by guess)", "inst-dana", func(c *Config) { c.DomainRoleKnown = false }, linkproto.CodeInstallerRoleUnknown},
	} {
		r := newRelay(t)
		b, rc := installerPairedBox(t, r, "acme-erp", c.madeBy, kr, c.tune)
		raw, _ := dana.envelope("acme-erp", "state", nil, nil, false)
		_, sa := send(t, rc, raw, nil)
		if got := refusalOf(sa); got != c.want {
			t.Errorf("%s: refusal %q, want %q", c.name, got, c.want)
		}
		if b.runner.count() != 0 {
			t.Errorf("%s: provision ran", c.name)
		}
	}
}

// A box paired before this release has no binding at all: no installer counts there (plan 090: every box already in
// service when it upgrades to this build).
func TestABoxPairedBeforeInstallersRefusesThem(t *testing.T) {
	r := newRelay(t)
	teamPriv, kr := team(t, 1)
	dana := newInstaller(t, teamPriv, "team-1", "inst-dana", 30*24*time.Hour)
	b, rc := pairedBox(t, r, "acme-erp", kr, "127.0.0.1:1", func(c *Config) { c.DomainRoleKnown = true })
	ms := b.store.Secrets.(*memSecrets)
	ms.mu.Lock()
	delete(ms.m, secretInstaller) // as on a box paired before this release
	ms.mu.Unlock()
	raw, _ := dana.envelope("acme-erp", "state", nil, nil, false)
	_, sa := send(t, rc, raw, nil)
	if got := refusalOf(sa); got != linkproto.CodeInstallerNotBound {
		t.Fatalf("refusal %q, want %q", got, linkproto.CodeInstallerNotBound)
	}
}

func TestAnInstallerCertificateMustBeTheTeams(t *testing.T) {
	r := newRelay(t)
	teamPriv, kr := team(t, 1)
	strangerPriv, _ := team(t, 1)
	b, rc := installerPairedBox(t, r, "acme-erp", "inst-dana", kr, nil)
	for _, c := range []struct {
		name string
		in   testInstaller
		want string
	}{
		{"certified by a key this release does not trust", newInstaller(t, strangerPriv, "team-1", "inst-dana", time.Hour), linkproto.CodeBadSignature},
		{"certified under another key id", newInstaller(t, teamPriv, "team-9", "inst-dana", time.Hour), linkproto.CodeKeyUnknown},
		{"an expired certificate", newInstaller(t, teamPriv, "team-1", "inst-dana", -time.Hour), linkproto.CodeExpired},
	} {
		raw, _ := c.in.envelope("acme-erp", "state", nil, nil, false)
		_, sa := send(t, rc, raw, nil)
		a := answerOf(sa)
		if a.Refusal != c.want {
			t.Errorf("%s: refusal %q, want %q", c.name, a.Refusal, c.want)
		}
		if a.Nonce != "" || a.Env != "" {
			t.Errorf("%s: a refusal before verification names an envelope", c.name)
		}
	}
	if b.runner.count() != 0 {
		t.Fatal("provision ran")
	}
}

// The team key is not touched by any of this: on an installer's box it still runs what it always ran.
func TestTheTeamStillCommandsAnInstallersBox(t *testing.T) {
	r := newRelay(t)
	teamPriv, kr := team(t, 1)
	b, rc := installerPairedBox(t, r, "acme-erp", "inst-dana", kr, func(c *Config) { c.DomainRole = 5 })
	stdin := []byte(`{}`)
	raw, _ := envelope(teamPriv, "team-1", "acme-erp", "counters", nil, stdin, time.Now())
	_, sa := send(t, rc, raw, stdin)
	if got := refusalOf(sa); got != "" {
		t.Fatalf("the team's command refused: %q", got)
	}
	if b.runner.count() != 1 || strings.Join(b.runner.calls[0], " ") != "counters --spec -" {
		t.Fatalf("provision ran %v", b.runner.calls)
	}
}

func TestEmptyInput(t *testing.T) {
	for in, want := range map[string]bool{"": true, "  \n": true, "{}": true, " { \t } ": true,
		`{"db":{}}`: false, "[]": false, "null": false, "{} {}": false, "x": false} {
		if got := emptyInput([]byte(in)); got != want {
			t.Errorf("emptyInput(%q) = %v", in, got)
		}
	}
}
