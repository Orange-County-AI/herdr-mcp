package httptransport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A private synthetic CA signs a server certificate with a loopback IP SAN.
// Clients must trust this CA explicitly; verification stays enabled throughout.
func testCertificate(t *testing.T) (string, string, *x509.CertPool) {
	return testCertificateAt(t, time.Now().Add(time.Hour))
}

func testCertificateAt(t *testing.T, leafExpires time.Time) (string, string, *x509.CertPool) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"}, NotBefore: now.Add(-time.Hour), NotAfter: leafExpires, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	certPath, keyPath := filepath.Join(root, "cert.pem"), filepath.Join(root, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})...)
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	return certPath, keyPath, roots
}

func TestNativeHTTPS(t *testing.T) {
	cert, key, roots := testCertificate(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := bearerConfig(listener.Addr().String())
	cfg.TLSCertFile, cfg.TLSKeyFile = cert, key
	transport, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: transport.Guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }), nil), ErrorLog: log.New(io.Discard, "", 0)}
	finished := make(chan error, 1)
	go func() { finished <- transport.Serve(server, listener) }()
	t.Cleanup(func() {
		server.Close()
		if err := <-finished; err != http.ErrServerClosed {
			t.Error(err)
		}
	})
	endpoint := "https://" + listener.Addr().String() + "/mcp"
	trusted := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}, Timeout: 3 * time.Second}
	defer trusted.CloseIdleConnections()
	for _, auth := range []string{"Bearer " + testSecret, "", "Bearer " + wrongSecret} {
		req, _ := http.NewRequest("POST", endpoint, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		response, err := trusted.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		want := 401
		if auth == "Bearer "+testSecret {
			want = 204
		}
		if response.StatusCode != want {
			t.Fatalf("status=%d want=%d", response.StatusCode, want)
		}
		if response.TLS.Version < tls.VersionTLS12 {
			t.Fatal("obsolete TLS negotiated")
		}
	}
	untrusted := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: x509.NewCertPool()}}, Timeout: 3 * time.Second}
	defer untrusted.CloseIdleConnections()
	if response, err := untrusted.Get(endpoint); err == nil {
		response.Body.Close()
		t.Fatal("untrusted CA accepted")
	}
	if conn, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11}); err == nil {
		conn.Close()
		t.Fatal("TLS 1.1 accepted")
	}
	if response, err := http.Get("http://" + listener.Addr().String() + "/mcp"); err == nil {
		response.Body.Close()
		if response.StatusCode != 400 {
			t.Fatal("plaintext reached TLS server")
		}
	}
}

func TestTLSAndPrivateConfiguration(t *testing.T) {
	cert, key, _ := testCertificate(t)
	good := bearerConfig("192.168.10.2:8091")
	good.AllowPrivate, good.TLSCertFile, good.TLSKeyFile = true, cert, key
	if _, err := New(good); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(*Config){
		func(c *Config) { c.AllowPrivate = false },
		func(c *Config) { c.LookupEnv = noEnv },
		func(c *Config) { c.TLSCertFile, c.TLSKeyFile = "", "" },
		func(c *Config) { c.TLSKeyFile = "" },
		func(c *Config) { c.TLSCertFile = "" },
		func(c *Config) { c.TLSKeyFile = cert },
		func(c *Config) { c.TLSCertFile = key },
		func(c *Config) { c.TLSCertFile = "/does/not/exist" },
		func(c *Config) { c.Listen = "0.0.0.0:8091" },
		func(c *Config) { c.Listen = "[::]:8091" },
		func(c *Config) { c.Listen = "8.8.8.8:8091" },
		func(c *Config) { c.Listen = "100.64.0.1:8091" },
		func(c *Config) { c.Listen = "private.example:8091" },
	} {
		c := good
		edit(&c)
		if _, err := New(c); err == nil {
			t.Fatalf("invalid configuration accepted: %+v", c)
		}
	}
	good.Listen = "[fd00::1]:8091"
	if _, err := New(good); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerOnlyTLSKeyAndCertificatePreflight(t *testing.T) {
	cert, key, roots := testCertificate(t)
	cfg := bearerConfig("127.0.0.1:8091")
	cfg.TLSCertFile, cfg.TLSKeyFile = cert, key
	transport, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.VerifyCertificate("127.0.0.1", roots); err != nil {
		t.Fatal(err)
	}
	if err := transport.VerifyCertificate("localhost", roots); err != nil {
		t.Fatal(err)
	}
	if err := transport.VerifyCertificate("different.test", roots); err == nil {
		t.Fatal("wrong SAN accepted")
	}
	if err := transport.VerifyCertificate("127.0.0.1", x509.NewCertPool()); err == nil {
		t.Fatal("untrusted chain accepted")
	}
	os.Chmod(key, 0o644)
	if _, err := New(cfg); err == nil {
		t.Fatal("world-readable private key accepted")
	}
	cfg = Config{Listen: "127.0.0.1:8091", LookupEnv: noEnv, BearerFileSet: true}
	if _, err := New(cfg); err == nil {
		t.Fatal("empty explicit secret source accepted")
	}
	cfg = Config{Listen: "127.0.0.1:8091", LookupEnv: noEnv, TLSSet: true}
	if _, err := New(cfg); err == nil {
		t.Fatal("empty explicit TLS sources accepted")
	}
	if _, err := New(Config{Listen: "127.0.0.1:0", LookupEnv: noEnv}); err == nil {
		t.Fatal("undiscoverable port zero accepted")
	}
}

func TestCertificatePreflightRejectsExpiredCertificate(t *testing.T) {
	cert, key, roots := testCertificateAt(t, time.Now().Add(-time.Minute))
	cfg := bearerConfig("127.0.0.1:8091")
	cfg.TLSCertFile, cfg.TLSKeyFile = cert, key
	transport, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.VerifyCertificate("127.0.0.1", roots); err == nil {
		t.Fatal("expired certificate accepted by installer preflight")
	}
}
