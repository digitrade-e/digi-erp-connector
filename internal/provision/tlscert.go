package provision

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"slices"
	"time"
)

const (
	// certValidity is long on purpose: both callers skip peer verification,
	// so nobody checks the dates, and a pair that expires is a chore with no
	// security in it.
	certValidity = 10 * 365 * 24 * time.Hour
	// certRenewBefore: a pair this close to expiry is replaced.
	certRenewBefore = 365 * 24 * time.Hour
)

// selfSignedPair returns a PEM certificate and a PEM ECDSA P-256 key for
// hosts: an IP becomes an IP SAN, anything else a DNS SAN. erp-manager checks
// the host name (verify_host is on), so the address in its baseUrl must be
// one of them.
func selfSignedPair(hosts []string, now time.Time) (certPEM, keyPEM []byte, err error) {
	if len(hosts) == 0 {
		return nil, nil, errors.New("a certificate needs at least one host")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate the key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, fmt.Errorf("generate a serial number: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: hosts[0], Organization: []string{"digi-erp-connector"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create the certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("encode the key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}

// certCovers reports whether certPEM already names exactly hosts and stays
// valid for more than certRenewBefore — then Apply keeps it.
func certCovers(certPEM []byte, hosts []string, now time.Time) bool {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return false
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil || now.Before(c.NotBefore) || now.Add(certRenewBefore).After(c.NotAfter) {
		return false
	}
	var have []string
	for _, ip := range c.IPAddresses {
		have = append(have, ip.String())
	}
	have = append(have, c.DNSNames...)
	want := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			h = ip.String()
		}
		want = append(want, h)
	}
	slices.Sort(have)
	slices.Sort(want)
	return slices.Equal(slices.Compact(have), slices.Compact(want))
}
