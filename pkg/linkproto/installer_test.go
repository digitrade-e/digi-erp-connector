package linkproto

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

// installerWorld is a team key and one installer certified by it.
type installerWorld struct {
	teamPriv ed25519.PrivateKey
	kr       Keyring
	instPriv ed25519.PrivateKey
	cert     Signed
	now      time.Time
}

func newInstallerWorld(t *testing.T) installerWorld {
	t.Helper()
	teamPub, teamPriv, _ := ed25519.GenerateKey(rand.Reader)
	instPub, instPriv, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now()
	cert, err := SignInstallerCert(teamPriv, "team-1", InstallerCert{ID: "inst-dana", Name: "dana",
		Pub: base64.StdEncoding.EncodeToString(instPub), IssuedAt: now.Unix(), ExpiresAt: now.Add(30 * 24 * time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return installerWorld{teamPriv: teamPriv, kr: Keyring{{ID: "team-1", Epoch: 1, Pub: teamPub}}, instPriv: instPriv, cert: cert, now: now}
}

func (w installerWorld) envelope(box string) Envelope {
	return Envelope{Box: box, Command: "state", StdinSHA256: SHA256Hex(nil), Nonce: RandomNonce(16),
		IssuedAt: w.now.Unix(), ExpiresAt: w.now.Add(4 * time.Minute).Unix()}
}

func TestInstallerCert(t *testing.T) {
	w := newInstallerWorld(t)
	c, k, err := w.kr.VerifyInstallerCert(w.cert, w.now, 1)
	if err != nil || c.ID != "inst-dana" || c.Name != "dana" || k.ID != "team-1" {
		t.Fatalf("a good certificate: %+v %+v %v", c, k, err)
	}

	instPub := w.instPriv.Public().(ed25519.PublicKey)
	good := InstallerCert{ID: "inst-dana", Name: "dana", Pub: base64.StdEncoding.EncodeToString(instPub),
		IssuedAt: w.now.Unix(), ExpiresAt: w.now.Add(time.Hour).Unix()}
	// a certificate signed now must not be refused by its own signer for its shape, and one with a bad shape must be
	for name, c := range map[string]InstallerCert{
		"a team key's id":      {ID: "team-2", Name: "dana", Pub: good.Pub, IssuedAt: good.IssuedAt, ExpiresAt: good.ExpiresAt},
		"an e-mail as name":    {ID: "inst-dana", Name: "dana@example.com", Pub: good.Pub, IssuedAt: good.IssuedAt, ExpiresAt: good.ExpiresAt},
		"a short key":          {ID: "inst-dana", Name: "dana", Pub: "AAAA", IssuedAt: good.IssuedAt, ExpiresAt: good.ExpiresAt},
		"91 days":              {ID: "inst-dana", Name: "dana", Pub: good.Pub, IssuedAt: good.IssuedAt, ExpiresAt: w.now.Add(91 * 24 * time.Hour).Unix()},
		"expiring before born": {ID: "inst-dana", Name: "dana", Pub: good.Pub, IssuedAt: good.IssuedAt, ExpiresAt: good.IssuedAt},
	} {
		if _, err := SignInstallerCert(w.teamPriv, "team-1", c); err == nil {
			t.Errorf("%s: signed", name)
		}
	}

	resign := func(c InstallerCert) Signed {
		c.V = Version
		s, _ := sign(w.teamPriv, "team-1", domainInstallerCert, c) // past SignInstallerCert's own checks
		return s
	}
	tampered := w.cert
	tampered.Body = json.RawMessage(string(tampered.Body[:len(tampered.Body)-1]) + ` `)
	_, otherTeamPriv, _ := ed25519.GenerateKey(rand.Reader)
	byOtherTeam, _ := SignInstallerCert(otherTeamPriv, "team-1", good)
	asEnvelope, _ := sign(w.teamPriv, "team-1", domainEnvelope, good) // a team signature, but not over a certificate
	withCert := w.cert
	withCert.Cert = &w.cert
	cases := []struct {
		name string
		s    Signed
		now  time.Time
		min  int
		want string
	}{
		{"tampered", tampered, w.now, 1, CodeBadSignature},
		{"another key under the team key's id", byOtherTeam, w.now, 1, CodeBadSignature},
		{"signed under the envelope domain", asEnvelope, w.now, 1, CodeBadSignature},
		{"below the epoch floor", w.cert, w.now, 2, CodeKeyRevoked},
		{"expired", w.cert, w.now.Add(31 * 24 * time.Hour), 1, CodeExpired},
		{"from the future", w.cert, w.now.Add(-time.Hour), 1, CodeFromFuture},
		{"a certificate carrying a certificate", withCert, w.now, 1, CodeMalformed},
		{"91 days, signed past the checks", resign(InstallerCert{ID: "inst-dana", Name: "dana", Pub: good.Pub, IssuedAt: good.IssuedAt,
			ExpiresAt: w.now.Add(91 * 24 * time.Hour).Unix()}), w.now, 1, CodeLifetime},
		{"a bad id, signed past the checks", resign(InstallerCert{ID: "x", Name: "dana", Pub: good.Pub, IssuedAt: good.IssuedAt,
			ExpiresAt: good.ExpiresAt}), w.now, 1, CodeMalformed},
	}
	for _, c := range cases {
		if _, _, err := w.kr.VerifyInstallerCert(c.s, c.now, c.min); code(err) != c.want {
			t.Errorf("%s: %q, want %q", c.name, code(err), c.want)
		}
	}
}

func TestInstallerEnvelope(t *testing.T) {
	w := newInstallerWorld(t)
	s, err := SignInstallerEnvelope(w.instPriv, w.cert, w.envelope("acme-erp"))
	if err != nil {
		t.Fatal(err)
	}
	e, c, k, err := w.kr.VerifyInstallerEnvelope(s, w.now, "acme-erp", 1)
	if err != nil || e.Command != "state" || c.ID != "inst-dana" || k.ID != "team-1" || s.Key != "inst-dana" {
		t.Fatalf("a good installer envelope: %+v %+v %+v %v", e, c, k, err)
	}
	// It survives the wire as JSON, the way the box receives it.
	raw, _ := json.Marshal(s)
	var back Signed
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := w.kr.VerifyInstallerEnvelope(back, w.now, "acme-erp", 1); err != nil {
		t.Fatalf("after a JSON round trip: %v", err)
	}

	_, strangerPriv, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := SignInstallerEnvelope(strangerPriv, w.cert, w.envelope("acme-erp")); code(err) != CodeBadKey {
		t.Fatalf("signing with a key the certificate does not name: %q", code(err))
	}
	byStranger, _ := sign(strangerPriv, "inst-dana", domainInstallerEnvelope, w.envelope("acme-erp"))
	byStranger.Cert = &w.cert
	underTeamDomain, _ := sign(w.instPriv, "inst-dana", domainEnvelope, w.envelope("acme-erp"))
	underTeamDomain.Cert = &w.cert
	underAdminDomain, _ := sign(w.instPriv, "inst-dana", domainInstallerAdmin, w.envelope("acme-erp"))
	underAdminDomain.Cert = &w.cert
	otherID := s
	otherID.Key = "inst-other"
	noCert := s
	noCert.Cert = nil
	other, _ := SignInstallerEnvelope(w.instPriv, w.cert, w.envelope("other-box"))
	cases := []struct {
		name string
		s    Signed
		want string
	}{
		{"signed by a key the certificate does not name", byStranger, CodeBadSignature},
		{"an installer signature under the team envelope domain", underTeamDomain, CodeBadSignature},
		{"an installer signature under the installer admin domain", underAdminDomain, CodeBadSignature},
		{"a key id that is not the certificate's", otherID, CodeKeyUnknown},
		{"no certificate", noCert, CodeMalformed},
		{"another box's envelope", other, CodeWrongBox},
	}
	for _, c := range cases {
		if _, _, _, err := w.kr.VerifyInstallerEnvelope(c.s, w.now, "acme-erp", 1); code(err) != c.want {
			t.Errorf("%s: %q, want %q", c.name, code(err), c.want)
		}
	}
}

// TestInstallerSignaturesNeverPassAsTheTeams pins plan 090's domain separation: whatever an installer signs, no team
// check accepts it, and a team signature is no installer's.
func TestInstallerSignaturesNeverPassAsTheTeams(t *testing.T) {
	w := newInstallerWorld(t)
	env, _ := SignInstallerEnvelope(w.instPriv, w.cert, w.envelope("acme-erp"))
	if _, _, err := w.kr.VerifyEnvelope(env, w.now, "acme-erp", 1); code(err) != CodeMalformed {
		t.Errorf("an installer envelope as a team envelope: %q", code(err))
	}
	stripped := env
	stripped.Cert = nil
	if _, _, err := w.kr.VerifyEnvelope(stripped, w.now, "acme-erp", 1); code(err) != CodeKeyUnknown {
		t.Errorf("an installer envelope without its certificate as a team envelope: %q", code(err))
	}
	body := []byte(`{"tenant":"acme"}`)
	adm, err := SignInstallerAdmin(w.instPriv, w.cert, AdminRequest{Op: OpPairCreate, Nonce: RandomNonce(16),
		IssuedAt: w.now.Unix(), ExpiresAt: w.now.Add(time.Minute).Unix()}, body)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.kr.VerifyAdmin(adm, w.now, OpPairCreate, body, 1); code(err) != CodeMalformed {
		t.Errorf("an installer admin request as a team one: %q", code(err))
	}
	// the certificate itself is a team signature over another domain: never a command, a request or a policy
	if _, _, err := w.kr.VerifyEnvelope(w.cert, w.now, "acme-erp", 1); code(err) != CodeBadSignature {
		t.Errorf("a certificate as a team envelope: %q", code(err))
	}
	if _, _, err := w.kr.VerifyAdmin(w.cert, w.now, OpPairCreate, body, 1); code(err) != CodeBadSignature {
		t.Errorf("a certificate as a team admin request: %q", code(err))
	}
	if _, _, err := w.kr.VerifySignedPolicy(SignedPolicy{KeyID: w.cert.Key, Policy: w.cert.Body, Sig: w.cert.Sig}, 1); code(err) != CodeBadSignature {
		t.Errorf("a certificate as a policy: %q", code(err))
	}
	// an installer's key signing a policy under its id is no team key's
	pol, _ := sign(w.instPriv, "inst-dana", domainPolicy, map[string]any{"v": Version})
	if _, _, err := w.kr.VerifySignedPolicy(SignedPolicy{KeyID: pol.Key, Policy: pol.Body, Sig: pol.Sig}, 1); code(err) != CodeKeyUnknown {
		t.Errorf("an installer's policy: %q", code(err))
	}
	// and a team envelope dressed with a certificate is no installer's
	teamEnv, _ := SignEnvelope(w.teamPriv, "team-1", w.envelope("acme-erp"))
	teamEnv.Cert = &w.cert
	if _, _, _, err := w.kr.VerifyInstallerEnvelope(teamEnv, w.now, "acme-erp", 1); code(err) != CodeKeyUnknown {
		t.Errorf("a team envelope with a certificate attached: %q", code(err))
	}
	teamEnv.Key = "inst-dana"
	if _, _, _, err := w.kr.VerifyInstallerEnvelope(teamEnv, w.now, "acme-erp", 1); code(err) != CodeBadSignature {
		t.Errorf("a team envelope relabelled as the installer's: %q", code(err))
	}
}

func TestInstallerAdmin(t *testing.T) {
	w := newInstallerWorld(t)
	body := []byte(`{"tenant":"acme","alias":"acme-erp"}`)
	r := AdminRequest{Op: OpPairCreate, Nonce: RandomNonce(16), IssuedAt: w.now.Unix(), ExpiresAt: w.now.Add(time.Minute).Unix()}
	s, err := SignInstallerAdmin(w.instPriv, w.cert, r, body)
	if err != nil {
		t.Fatal(err)
	}
	got, c, _, err := w.kr.VerifyInstallerAdmin(s, w.now, OpPairCreate, body, 1)
	if err != nil || got.Op != OpPairCreate || c.Name != "dana" {
		t.Fatalf("a good installer request: %+v %+v %v", got, c, err)
	}
	if _, _, _, err := w.kr.VerifyInstallerAdmin(s, w.now, OpBoxesList, body, 1); code(err) != CodeWrongOp {
		t.Errorf("another operation: %q", code(err))
	}
	if _, _, _, err := w.kr.VerifyInstallerAdmin(s, w.now, OpPairCreate, []byte(`{}`), 1); code(err) != CodeBodyMismatch {
		t.Errorf("another body: %q", code(err))
	}
	env, _ := SignInstallerEnvelope(w.instPriv, w.cert, w.envelope("acme-erp"))
	if _, _, _, err := w.kr.VerifyInstallerAdmin(env, w.now, OpPairCreate, body, 1); code(err) != CodeBadSignature {
		t.Errorf("an installer envelope as an installer request: %q", code(err))
	}
}
