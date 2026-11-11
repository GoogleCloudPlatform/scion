/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// testPKI is a synthetic CA with one server and one client leaf, generated
// in-test. PEM files live only under the test's TempDir.
type testPKI struct {
	caFile, clientCertFile, clientKeyFile string
	caPool                                *x509.CertPool
	serverCert                            tls.Certificate
	// clientKeyPEM is kept so tests can assert that errors never echo it.
	clientKeyPEM []byte
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	dir := t.TempDir()
	now := time.Now()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "scion-test-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	leaf := func(serial int64, cn string, usage x509.ExtKeyUsage, ips []net.IP) (certPEM, keyPEM []byte) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			Subject:      pkix.Name{CommonName: cn},
			NotBefore:    now.Add(-time.Hour),
			NotAfter:     now.Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{usage},
			IPAddresses:  ips,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
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

	serverCertPEM, serverKeyPEM := leaf(2, "scion-test-server", x509.ExtKeyUsageServerAuth, []net.IP{net.IPv4(127, 0, 0, 1)})
	serverCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	clientCertPEM, clientKeyPEM := leaf(3, "scion-test-client", x509.ExtKeyUsageClientAuth, nil)

	p := &testPKI{
		caFile:         filepath.Join(dir, "ca.pem"),
		clientCertFile: filepath.Join(dir, "client.pem"),
		clientKeyFile:  filepath.Join(dir, "client-key.pem"),
		caPool:         x509.NewCertPool(),
		serverCert:     serverCert,
		clientKeyPEM:   clientKeyPEM,
	}
	p.caPool.AddCert(caCert)
	for path, data := range map[string][]byte{
		p.caFile:         pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		p.clientCertFile: clientCertPEM,
		p.clientKeyFile:  clientKeyPEM,
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func TestLoadOTLPTLSConfig_ClientKeyPair(t *testing.T) {
	pki := newTestPKI(t)
	tlsConfig, err := loadOTLPTLSConfig(pki.caFile, pki.clientCertFile, pki.clientKeyFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tlsConfig.Certificates) != 1 {
		t.Fatalf("expected one client certificate, got %d", len(tlsConfig.Certificates))
	}
	if tlsConfig.RootCAs == nil {
		t.Fatal("expected the CA bundle to be loaded alongside the client certificate")
	}
	if tlsConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %#x, want TLS 1.2", tlsConfig.MinVersion)
	}
}

func TestLoadOTLPTLSConfig_ClientKeyPairIncomplete(t *testing.T) {
	pki := newTestPKI(t)
	for _, tc := range []struct {
		name, cert, key, wantPath string
	}{
		{"certificate without key", pki.clientCertFile, "", pki.clientCertFile},
		{"key without certificate", "", pki.clientKeyFile, pki.clientKeyFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadOTLPTLSConfig("", tc.cert, tc.key)
			if !errors.Is(err, errOTLPIncompleteClientKeyPair) {
				t.Fatalf("error = %v, want errOTLPIncompleteClientKeyPair", err)
			}
			if !strings.Contains(err.Error(), tc.wantPath) {
				t.Fatalf("error %q does not name the configured path %q", err, tc.wantPath)
			}
		})
	}
}

// TestLoadOTLPTLSConfig_BadFilesNamePathOnly checks that a missing or
// unparseable CA, certificate or key file fails with an error that names
// the path and never echoes the file contents.
func TestLoadOTLPTLSConfig_BadFilesNamePathOnly(t *testing.T) {
	pki := newTestPKI(t)
	dir := t.TempDir()
	const garbage = "synthetic-not-a-pem-payload-7f3a"
	garbageFile := filepath.Join(dir, "garbage.pem")
	if err := os.WriteFile(garbageFile, []byte(garbage), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.pem")
	// keyBody is the base64 payload of the client key; no error may carry it.
	keyBody := strings.Split(string(pki.clientKeyPEM), "\n")[1]

	for _, tc := range []struct {
		name, ca, cert, key, wantPath string
	}{
		{"missing CA", missing, "", "", missing},
		{"CA without certificates", garbageFile, "", "", garbageFile},
		{"missing certificate", "", missing, pki.clientKeyFile, missing},
		{"missing key", "", pki.clientCertFile, missing, missing},
		{"unparseable certificate", "", garbageFile, pki.clientKeyFile, garbageFile},
		{"unparseable key", "", pki.clientCertFile, garbageFile, garbageFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadOTLPTLSConfig(tc.ca, tc.cert, tc.key)
			if err == nil {
				t.Fatal("expected an error")
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.wantPath) {
				t.Fatalf("error %q does not name %q", msg, tc.wantPath)
			}
			if strings.Contains(msg, garbage) || strings.Contains(msg, keyBody) {
				t.Fatalf("error %q leaks file contents", msg)
			}
		})
	}
}

// TestGenericGRPCMutualTLSAgainstRealServer runs the generic OTLP gRPC
// exporter against an in-process server that requires a client certificate
// signed by the test CA. It covers both TLS consumers: the raw proto client
// (ExportProtoLogs) and the otlptracegrpc trace exporter (ExportSpans).
func TestGenericGRPCMutualTLSAgainstRealServer(t *testing.T) {
	pki := newTestPKI(t)
	for _, tc := range []struct {
		name        string
		cert, key   string
		wantSuccess bool
	}{
		{"with client certificate", pki.clientCertFile, pki.clientKeyFile, true},
		{"without client certificate", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			serverTLS := &tls.Config{
				Certificates: []tls.Certificate{pki.serverCert},
				ClientAuth:   tls.RequireAndVerifyClientCert,
				ClientCAs:    pki.caPool,
				MinVersion:   tls.VersionTLS12,
			}
			var received, receivedSpans atomic.Int32
			server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
			collogspb.RegisterLogsServiceServer(server, &logsServiceServer{handler: func(context.Context, []*logspb.ResourceLogs) error {
				received.Add(1)
				return nil
			}})
			coltracepb.RegisterTraceServiceServer(server, &traceServiceServer{handler: func(context.Context, []*tracepb.ResourceSpans) error {
				receivedSpans.Add(1)
				return nil
			}})
			go func() { _ = server.Serve(listener) }()
			defer server.Stop()

			config := &Config{
				CloudEnabled: true,
				Endpoint:     listener.Addr().String(),
				Protocol:     "grpc",
				CAFile:       pki.caFile,
				CertFile:     tc.cert,
				KeyFile:      tc.key,
			}
			exporter, err := NewCloudExporter(context.Background(), config)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = exporter.Shutdown(context.Background()) }()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			err = exporter.ExportProtoLogs(ctx, []*logspb.ResourceLogs{{}})
			if (err == nil) != tc.wantSuccess {
				t.Fatalf("ExportProtoLogs error = %v, want success=%t", err, tc.wantSuccess)
			}
			wantReceived := int32(0)
			if tc.wantSuccess {
				wantReceived = 1
			}
			if got := received.Load(); got != wantReceived {
				t.Fatalf("server received %d export calls, want %d", got, wantReceived)
			}

			spanCtx, spanCancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer spanCancel()
			err = exporter.ExportSpans(spanCtx, tracetest.SpanStubs{{Name: "mtls-probe"}}.Snapshots())
			if (err == nil) != tc.wantSuccess {
				t.Fatalf("ExportSpans error = %v, want success=%t", err, tc.wantSuccess)
			}
			if got := receivedSpans.Load(); got != wantReceived {
				t.Fatalf("server received %d trace export calls, want %d", got, wantReceived)
			}
		})
	}
}
