/*
Copyright 2026 The Scion Authors.
*/

package logging

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

// bridgePKI is a synthetic CA with a server leaf for 127.0.0.1 and a client
// leaf, generated in-test. PEM files live only under the test's TempDir.
type bridgePKI struct {
	caFile, certFile, keyFile string
	caPool                    *x509.CertPool
	serverCert                tls.Certificate
}

func newBridgePKI(t *testing.T) *bridgePKI {
	t.Helper()
	dir := t.TempDir()
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "log-bridge-test-ca"},
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
	serverCertPEM, serverKeyPEM := leaf(2, "log-bridge-test-server", x509.ExtKeyUsageServerAuth, []net.IP{net.IPv4(127, 0, 0, 1)})
	serverCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	clientCertPEM, clientKeyPEM := leaf(3, "log-bridge-test-client", x509.ExtKeyUsageClientAuth, nil)

	p := &bridgePKI{
		caFile:     filepath.Join(dir, "ca.pem"),
		certFile:   filepath.Join(dir, "client.pem"),
		keyFile:    filepath.Join(dir, "client-key.pem"),
		caPool:     x509.NewCertPool(),
		serverCert: serverCert,
	}
	p.caPool.AddCert(caCert)
	for path, data := range map[string][]byte{
		p.caFile:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		p.certFile: clientCertPEM,
		p.keyFile:  clientKeyPEM,
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// exportCall is what the collector saw for one logs export request.
type exportCall struct {
	clientCN string
	headers  metadata.MD
}

type recordingLogsServer struct {
	collogspb.UnimplementedLogsServiceServer
	mu    sync.Mutex
	calls []exportCall
}

func (s *recordingLogsServer) Export(ctx context.Context, _ *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	var call exportCall
	if p, ok := peer.FromContext(ctx); ok {
		if info, ok := p.AuthInfo.(credentials.TLSInfo); ok && len(info.State.PeerCertificates) > 0 {
			call.clientCN = info.State.PeerCertificates[0].Subject.CommonName
		}
	}
	call.headers, _ = metadata.FromIncomingContext(ctx)
	s.mu.Lock()
	s.calls = append(s.calls, call)
	s.mu.Unlock()
	return &collogspb.ExportLogsServiceResponse{}, nil
}

func (s *recordingLogsServer) snapshot() []exportCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]exportCall(nil), s.calls...)
}

// startMTLSCollector starts an OTLP logs gRPC server that requires a client
// certificate signed by the test CA, and returns its address.
func startMTLSCollector(t *testing.T, pki *bridgePKI) (string, *recordingLogsServer) {
	t.Helper()
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
	rec := &recordingLogsServer{}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	collogspb.RegisterLogsServiceServer(server, rec)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener.Addr().String(), rec
}

// emitAndFlush emits one record through lp and flushes it to the collector.
func emitAndFlush(t *testing.T, lp otellog.LoggerProvider, timeout time.Duration) {
	t.Helper()
	sdkProvider, ok := lp.(*sdklog.LoggerProvider)
	if !ok {
		t.Fatalf("provider is %T, want *sdklog.LoggerProvider", lp)
	}
	var record otellog.Record
	record.SetBody(attribute.StringValue("log bridge TLS test"))
	lp.Logger("log-bridge-test").Emit(context.Background(), record)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_ = sdkProvider.ForceFlush(ctx)
}

// TestNewLoggerProvider_MutualTLS checks that the log bridge applies the
// configured CA and client certificate: an mTLS collector receives the
// record with the client certificate, and refuses it without one.
func TestNewLoggerProvider_MutualTLS(t *testing.T) {
	pki := newBridgePKI(t)
	for _, tc := range []struct {
		name      string
		cert, key string
		wantCN    string
	}{
		{"with client certificate", pki.certFile, pki.keyFile, "log-bridge-test-client"},
		{"without client certificate", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, rec := startMTLSCollector(t, pki)
			// The provider's context also bounds its shutdown in cleanup, so
			// an exporter still retrying a refused export cannot hang.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			lp, cleanup, err := NewLoggerProvider(ctx, OTelConfig{
				Endpoint: addr,
				CAFile:   pki.caFile,
				CertFile: tc.cert,
				KeyFile:  tc.key,
			})
			if err != nil {
				t.Fatalf("NewLoggerProvider: %v", err)
			}
			defer cleanup()

			timeout := 5 * time.Second
			if tc.wantCN == "" {
				timeout = 2 * time.Second
			}
			emitAndFlush(t, lp, timeout)

			calls := rec.snapshot()
			if tc.wantCN == "" {
				if len(calls) != 0 {
					t.Fatalf("collector accepted %d export(s) without a client certificate", len(calls))
				}
				return
			}
			if len(calls) == 0 {
				t.Fatal("collector received no export over mTLS")
			}
			if calls[0].clientCN != tc.wantCN {
				t.Fatalf("client certificate CN = %q, want %q", calls[0].clientCN, tc.wantCN)
			}
		})
	}
}

// TestInitOTelLogging_TLSAndHeadersFromEnv checks that the server log
// bridge reads the same SCION_OTEL_* TLS variables as the agent exporter,
// and applies SCION_OTEL_HEADERS to export requests.
func TestInitOTelLogging_TLSAndHeadersFromEnv(t *testing.T) {
	pki := newBridgePKI(t)
	addr, rec := startMTLSCollector(t, pki)
	t.Setenv(EnvOTelLogEnable, "true")
	t.Setenv(EnvOTelEndpoint, addr)
	t.Setenv(EnvOTelInsecure, "")
	t.Setenv(EnvOTelSkipTLSVerify, "")
	t.Setenv(EnvOTelCAFile, pki.caFile)
	t.Setenv(EnvOTelCertFile, pki.certFile)
	t.Setenv(EnvOTelKeyFile, pki.keyFile)
	t.Setenv(EnvOTelHeaders, `{"X-Scion-Test":"bridge-header-value"}`)

	lp, cleanup, err := InitOTelLogging(context.Background(), OTelConfig{})
	if err != nil {
		t.Fatalf("InitOTelLogging: %v", err)
	}
	if lp == nil {
		t.Fatal("expected a LoggerProvider")
	}
	defer cleanup()
	emitAndFlush(t, lp, 5*time.Second)

	calls := rec.snapshot()
	if len(calls) == 0 {
		t.Fatal("collector received no export")
	}
	if calls[0].clientCN != "log-bridge-test-client" {
		t.Fatalf("client certificate CN = %q, want the configured client certificate", calls[0].clientCN)
	}
	if got := calls[0].headers.Get("x-scion-test"); len(got) != 1 || got[0] != "bridge-header-value" {
		t.Fatalf("x-scion-test header = %v, want [bridge-header-value]", got)
	}
}

// TestNewLoggerProvider_TLSConfigErrors checks the same rejections as the
// agent exporter: plaintext with TLS options, and a half client pair.
func TestNewLoggerProvider_TLSConfigErrors(t *testing.T) {
	pki := newBridgePKI(t)
	for _, tc := range []struct {
		name      string
		config    OTelConfig
		wantError string
	}{
		{"plaintext with client certificate", OTelConfig{Endpoint: "127.0.0.1:4317", Insecure: true, CertFile: pki.certFile, KeyFile: pki.keyFile}, "conflicts"},
		{"plaintext with skip verify", OTelConfig{Endpoint: "127.0.0.1:4317", Insecure: true, SkipTLSVerify: true}, "conflicts"},
		{"certificate without key", OTelConfig{Endpoint: "127.0.0.1:4317", CertFile: pki.certFile}, "must be set together"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lp, _, err := NewLoggerProvider(context.Background(), tc.config)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantError)
			}
			if lp != nil {
				t.Fatal("expected no provider on error")
			}
		})
	}
}

// TestInitOTelLogging_BadHeadersIgnored checks that a malformed
// SCION_OTEL_HEADERS value drops the headers but keeps log export.
func TestInitOTelLogging_BadHeadersIgnored(t *testing.T) {
	t.Setenv(EnvOTelLogEnable, "true")
	t.Setenv(EnvOTelEndpoint, "127.0.0.1:4317")
	t.Setenv(EnvOTelInsecure, "true")
	t.Setenv(EnvOTelSkipTLSVerify, "")
	t.Setenv(EnvOTelCAFile, "")
	t.Setenv(EnvOTelCertFile, "")
	t.Setenv(EnvOTelKeyFile, "")
	t.Setenv(EnvOTelHeaders, "not-json")

	lp, cleanup, err := InitOTelLogging(context.Background(), OTelConfig{})
	if err != nil {
		t.Fatalf("InitOTelLogging: %v", err)
	}
	if lp == nil {
		t.Fatal("expected a LoggerProvider despite the malformed headers value")
	}
	cleanup()
}
