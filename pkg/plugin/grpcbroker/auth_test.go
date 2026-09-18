// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package grpcbroker

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin/refbroker"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	brokerv1 "github.com/GoogleCloudPlatform/scion/proto/broker/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// --- Test helpers ---

// mockTokenSource is a test TokenSource that returns a fixed token.
type mockTokenSource struct {
	mu     sync.Mutex
	token  string
	expiry time.Time
	err    error
}

func newMockTokenSource(token string) *mockTokenSource {
	return &mockTokenSource{
		token:  token,
		expiry: time.Now().Add(time.Hour),
	}
}

func (m *mockTokenSource) Token() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return "", m.err
	}
	return m.token, nil
}

func (m *mockTokenSource) SetToken(token string, expiry time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.token = token
	m.expiry = expiry
}

func (m *mockTokenSource) Expiry() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.expiry
}

// startAuthenticatedTestServer starts a gRPC server with a token validator.
func startAuthenticatedTestServer(t *testing.T, impl *refbroker.RefBroker, validator TokenValidator, opts ...grpc.ServerOption) (string, func()) {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	allOpts := append([]grpc.ServerOption{
		grpc.UnaryInterceptor(UnaryAuthInterceptor(validator)),
		grpc.StreamInterceptor(StreamAuthInterceptor(validator)),
	}, opts...)

	s := grpc.NewServer(allOpts...)
	brokerv1.RegisterBrokerServiceServer(s, NewServer(impl))

	go func() { _ = s.Serve(lis) }()

	return lis.Addr().String(), func() { s.GracefulStop() }
}

// acceptTokenValidator accepts a specific token and rejects everything else.
func acceptTokenValidator(validToken string) TokenValidator {
	return TokenValidatorFunc(func(_ context.Context, token string) error {
		if token != validToken {
			return status.Errorf(codes.PermissionDenied, "invalid token")
		}
		return nil
	})
}

// --- TokenSourceCredentials tests ---

func TestTokenSourceCredentials_GetRequestMetadata(t *testing.T) {
	src := newMockTokenSource("test-token-123")
	creds := NewTokenSourceCredentials(src, true)

	md, err := creds.GetRequestMetadata(context.Background(), "some-uri")
	require.NoError(t, err)
	assert.Equal(t, "Bearer test-token-123", md["authorization"])
}

func TestTokenSourceCredentials_RequireTransportSecurity(t *testing.T) {
	src := newMockTokenSource("tok")
	assert.True(t, NewTokenSourceCredentials(src, true).RequireTransportSecurity())
	assert.False(t, NewTokenSourceCredentials(src, false).RequireTransportSecurity())
}

func TestTokenSourceCredentials_TokenError(t *testing.T) {
	src := newMockTokenSource("")
	src.err = fmt.Errorf("token refresh failed")
	creds := NewTokenSourceCredentials(src, false)

	_, err := creds.GetRequestMetadata(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token fetch")
}

func TestTokenSourceCredentials_TokenRefresh(t *testing.T) {
	src := newMockTokenSource("token-v1")
	creds := NewTokenSourceCredentials(src, false)

	md, err := creds.GetRequestMetadata(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "Bearer token-v1", md["authorization"])

	// Simulate token rotation.
	src.SetToken("token-v2", time.Now().Add(time.Hour))

	md, err = creds.GetRequestMetadata(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "Bearer token-v2", md["authorization"])
}

// --- Server-side auth interceptor tests ---

func TestAuthInterceptor_Authorized(t *testing.T) {
	broker := refbroker.New(slog.Default())
	defer func() { _ = broker.Close() }()

	validToken := "valid-hub-token-xyz"
	addr, stop := startAuthenticatedTestServer(t, broker, acceptTokenValidator(validToken))
	defer stop()

	// Client with valid credentials.
	src := newMockTokenSource(validToken)
	creds := NewTokenSourceCredentials(src, false)

	adapter := NewGRPCBrokerAdapter(AdapterConfig{
		Address:       addr,
		Logger:        slog.Default(),
		Authenticator: creds,
	})
	defer func() { _ = adapter.Close() }()

	// All control methods should succeed.
	info, err := adapter.GetInfo()
	require.NoError(t, err)
	assert.Equal(t, "refbroker", info.Name)

	health, err := adapter.HealthCheck()
	require.NoError(t, err)
	assert.Equal(t, "healthy", health.Status)

	err = adapter.Configure(map[string]string{"hub_url": "http://localhost:8080"})
	require.NoError(t, err)

	err = adapter.Publish(context.Background(), "test.topic",
		messages.NewInstruction("user:alice", "agent:coder", "hello"))
	require.NoError(t, err)
}

func TestAuthInterceptor_Unauthorized_NoToken(t *testing.T) {
	broker := refbroker.New(slog.Default())
	defer func() { _ = broker.Close() }()

	addr, stop := startAuthenticatedTestServer(t, broker, acceptTokenValidator("valid-token"))
	defer stop()

	// Client without any credentials.
	adapter := NewGRPCBrokerAdapter(AdapterConfig{
		Address: addr,
		Logger:  slog.Default(),
	})
	defer func() { _ = adapter.Close() }()

	// All methods should fail with Unauthenticated.
	_, err := adapter.GetInfo()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unauthenticated, st.Code())

	// HealthCheck swallows transport errors and returns degraded status.
	health, err := adapter.HealthCheck()
	require.NoError(t, err)
	assert.Equal(t, "unknown", health.Status)
	assert.Contains(t, health.Message, "health check failed")

	err = adapter.Configure(map[string]string{})
	require.Error(t, err)
}

func TestAuthInterceptor_Unauthorized_WrongToken(t *testing.T) {
	broker := refbroker.New(slog.Default())
	defer func() { _ = broker.Close() }()

	addr, stop := startAuthenticatedTestServer(t, broker, acceptTokenValidator("correct-token"))
	defer stop()

	// Client with wrong credentials.
	src := newMockTokenSource("wrong-token")
	creds := NewTokenSourceCredentials(src, false)

	adapter := NewGRPCBrokerAdapter(AdapterConfig{
		Address:       addr,
		Logger:        slog.Default(),
		Authenticator: creds,
	})
	defer func() { _ = adapter.Close() }()

	_, err := adapter.GetInfo()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.PermissionDenied, st.Code())
}

func TestAuthInterceptor_TokenRotation(t *testing.T) {
	broker := refbroker.New(slog.Default())
	defer func() { _ = broker.Close() }()

	// Validator that accepts any token starting with "valid-".
	validator := TokenValidatorFunc(func(_ context.Context, token string) error {
		if len(token) < 6 || token[:6] != "valid-" {
			return status.Errorf(codes.PermissionDenied, "invalid token")
		}
		return nil
	})

	addr, stop := startAuthenticatedTestServer(t, broker, validator)
	defer stop()

	src := newMockTokenSource("valid-v1")
	creds := NewTokenSourceCredentials(src, false)

	adapter := NewGRPCBrokerAdapter(AdapterConfig{
		Address:       addr,
		Logger:        slog.Default(),
		Authenticator: creds,
	})
	defer func() { _ = adapter.Close() }()

	// First call with v1 token.
	_, err := adapter.GetInfo()
	require.NoError(t, err)

	// Rotate token.
	src.SetToken("valid-v2", time.Now().Add(time.Hour))

	// Second call should use v2 token seamlessly.
	_, err = adapter.GetInfo()
	require.NoError(t, err)

	// Rotate to invalid token — should fail.
	src.SetToken("expired-abc", time.Now().Add(time.Hour))

	_, err = adapter.GetInfo()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.PermissionDenied, st.Code())
}

// --- Factory/config path tests ---

func TestNewAdapterFromEntry_NoAuth(t *testing.T) {
	broker := refbroker.New(slog.Default())
	defer func() { _ = broker.Close() }()

	addr, stop := startTestServer(t, broker)
	defer stop()

	// No auth fields → backward compatible, no authenticator.
	entry := plugin.PluginEntry{
		Address: addr,
		Mode:    "grpc",
	}

	client, err := NewAdapterFromEntry(entry, slog.Default())
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	info, err := client.GetInfo()
	require.NoError(t, err)
	assert.Equal(t, "refbroker", info.Name)
}

func TestNewAdapterFromEntry_GoogleIDToken_RequiresAudience(t *testing.T) {
	entry := plugin.PluginEntry{
		Address:  "example.com:443",
		Mode:     "grpc",
		AuthType: "google_id_token",
		// AuthAudience intentionally omitted.
	}

	_, err := NewAdapterFromEntry(entry, slog.Default())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth_audience")
}

func TestNewAdapterFromEntry_UnsupportedAuthType(t *testing.T) {
	entry := plugin.PluginEntry{
		Address:  "example.com:443",
		Mode:     "grpc",
		AuthType: "kerberos",
	}

	_, err := NewAdapterFromEntry(entry, slog.Default())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported auth_type")
}

func TestNewAdapterFromEntry_GoogleIDToken_WithMockSource(t *testing.T) {
	broker := refbroker.New(slog.Default())
	defer func() { _ = broker.Close() }()

	validToken := "mock-id-token-for-test"
	addr, stop := startAuthenticatedTestServer(t, broker, acceptTokenValidator(validToken))
	defer stop()

	// Override the isOnGCE and adcSourceNew for testing.
	origIsOnGCE := isOnGCE
	origADC := adcSourceNew
	defer func() {
		isOnGCE = origIsOnGCE
		adcSourceNew = origADC
	}()

	isOnGCE = func() bool { return false }
	adcSourceNew = func(audience string) (transportauth.TokenSource, error) {
		return newMockTokenSource(validToken), nil
	}

	entry := plugin.PluginEntry{
		Address:      addr,
		Mode:         "grpc",
		AuthType:     AuthTypeGoogleIDToken,
		AuthAudience: "https://bridge.example.com",
	}

	client, err := NewAdapterFromEntry(entry, slog.Default())
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	info, err := client.GetInfo()
	require.NoError(t, err)
	assert.Equal(t, "refbroker", info.Name)
}

func TestNewAdapterFromEntry_GoogleIDToken_NoCredentialSource(t *testing.T) {
	origIsOnGCE := isOnGCE
	origADC := adcSourceNew
	defer func() {
		isOnGCE = origIsOnGCE
		adcSourceNew = origADC
	}()

	isOnGCE = func() bool { return false }
	adcSourceNew = nil

	entry := plugin.PluginEntry{
		Address:      "bridge.example.com:443",
		Mode:         "grpc",
		AuthType:     AuthTypeGoogleIDToken,
		AuthAudience: "https://bridge.example.com",
	}

	_, err := NewAdapterFromEntry(entry, slog.Default())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no credential source available")
}

// --- ServerOptions tests ---

func TestServerOptions_WithValidator(t *testing.T) {
	opts, err := ServerOptions(ServerAuthConfig{
		Validator: acceptTokenValidator("test"),
	})
	require.NoError(t, err)
	// Should have unary + stream interceptors.
	assert.Len(t, opts, 2)
}

func TestServerOptions_Empty(t *testing.T) {
	opts, err := ServerOptions(ServerAuthConfig{})
	require.NoError(t, err)
	assert.Empty(t, opts)
}

// --- TLS tests ---

// testPKI generates a CA, server cert, and client cert for TLS tests.
type testPKI struct {
	caDir       string
	caCertFile  string
	serverCert  string
	serverKey   string
	clientCert  string
	clientKey   string
	wrongCACert string
	wrongCAKey  string
}

func generateTestPKI(t *testing.T) *testPKI {
	t.Helper()
	dir := t.TempDir()

	// Generate CA.
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	caCertFile := filepath.Join(dir, "ca.pem")
	writePEM(t, caCertFile, "CERTIFICATE", caDER)

	// Generate server cert signed by CA.
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serverTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTmpl, caTmpl, &serverKey.PublicKey, caKey)
	require.NoError(t, err)
	serverCertFile := filepath.Join(dir, "server.pem")
	serverKeyFile := filepath.Join(dir, "server-key.pem")
	writePEM(t, serverCertFile, "CERTIFICATE", serverDER)
	writeKeyPEM(t, serverKeyFile, serverKey)

	// Generate client cert signed by CA.
	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	clientTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "hub-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTmpl, caTmpl, &clientKey.PublicKey, caKey)
	require.NoError(t, err)
	clientCertFile := filepath.Join(dir, "client.pem")
	clientKeyFile := filepath.Join(dir, "client-key.pem")
	writePEM(t, clientCertFile, "CERTIFICATE", clientDER)
	writeKeyPEM(t, clientKeyFile, clientKey)

	// Generate a different CA for "wrong CA" tests.
	wrongCAKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	wrongCATmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(100),
		Subject:               pkix.Name{CommonName: "Wrong CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	wrongCADER, err := x509.CreateCertificate(rand.Reader, wrongCATmpl, wrongCATmpl, &wrongCAKey.PublicKey, wrongCAKey)
	require.NoError(t, err)
	wrongCACertFile := filepath.Join(dir, "wrong-ca.pem")
	writePEM(t, wrongCACertFile, "CERTIFICATE", wrongCADER)

	// Generate a client cert signed by the wrong CA.
	wrongClientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	wrongClientTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(101),
		Subject:      pkix.Name{CommonName: "wrong-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	wrongClientDER, err := x509.CreateCertificate(rand.Reader, wrongClientTmpl, wrongCATmpl, &wrongClientKey.PublicKey, wrongCAKey)
	require.NoError(t, err)
	wrongClientCertFile := filepath.Join(dir, "wrong-client.pem")
	wrongClientKeyFile := filepath.Join(dir, "wrong-client-key.pem")
	writePEM(t, wrongClientCertFile, "CERTIFICATE", wrongClientDER)
	writeKeyPEM(t, wrongClientKeyFile, wrongClientKey)

	return &testPKI{
		caDir:       dir,
		caCertFile:  caCertFile,
		serverCert:  serverCertFile,
		serverKey:   serverKeyFile,
		clientCert:  clientCertFile,
		clientKey:   clientKeyFile,
		wrongCACert: wrongCACertFile,
		wrongCAKey:  wrongClientKeyFile,
	}
}

func writePEM(t *testing.T, path, pemType string, der []byte) {
	t.Helper()
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()
	require.NoError(t, pem.Encode(f, &pem.Block{Type: pemType, Bytes: der}))
}

func writeKeyPEM(t *testing.T, path string, key *ecdsa.PrivateKey) {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	writePEM(t, path, "EC PRIVATE KEY", der)
}

func TestTLS_ServerClientCA_ValidClient(t *testing.T) {
	pki := generateTestPKI(t)
	broker := refbroker.New(slog.Default())
	defer func() { _ = broker.Close() }()

	// Server with TLS + mTLS (client CA verification).
	serverOpts, err := ServerOptions(ServerAuthConfig{
		TLSCertFile:     pki.serverCert,
		TLSKeyFile:      pki.serverKey,
		TLSClientCAFile: pki.caCertFile,
	})
	require.NoError(t, err)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	s := grpc.NewServer(serverOpts...)
	brokerv1.RegisterBrokerServiceServer(s, NewServer(broker))
	go func() { _ = s.Serve(lis) }()
	defer s.GracefulStop()

	// Client with valid client cert and correct CA.
	adapter := NewGRPCBrokerAdapter(AdapterConfig{
		Address: lis.Addr().String(),
		TLS: &TLSConfig{
			CertFile: pki.clientCert,
			KeyFile:  pki.clientKey,
			CAFile:   pki.caCertFile,
		},
		Logger: slog.Default(),
	})
	defer func() { _ = adapter.Close() }()

	info, err := adapter.GetInfo()
	require.NoError(t, err)
	assert.Equal(t, "refbroker", info.Name)
}

func TestTLS_WrongCA_FailsClosed(t *testing.T) {
	pki := generateTestPKI(t)
	broker := refbroker.New(slog.Default())
	defer func() { _ = broker.Close() }()

	// Server with TLS.
	serverOpts, err := ServerOptions(ServerAuthConfig{
		TLSCertFile: pki.serverCert,
		TLSKeyFile:  pki.serverKey,
	})
	require.NoError(t, err)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	s := grpc.NewServer(serverOpts...)
	brokerv1.RegisterBrokerServiceServer(s, NewServer(broker))
	go func() { _ = s.Serve(lis) }()
	defer s.GracefulStop()

	// Client with wrong CA — should fail.
	adapter := NewGRPCBrokerAdapter(AdapterConfig{
		Address: lis.Addr().String(),
		TLS: &TLSConfig{
			CAFile: pki.wrongCACert,
		},
		Logger: slog.Default(),
	})
	defer func() { _ = adapter.Close() }()

	_, err = adapter.GetInfo()
	require.Error(t, err, "connection with wrong CA should fail")
}

func TestTLS_WrongClientCert_FailsClosed(t *testing.T) {
	pki := generateTestPKI(t)
	broker := refbroker.New(slog.Default())
	defer func() { _ = broker.Close() }()

	// Server with mTLS — requires client cert from our CA.
	serverOpts, err := ServerOptions(ServerAuthConfig{
		TLSCertFile:     pki.serverCert,
		TLSKeyFile:      pki.serverKey,
		TLSClientCAFile: pki.caCertFile,
	})
	require.NoError(t, err)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	s := grpc.NewServer(serverOpts...)
	brokerv1.RegisterBrokerServiceServer(s, NewServer(broker))
	go func() { _ = s.Serve(lis) }()
	defer s.GracefulStop()

	// Client with cert from wrong CA — the server should reject it.
	// Load the wrong client cert manually for the dialer.
	wrongCert, err := tls.LoadX509KeyPair(
		filepath.Join(pki.caDir, "wrong-client.pem"),
		filepath.Join(pki.caDir, "wrong-client-key.pem"),
	)
	require.NoError(t, err)

	caPool := x509.NewCertPool()
	caCert, err := os.ReadFile(pki.caCertFile)
	require.NoError(t, err)
	caPool.AppendCertsFromPEM(caCert)

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{wrongCert},
			RootCAs:      caPool,
		})),
	)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	client := brokerv1.NewBrokerServiceClient(conn)
	_, err = client.GetInfo(context.Background(), &brokerv1.GetInfoRequest{})
	require.Error(t, err, "connection with wrong client cert should fail")
}

func TestTLS_NoClientCert_WhenRequired_FailsClosed(t *testing.T) {
	pki := generateTestPKI(t)
	broker := refbroker.New(slog.Default())
	defer func() { _ = broker.Close() }()

	// Server requires mTLS.
	serverOpts, err := ServerOptions(ServerAuthConfig{
		TLSCertFile:     pki.serverCert,
		TLSKeyFile:      pki.serverKey,
		TLSClientCAFile: pki.caCertFile,
	})
	require.NoError(t, err)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	s := grpc.NewServer(serverOpts...)
	brokerv1.RegisterBrokerServiceServer(s, NewServer(broker))
	go func() { _ = s.Serve(lis) }()
	defer s.GracefulStop()

	// Client with correct CA but no client cert.
	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			RootCAs: func() *x509.CertPool {
				pool := x509.NewCertPool()
				caCert, _ := os.ReadFile(pki.caCertFile)
				pool.AppendCertsFromPEM(caCert)
				return pool
			}(),
		})),
	)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	client := brokerv1.NewBrokerServiceClient(conn)
	_, err = client.GetInfo(context.Background(), &brokerv1.GetInfoRequest{})
	require.Error(t, err, "connection without client cert when mTLS required should fail")
}

// --- Combined auth + TLS test ---

func TestTLS_WithAuth_EndToEnd(t *testing.T) {
	pki := generateTestPKI(t)
	broker := refbroker.New(slog.Default())
	defer func() { _ = broker.Close() }()

	validToken := "hub-service-token"

	// Server with TLS + auth interceptor.
	serverOpts, err := ServerOptions(ServerAuthConfig{
		Validator:   acceptTokenValidator(validToken),
		TLSCertFile: pki.serverCert,
		TLSKeyFile:  pki.serverKey,
	})
	require.NoError(t, err)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	s := grpc.NewServer(serverOpts...)
	brokerv1.RegisterBrokerServiceServer(s, NewServer(broker))
	go func() { _ = s.Serve(lis) }()
	defer s.GracefulStop()

	// Authorized client with correct CA and token.
	src := newMockTokenSource(validToken)
	creds := NewTokenSourceCredentials(src, true)

	adapter := NewGRPCBrokerAdapter(AdapterConfig{
		Address: lis.Addr().String(),
		TLS: &TLSConfig{
			CAFile: pki.caCertFile,
		},
		Logger:        slog.Default(),
		Authenticator: creds,
	})
	defer func() { _ = adapter.Close() }()

	info, err := adapter.GetInfo()
	require.NoError(t, err)
	assert.Equal(t, "refbroker", info.Name)

	// Unauthorized client — has TLS but wrong token.
	wrongSrc := newMockTokenSource("wrong-token")
	wrongCreds := NewTokenSourceCredentials(wrongSrc, true)

	badAdapter := NewGRPCBrokerAdapter(AdapterConfig{
		Address: lis.Addr().String(),
		TLS: &TLSConfig{
			CAFile: pki.caCertFile,
		},
		Logger:        slog.Default(),
		Authenticator: wrongCreds,
	})
	defer func() { _ = badAdapter.Close() }()

	_, err = badAdapter.GetInfo()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.PermissionDenied, st.Code())
}

// --- Reconnect with auth test ---

func TestAuthInterceptor_Reconnect_PreservesAuth(t *testing.T) {
	broker1 := refbroker.New(slog.Default())

	validToken := "reconnect-token"
	addr1, stop1 := startAuthenticatedTestServer(t, broker1, acceptTokenValidator(validToken))

	src := newMockTokenSource(validToken)
	creds := NewTokenSourceCredentials(src, false)

	adapter := NewGRPCBrokerAdapter(AdapterConfig{
		Address:       addr1,
		Logger:        slog.Default(),
		Authenticator: creds,
	})
	defer func() { _ = adapter.Close() }()

	// Initial call succeeds.
	handler := func(_ context.Context, _ string, _ *messages.StructuredMessage) {}
	_, err := adapter.Subscribe("test.>", handler)
	require.NoError(t, err)

	// Stop first server.
	stop1()
	_ = broker1.Close()

	// Disconnect adapter.
	adapter.mu.Lock()
	if adapter.conn != nil {
		_ = adapter.conn.Close()
		adapter.conn = nil
		adapter.client = nil
	}
	adapter.mu.Unlock()

	// Start new server on different port.
	broker2 := refbroker.New(slog.Default())
	defer func() { _ = broker2.Close() }()

	addr2, stop2 := startAuthenticatedTestServer(t, broker2, acceptTokenValidator(validToken))
	defer stop2()

	// Point adapter to new server.
	adapter.mu.Lock()
	adapter.address = addr2
	adapter.mu.Unlock()

	// Reconnect should work with existing credentials.
	info, err := adapter.GetInfo()
	require.NoError(t, err)
	assert.Equal(t, "refbroker", info.Name)
}

// --- extractBearerToken tests ---

func TestExtractBearerToken_ValidToken(t *testing.T) {
	md := metadata.Pairs("authorization", "Bearer my-token")
	ctx := metadata.NewIncomingContext(context.Background(), md)

	token, err := extractBearerToken(ctx)
	require.NoError(t, err)
	assert.Equal(t, "my-token", token)
}

func TestExtractBearerToken_MissingMetadata(t *testing.T) {
	_, err := extractBearerToken(context.Background())
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.Unauthenticated, st.Code())
}

func TestExtractBearerToken_MissingAuthHeader(t *testing.T) {
	md := metadata.Pairs("other-header", "value")
	ctx := metadata.NewIncomingContext(context.Background(), md)

	_, err := extractBearerToken(ctx)
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.Unauthenticated, st.Code())
}

func TestExtractBearerToken_WrongFormat(t *testing.T) {
	md := metadata.Pairs("authorization", "Basic dXNlcjpwYXNz")
	ctx := metadata.NewIncomingContext(context.Background(), md)

	_, err := extractBearerToken(ctx)
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.Unauthenticated, st.Code())
}

func TestExtractBearerToken_EmptyBearer(t *testing.T) {
	md := metadata.Pairs("authorization", "Bearer ")
	ctx := metadata.NewIncomingContext(context.Background(), md)

	_, err := extractBearerToken(ctx)
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.Unauthenticated, st.Code())
}

// --- All control methods protected tests ---

func TestAuthInterceptor_AllMethodsProtected(t *testing.T) {
	broker := refbroker.New(slog.Default())
	defer func() { _ = broker.Close() }()

	validToken := "all-methods-token"
	addr, stop := startAuthenticatedTestServer(t, broker, acceptTokenValidator(validToken))
	defer stop()

	// Unauthenticated raw client.
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	client := brokerv1.NewBrokerServiceClient(conn)

	ctx := context.Background()

	methods := []struct {
		name string
		call func() error
	}{
		{"Configure", func() error {
			_, err := client.Configure(ctx, &brokerv1.ConfigureRequest{Config: map[string]string{}})
			return err
		}},
		{"Publish", func() error {
			_, err := client.Publish(ctx, &brokerv1.PublishRequest{
				Topic:   "test",
				Message: &brokerv1.StructuredMessage{Version: 1, Msg: "test"},
			})
			return err
		}},
		{"GetInfo", func() error {
			_, err := client.GetInfo(ctx, &brokerv1.GetInfoRequest{})
			return err
		}},
		{"HealthCheck", func() error {
			_, err := client.HealthCheck(ctx, &brokerv1.HealthCheckRequest{})
			return err
		}},
		{"Subscribe", func() error {
			_, err := client.Subscribe(ctx, &brokerv1.SubscribeRequest{Pattern: "test.>"})
			return err
		}},
		{"Unsubscribe", func() error {
			_, err := client.Unsubscribe(ctx, &brokerv1.UnsubscribeRequest{Pattern: "test.>"})
			return err
		}},
	}

	for _, m := range methods {
		t.Run(m.name, func(t *testing.T) {
			err := m.call()
			require.Error(t, err, "%s should be denied without auth", m.name)
			st, ok := status.FromError(err)
			require.True(t, ok)
			assert.Equal(t, codes.Unauthenticated, st.Code(),
				"%s should return Unauthenticated, got %s", m.name, st.Code())
		})
	}
}
