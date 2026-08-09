package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// writeSelfSignedPair writes a usable cert/key pair and returns their paths.
func writeSelfSignedPair(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "digi-erp-connector-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	certPEM, _ := os.Create(certFile)
	defer certPEM.Close()
	if err := pem.Encode(certPEM, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("encode cert: %v", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM, _ := os.Create(keyFile)
	defer keyPEM.Close()
	if err := pem.Encode(keyPEM, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		t.Fatalf("encode key: %v", err)
	}

	return certFile, keyFile
}

func TestServerWithoutTLSHasNoTLSConfig(t *testing.T) {
	cfg := config.Default()
	cfg.BearerToken = testStaticToken

	srv := mustServer(t, cfg)
	if srv.TLSConfig != nil {
		t.Error("TLSConfig should be nil when tls is not configured")
	}
}

func TestServerWithTLSSetsAMinimumVersion(t *testing.T) {
	certFile, keyFile := writeSelfSignedPair(t)

	cfg := config.Default()
	cfg.BearerToken = testStaticToken
	cfg.TLS = config.TLSConfig{CertFile: certFile, KeyFile: keyFile}

	srv := mustServer(t, cfg)
	if srv.TLSConfig == nil {
		t.Fatal("TLSConfig is nil although tls is configured")
	}
	// TLS 1.0/1.1 are broken; refusing them is the point of setting this.
	if srv.TLSConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want TLS 1.2 (%x)", srv.TLSConfig.MinVersion, tls.VersionTLS12)
	}
}

// Half-configured TLS must stop the server starting. Silently serving plaintext
// when the operator believes they configured HTTPS is how a token leaks.
func TestServerRejectsHalfConfiguredTLS(t *testing.T) {
	certFile, keyFile := writeSelfSignedPair(t)

	for _, tc := range []struct {
		name string
		tls  config.TLSConfig
	}{
		{"cert without key", config.TLSConfig{CertFile: certFile}},
		{"key without cert", config.TLSConfig{KeyFile: keyFile}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.BearerToken = testStaticToken
			cfg.TLS = tc.tls

			_, err := newServerForTest(t, cfg)
			if err == nil {
				t.Fatal("expected NewServer to refuse a half-configured TLS block")
			}
			if !strings.Contains(err.Error(), "certFile") || !strings.Contains(err.Error(), "keyFile") {
				t.Errorf("error %q should name both settings", err)
			}
		})
	}
}

// A missing or mismatched pair must fail at startup, not on the first request.
func TestServerRejectsUnusableCertificate(t *testing.T) {
	certFile, keyFile := writeSelfSignedPair(t)
	otherCert, _ := writeSelfSignedPair(t)

	for _, tc := range []struct {
		name string
		tls  config.TLSConfig
	}{
		{"missing files", config.TLSConfig{CertFile: "nope.pem", KeyFile: "nope.key"}},
		{"key does not match cert", config.TLSConfig{CertFile: otherCert, KeyFile: keyFile}},
		{"cert file is not a certificate", config.TLSConfig{CertFile: keyFile, KeyFile: certFile}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.BearerToken = testStaticToken
			cfg.TLS = tc.tls

			if _, err := newServerForTest(t, cfg); err == nil {
				t.Error("expected NewServer to reject an unusable certificate pair")
			}
		})
	}
}

// End to end: the server really speaks HTTPS and really rejects a plaintext
// request, so the credential cannot cross the wire in the clear.
func TestServerServesHTTPS(t *testing.T) {
	certFile, keyFile := writeSelfSignedPair(t)

	cfg := config.Default()
	cfg.BearerToken = testStaticToken
	// Only used for validation; the listener below is what actually binds.
	cfg.APIListen = "127.0.0.1:8443"
	cfg.TLS = config.TLSConfig{CertFile: certFile, KeyFile: keyFile}

	srv := mustServer(t, cfg)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.ServeTLS(ln, certFile, keyFile) }()
	t.Cleanup(func() { _ = srv.Close() })

	url := "https://" + ln.Addr().String() + "/api/folders/list"
	client := &http.Client{
		Timeout: 5 * time.Second,
		// The certificate is self-signed; this test is about the transport.
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}

	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+testStaticToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("HTTPS request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, body)
	}
	if resp.TLS == nil {
		t.Fatal("connection was not TLS")
	}
	if resp.TLS.Version < tls.VersionTLS12 {
		t.Errorf("negotiated TLS %x, want at least 1.2", resp.TLS.Version)
	}

	// A plaintext request must not be served. net/http answers such a request
	// with 400 and an explanatory body rather than refusing the connection, which
	// is helpful — but it must never reach a handler.
	plain := &http.Client{Timeout: 3 * time.Second}
	plainResp, err := plain.Get("http://" + ln.Addr().String() + "/api/folders/list")
	if err != nil {
		return // connection refused is also an acceptable outcome
	}
	defer plainResp.Body.Close()

	if plainResp.StatusCode/100 == 2 {
		t.Fatalf("a plaintext request was served with %d — the listener is not TLS-only", plainResp.StatusCode)
	}
	plainBody, _ := io.ReadAll(plainResp.Body)
	if !strings.Contains(string(plainBody), "HTTPS") {
		t.Errorf("plaintext request got %d %q; expected it to be rejected as non-HTTPS",
			plainResp.StatusCode, plainBody)
	}
}
