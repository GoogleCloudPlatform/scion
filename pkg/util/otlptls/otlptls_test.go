/*
Copyright 2026 The Scion Authors.
*/

package otlptls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testCA is a synthetic CA generated in-test. Its PEM files live only under
// the test's TempDir.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	der  []byte
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	return newNamedTestCA(t, "otlptls-test-ca")
}

// newNamedTestCA is newTestCA with the given subject common name, so two
// CAs in one test have distinct subjects.
func newNamedTestCA(t *testing.T, cn string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key, der: der}
}

// clientPair returns a PEM client certificate with the given serial and its
// PEM key, signed by the CA.
func (ca *testCA) clientPair(t *testing.T, serial int64) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "otlptls-test-client"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// pairFiles writes a CA file and a client pair with the given serial.
func pairFiles(t *testing.T, ca *testCA, serial int64) (caFile, certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()
	caFile = filepath.Join(dir, "ca.pem")
	certFile = filepath.Join(dir, "client.pem")
	keyFile = filepath.Join(dir, "client-key.pem")
	writeFile(t, caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.der}))
	certPEM, keyPEM := ca.clientPair(t, serial)
	writeFile(t, certFile, certPEM)
	writeFile(t, keyFile, keyPEM)
	return caFile, certFile, keyFile
}

// certificateRequest returns a realistic TLS 1.3 certificate request that
// accepts the test client certificates (ECDSA P-256), optionally limited to
// the given acceptable CA subjects.
func certificateRequest(acceptableCAs ...[]byte) *tls.CertificateRequestInfo {
	return &tls.CertificateRequestInfo{
		Version:          tls.VersionTLS13,
		SignatureSchemes: []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256, tls.PSSWithSHA256},
		AcceptableCAs:    acceptableCAs,
	}
}

// presentedCert calls the config's client certificate callback with cri, as
// a TLS handshake does, and returns what it presents.
func presentedCert(t *testing.T, cfg *tls.Config, cri *tls.CertificateRequestInfo) *tls.Certificate {
	t.Helper()
	if cfg.GetClientCertificate == nil {
		t.Fatal("GetClientCertificate is not set")
	}
	cert, err := cfg.GetClientCertificate(cri)
	if err != nil {
		t.Fatalf("GetClientCertificate: %v", err)
	}
	if cert == nil {
		t.Fatal("GetClientCertificate returned nil; crypto/tls needs an empty certificate to send none")
	}
	return cert
}

// presentedSerial returns the serial of the certificate presented for a
// request that accepts it.
func presentedSerial(t *testing.T, cfg *tls.Config) int64 {
	t.Helper()
	cert := presentedCert(t, cfg, certificateRequest())
	if len(cert.Certificate) == 0 {
		t.Fatal("GetClientCertificate returned no certificate")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.SerialNumber.Int64()
}

func TestLoadClientConfig_CAAndPair(t *testing.T) {
	ca := newTestCA(t)
	caFile, certFile, keyFile := pairFiles(t, ca, 10)
	cfg, err := LoadClientConfig(caFile, certFile, keyFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %#x, want TLS 1.2", cfg.MinVersion)
	}
	if cfg.RootCAs == nil {
		t.Fatal("expected the CA bundle to be loaded")
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("expected one client certificate, got %d", len(cfg.Certificates))
	}
	if got := presentedSerial(t, cfg); got != 10 {
		t.Fatalf("presented serial = %d, want 10", got)
	}

	empty, err := LoadClientConfig("", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if empty.RootCAs != nil || len(empty.Certificates) != 0 || empty.GetClientCertificate != nil {
		t.Fatal("expected system defaults with no CA and no client certificate")
	}
}

func TestLoadClientConfig_IncompletePair(t *testing.T) {
	ca := newTestCA(t)
	_, certFile, keyFile := pairFiles(t, ca, 11)
	for _, tc := range []struct {
		name, cert, key, wantPath string
	}{
		{"certificate without key", certFile, "", certFile},
		{"key without certificate", "", keyFile, keyFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadClientConfig("", tc.cert, tc.key)
			if !errors.Is(err, ErrIncompleteClientKeyPair) {
				t.Fatalf("error = %v, want ErrIncompleteClientKeyPair", err)
			}
			if !namesPath(err, tc.wantPath) {
				t.Fatalf("error %q does not name %q", err, tc.wantPath)
			}
		})
	}
}

// TestLoadClientConfig_BadFilesNamePathOnly checks that bad files fail at
// load with an error that names the path and never echoes file contents.
func TestLoadClientConfig_BadFilesNamePathOnly(t *testing.T) {
	ca := newTestCA(t)
	_, certFile, keyFile := pairFiles(t, ca, 12)
	dir := t.TempDir()
	const garbage = "synthetic-not-a-pem-payload-5c1d"
	garbageFile := filepath.Join(dir, "garbage.pem")
	writeFile(t, garbageFile, []byte(garbage))
	missing := filepath.Join(dir, "missing.pem")

	for _, tc := range []struct {
		name, ca, cert, key, wantPath string
		wantIs                        error
	}{
		{"missing CA", missing, "", "", missing, nil},
		{"CA without certificates", garbageFile, "", "", garbageFile, ErrCACertsNotFound},
		{"missing certificate", "", missing, keyFile, missing, nil},
		{"unparseable key", "", certFile, garbageFile, garbageFile, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadClientConfig(tc.ca, tc.cert, tc.key)
			if err == nil {
				t.Fatal("expected an error")
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Fatalf("error = %v, want %v", err, tc.wantIs)
			}
			if !namesPath(err, tc.wantPath) {
				t.Fatalf("error %q does not name %q", err, tc.wantPath)
			}
			if strings.Contains(err.Error(), garbage) {
				t.Fatalf("error %q leaks file contents", err)
			}
		})
	}
}

// TestLoadClientConfig_ClientCertRotation replaces the pair on disk after
// load and checks that the next handshake presents the new certificate.
func TestLoadClientConfig_ClientCertRotation(t *testing.T) {
	ca := newTestCA(t)
	caFile, certFile, keyFile := pairFiles(t, ca, 20)
	cfg, err := LoadClientConfig(caFile, certFile, keyFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := presentedSerial(t, cfg); got != 20 {
		t.Fatalf("presented serial before rotation = %d, want 20", got)
	}

	certPEM, keyPEM := ca.clientPair(t, 21)
	writeFile(t, certFile, certPEM)
	writeFile(t, keyFile, keyPEM)

	if got := presentedSerial(t, cfg); got != 21 {
		t.Fatalf("presented serial after rotation = %d, want 21", got)
	}
}

// TestLoadClientConfig_RotationKeepsLastGoodPair checks that a pair that
// cannot be read mid-rotation does not break the handshake: the last pair
// that loaded is presented, and the new pair is used once it is complete.
func TestLoadClientConfig_RotationKeepsLastGoodPair(t *testing.T) {
	ca := newTestCA(t)
	caFile, certFile, keyFile := pairFiles(t, ca, 30)
	cfg, err := LoadClientConfig(caFile, certFile, keyFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Half-done rotation: new certificate, old key. The pair does not match.
	certPEM, keyPEM := ca.clientPair(t, 31)
	writeFile(t, certFile, certPEM)
	if got := presentedSerial(t, cfg); got != 30 {
		t.Fatalf("presented serial with mismatched pair = %d, want last good 30", got)
	}

	writeFile(t, keyFile, keyPEM)
	if got := presentedSerial(t, cfg); got != 31 {
		t.Fatalf("presented serial after completed rotation = %d, want 31", got)
	}
}

func TestParseHeaders(t *testing.T) {
	const secret = "synthetic-header-secret-9e2b"
	for _, tc := range []struct {
		name    string
		raw     string
		want    map[string]string
		wantErr bool
	}{
		{"empty", "", nil, false},
		{"object", `{"X-Api-Key":"v1","X-Tenant":"t"}`, map[string]string{"X-Api-Key": "v1", "X-Tenant": "t"}, false},
		{"not JSON", "X-Api-Key=" + secret, nil, true},
		{"non-string value", `{"X-Api-Key":1}`, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseHeaders(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %t", err, tc.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), secret) {
				t.Fatalf("error %q echoes the header value", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("headers = %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("headers[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

// TestLoadClientConfig_PresentsOnlyAcceptableCert checks that the reload
// callback keeps crypto/tls's selection: the certificate is presented when
// the server's acceptable CAs include its issuer, and no certificate is sent
// when they do not.
func TestLoadClientConfig_PresentsOnlyAcceptableCert(t *testing.T) {
	ca := newTestCA(t)
	other := newNamedTestCA(t, "otlptls-other-ca")
	caFile, certFile, keyFile := pairFiles(t, ca, 40)
	cfg, err := LoadClientConfig(caFile, certFile, keyFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	accepted := presentedCert(t, cfg, certificateRequest(ca.cert.RawSubject))
	if len(accepted.Certificate) == 0 {
		t.Fatal("no certificate presented although the acceptable CAs include its issuer")
	}

	refused := presentedCert(t, cfg, certificateRequest(other.cert.RawSubject))
	if len(refused.Certificate) != 0 {
		t.Fatal("a certificate was presented although the acceptable CAs do not include its issuer")
	}
}

// namesPath reports whether err names path, either as written or quoted
// with %q (which doubles the backslashes of a Windows path).
func namesPath(err error, path string) bool {
	msg := err.Error()
	return strings.Contains(msg, path) || strings.Contains(msg, strconv.Quote(path))
}
