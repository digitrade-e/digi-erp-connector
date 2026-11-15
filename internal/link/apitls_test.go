package link

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The link's TLS hop to the daemon trusts exactly the daemon's own certificate (CodeQL go/disabled-certificate-check).
func TestAPITLSConfigTrustsOnlyTheDaemonsCertificate(t *testing.T) {
	daemon := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer daemon.Close()
	pemOf := func(s *httptest.Server) []byte {
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})
	}
	// another self-signed certificate for the same address (httptest's servers all share one certificate)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "other"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	otherPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	dial := func(addr string, certPEM []byte) error {
		conf, err := APITLSConfig(certPEM)
		if err != nil {
			return err
		}
		c, err := net.Dial("tcp", addr)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		return tls.Client(c, conf).Handshake()
	}
	if err := dial(daemon.Listener.Addr().String(), pemOf(daemon)); err != nil {
		t.Fatalf("the daemon's own certificate: %v", err)
	}
	if err := dial(daemon.Listener.Addr().String(), otherPEM); err == nil {
		t.Fatal("another certificate was trusted")
	}
	if _, err := APITLSConfig(nil); err == nil {
		t.Fatal("no certificate: a config without verification")
	}
}
