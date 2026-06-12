/*
Copyright 2025 The Scion Authors.
*/

package telemetry

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadGCPDialOptions_EmptyPath(t *testing.T) {
	opts, err := loadGCPDialOptions(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts != nil {
		t.Errorf("expected nil options for empty path, got %v", opts)
	}
}

func TestLoadGCPDialOptions_InvalidPath(t *testing.T) {
	_, err := loadGCPDialOptions(context.Background(), "/nonexistent/path/sa.json")
	if err == nil {
		t.Fatal("expected error for non-existent file")
	}
}

func TestLoadGCPDialOptions_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	credFile := filepath.Join(tmpDir, "bad.json")
	if err := os.WriteFile(credFile, []byte("not-json"), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := loadGCPDialOptions(context.Background(), credFile)
	if err == nil {
		t.Fatal("expected error for invalid JSON credentials")
	}
}

func TestLoadGCPDialOptions_ValidKey(t *testing.T) {
	// Generate a test RSA private key
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}
	privBytes := x509.MarshalPKCS1PrivateKey(privKey)
	privPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: privBytes,
	})

	// Build a minimal service account JSON key
	saKey := map[string]string{
		"type":                        "service_account",
		"project_id":                  "test-project",
		"private_key_id":              "key-id",
		"private_key":                 string(privPEM),
		"client_email":                "test@test-project.iam.gserviceaccount.com",
		"client_id":                   "123456789",
		"auth_uri":                    "https://accounts.google.com/o/oauth2/auth",
		"token_uri":                   "https://oauth2.googleapis.com/token",
		"auth_provider_x509_cert_url": "https://www.googleapis.com/oauth2/v1/certs",
		"client_x509_cert_url":        "https://www.googleapis.com/robot/v1/metadata/x509/test",
	}
	keyJSON, err := json.Marshal(saKey)
	if err != nil {
		t.Fatalf("failed to marshal SA key: %v", err)
	}

	tmpDir := t.TempDir()
	credFile := filepath.Join(tmpDir, "sa.json")
	if err := os.WriteFile(credFile, keyJSON, 0600); err != nil {
		t.Fatal(err)
	}

	opts, err := loadGCPDialOptions(context.Background(), credFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(opts) == 0 {
		t.Error("expected non-empty dial options for valid key")
	}
}

func TestLoadOTLPTLSConfig_EmptyPath(t *testing.T) {
	tlsConfig, err := LoadOTLPTLSConfig("", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tlsConfig == nil {
		t.Fatal("expected non-nil tls.Config")
	}
	if tlsConfig.RootCAs != nil {
		t.Errorf("expected nil RootCAs without CA file, got %#v", tlsConfig.RootCAs)
	}
}

func TestLoadOTLPTLSConfig_InvalidPath(t *testing.T) {
	_, err := LoadOTLPTLSConfig("/nonexistent/path/root.pem", "", "")
	if err == nil {
		t.Fatal("expected error for missing CA file")
	}
}

func TestLoadOTLPTLSConfig_ValidCAFile(t *testing.T) {
	tmpDir := t.TempDir()
	caPath := filepath.Join(tmpDir, "root.pem")
	caPEM := generateTestCertificatePEM(t)
	if err := os.WriteFile(caPath, caPEM, 0600); err != nil {
		t.Fatalf("failed to write CA file: %v", err)
	}

	tlsConfig, err := LoadOTLPTLSConfig(caPath, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tlsConfig.RootCAs == nil {
		t.Fatal("expected custom RootCAs to be configured")
	}
	block, _ := pem.Decode(caPEM)
	if block == nil {
		t.Fatal("expected PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("failed to parse generated certificate: %v", err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: tlsConfig.RootCAs}); err != nil {
		t.Fatalf("expected generated certificate to verify against loaded RootCAs: %v", err)
	}
}

func TestLoadOTLPTLSConfig_MissingClientKeyPair(t *testing.T) {
	_, err := LoadOTLPTLSConfig("", "/tmp/client.pem", "")
	if !errors.Is(err, errOTLPMissingClientKeyPair) {
		t.Fatalf("expected errOTLPMissingClientKeyPair, got %v", err)
	}
}

func TestLoadOTLPTLSConfig_ClientKeyPair(t *testing.T) {
	tmpDir := t.TempDir()
	certPEM, keyPEM := generateTestClientKeyPairPEM(t)
	certPath := filepath.Join(tmpDir, "client.pem")
	keyPath := filepath.Join(tmpDir, "client-key.pem")
	if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		t.Fatal(err)
	}

	tlsConfig, err := LoadOTLPTLSConfig("", certPath, keyPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tlsConfig.Certificates) != 1 {
		t.Fatalf("expected one client certificate, got %d", len(tlsConfig.Certificates))
	}
}

func generateTestCertificatePEM(t *testing.T) []byte {
	t.Helper()

	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "scion-test-root",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &privKey.PublicKey, privKey)
	if err != nil {
		t.Fatalf("failed to create test certificate: %v", err)
	}

	return pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: derBytes,
	})
}

func generateTestClientKeyPairPEM(t *testing.T) ([]byte, []byte) {
	t.Helper()

	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "scion-test-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &privKey.PublicKey, privKey)
	if err != nil {
		t.Fatalf("failed to create test client certificate: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privKey)})
	return certPEM, keyPEM
}
