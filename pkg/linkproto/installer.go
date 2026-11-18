package linkproto

import (
	"crypto/ed25519"
	"encoding/base64"
	"regexp"
	"time"
)

// Installers (Digitrade plan 090 D1): a project manager's own Ed25519 key, certified by a team key. The certificate
// is the only thing that makes such a key count, and it says nothing about what the key may do: that is fixed in
// code, at the relay (pairing, its own boxes) and on the box (reads only on a box bound to this installer; plan 090
// D3). An installer signature is made under its own domains, so it can never be read as a team signature.

// MaxInstallerCertLife bounds a certificate: a stolen key stops counting within this, whatever the relay does.
const MaxInstallerCertLife = 90 * 24 * time.Hour

// InstallerCert is what a team key certifies about an installer's key.
type InstallerCert struct {
	V         int    `json:"v"`
	ID        string `json:"id"`   // inst-<name>: never a team key's id
	Name      string `json:"name"` // the person, as the team names them (a handle, not an e-mail)
	Pub       string `json:"pub"`  // base64 Ed25519 public key
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

const (
	domainInstallerCert     = "digi-link-installer-cert-v1\x00"
	domainInstallerEnvelope = "digi-link-installer-envelope-v1\x00"
	domainInstallerAdmin    = "digi-link-installer-admin-v1\x00"
)

var (
	installerIDShape   = regexp.MustCompile(`^inst-[a-z0-9][a-z0-9-]{1,40}$`)
	installerNameShape = regexp.MustCompile(`^[0-9A-Za-z._-]{1,64}$`)
)

// ValidInstallerID reports whether id has the shape of an installer id.
func ValidInstallerID(id string) bool { return installerIDShape.MatchString(id) }

// PublicKey is the certified key.
func (c InstallerCert) PublicKey() (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(c.Pub)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, refuse(CodeBadKey)
	}
	return ed25519.PublicKey(b), nil
}

func (c InstallerCert) checkShape() error {
	switch {
	case c.V != Version:
		return refuse(CodeVersion)
	case !ValidInstallerID(c.ID), !installerNameShape.MatchString(c.Name):
		return refuse(CodeMalformed)
	}
	_, err := c.PublicKey()
	return err
}

// checkCertTimes: issued no later than now plus the skew, not yet expired, and alive no longer than the bound.
func checkCertTimes(iat, exp int64, now time.Time) error {
	issued, expires := time.Unix(iat, 0), time.Unix(exp, 0)
	switch {
	case issued.After(now.Add(MaxClockSkew)):
		return refuse(CodeFromFuture)
	case !expires.After(now):
		return refuse(CodeExpired)
	case !expires.After(issued), expires.Sub(issued) > MaxInstallerCertLife:
		return refuse(CodeLifetime)
	}
	return nil
}

// SignInstallerCert certifies c with a team key (the team's digi-mcp, behind its approval form).
func SignInstallerCert(priv ed25519.PrivateKey, keyID string, c InstallerCert) (Signed, error) {
	c.V = Version
	if err := c.checkShape(); err != nil {
		return Signed{}, err
	}
	if !time.Unix(c.ExpiresAt, 0).After(time.Unix(c.IssuedAt, 0)) || time.Unix(c.ExpiresAt, 0).Sub(time.Unix(c.IssuedAt, 0)) > MaxInstallerCertLife {
		return Signed{}, refuse(CodeLifetime)
	}
	return sign(priv, keyID, domainInstallerCert, c)
}

// VerifyInstallerCert checks a certificate at time now: a trusted team key at or above minEpoch, the signature under
// the certificate domain, the shape, and the times.
func (kr Keyring) VerifyInstallerCert(s Signed, now time.Time, minEpoch int) (InstallerCert, TeamKey, error) {
	k, err := kr.verify(s, domainInstallerCert, minEpoch) // refuses a Signed carrying a certificate of its own
	if err != nil {
		return InstallerCert{}, TeamKey{}, err
	}
	var c InstallerCert
	if err := strictUnmarshal(s.Body, &c); err != nil {
		return InstallerCert{}, TeamKey{}, err
	}
	if err := c.checkShape(); err != nil {
		return InstallerCert{}, TeamKey{}, err
	}
	if err := checkCertTimes(c.IssuedAt, c.ExpiresAt, now); err != nil {
		return InstallerCert{}, TeamKey{}, err
	}
	return c, k, nil
}

// installerSign signs v with an installer's key under domain and attaches its certificate. The certificate must name
// priv's public key: a signer's mix-up is caught here, not at the box.
func installerSign(priv ed25519.PrivateKey, cert Signed, domain string, v any) (Signed, error) {
	var c InstallerCert
	if err := strictUnmarshal(cert.Body, &c); err != nil {
		return Signed{}, err
	}
	pub, err := c.PublicKey()
	if err != nil {
		return Signed{}, err
	}
	if !pub.Equal(priv.Public()) {
		return Signed{}, refuse(CodeBadKey)
	}
	s, err := sign(priv, c.ID, domain, v)
	if err != nil {
		return Signed{}, err
	}
	cert.Cert = nil
	s.Cert = &cert
	return s, nil
}

// installerVerify checks the certificate s carries, then s's signature under domain with the certified key.
func (kr Keyring) installerVerify(s Signed, domain string, now time.Time, minEpoch int) (InstallerCert, TeamKey, error) {
	if s.Cert == nil {
		return InstallerCert{}, TeamKey{}, refuse(CodeMalformed)
	}
	c, k, err := kr.VerifyInstallerCert(*s.Cert, now, minEpoch)
	if err != nil {
		return InstallerCert{}, TeamKey{}, err
	}
	if s.Key != c.ID {
		return InstallerCert{}, TeamKey{}, refuse(CodeKeyUnknown)
	}
	pub, _ := c.PublicKey() // checked by VerifyInstallerCert
	sig, err := base64.StdEncoding.DecodeString(s.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize || !ed25519.Verify(pub, append([]byte(domain), s.Body...), sig) {
		return InstallerCert{}, TeamKey{}, refuse(CodeBadSignature)
	}
	return c, k, nil
}

// SignInstallerEnvelope signs e with an installer's key; cert is its team-signed certificate.
func SignInstallerEnvelope(priv ed25519.PrivateKey, cert Signed, e Envelope) (Signed, error) {
	e.V = Version
	return installerSign(priv, cert, domainInstallerEnvelope, e)
}

// VerifyInstallerEnvelope checks a command an installer signed, for box at time now: the certificate against the team
// keys at or above minEpoch, the envelope against the certified key, then the envelope's shape, box and times. What
// an installer may run is the caller's to decide; the nonce's novelty too.
func (kr Keyring) VerifyInstallerEnvelope(s Signed, now time.Time, box string, minEpoch int) (Envelope, InstallerCert, TeamKey, error) {
	c, k, err := kr.installerVerify(s, domainInstallerEnvelope, now, minEpoch)
	if err != nil {
		return Envelope{}, InstallerCert{}, TeamKey{}, err
	}
	e, err := decodeEnvelope(s.Body, now, box)
	if err != nil {
		return Envelope{}, InstallerCert{}, TeamKey{}, err
	}
	return e, c, k, nil
}

// SignInstallerAdmin signs a request to the relay's operation op over body with an installer's key.
func SignInstallerAdmin(priv ed25519.PrivateKey, cert Signed, r AdminRequest, body []byte) (Signed, error) {
	r.V = Version
	r.BodySHA256 = SHA256Hex(body)
	return installerSign(priv, cert, domainInstallerAdmin, r)
}

// VerifyInstallerAdmin checks an installer's request to op carrying body. Which operations an installer may call,
// and on what, is the relay's to decide.
func (kr Keyring) VerifyInstallerAdmin(s Signed, now time.Time, op string, body []byte, minEpoch int) (AdminRequest, InstallerCert, TeamKey, error) {
	c, k, err := kr.installerVerify(s, domainInstallerAdmin, now, minEpoch)
	if err != nil {
		return AdminRequest{}, InstallerCert{}, TeamKey{}, err
	}
	r, err := decodeAdmin(s.Body, now, op, body)
	if err != nil {
		return AdminRequest{}, InstallerCert{}, TeamKey{}, err
	}
	return r, c, k, nil
}
