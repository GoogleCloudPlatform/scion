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

package substrate

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
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/peer"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// generateTestCAPEM returns a freshly generated, self-signed CA certificate
// in PEM form, for tests that need CA material but must never use a real
// credential.
func generateTestCAPEM(t *testing.T) []byte {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestHostOnly(t *testing.T) {
	cases := map[string]string{
		"api.ate-system.svc:443":   "api.ate-system.svc",
		"atenet-router.ate-system": "atenet-router.ate-system",
		"127.0.0.1:8443":           "127.0.0.1",
		"[::1]:443":                "::1",
	}
	for in, want := range cases {
		if got := hostOnly(in); got != want {
			t.Errorf("hostOnly(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDialTarget(t *testing.T) {
	cases := map[string]string{
		"api.ate-system.svc:443":                 "dns:///api.ate-system.svc:443",
		" api.ate-system.svc:443 ":               "dns:///api.ate-system.svc:443",
		"dns:///api.ate-system.svc:443":          "dns:///api.ate-system.svc:443",
		"dns://10.0.0.10/api.ate-system.svc:443": "dns://10.0.0.10/api.ate-system.svc:443",
		"passthrough:///api.ate-system.svc:443":  "passthrough:///api.ate-system.svc:443",
		"127.0.0.1:8443":                         "dns:///127.0.0.1:8443",
	}
	for in, want := range cases {
		if got := dialTarget(in); got != want {
			t.Errorf("dialTarget(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDial_UsesDNSTargetAndCAFile(t *testing.T) {
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, generateTestCAPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := DialerConfig{
		APIEndpoint:        "api.ate-system.svc:443",
		CAFile:             caFile,
		Namespace:          "scion",
		ServiceAccountName: "scion-substrate-broker",
	}
	conn, err := Dial(context.Background(), kubefake.NewClientset(), cfg)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer func() { _ = conn.Close() }()
	if got, want := conn.Target(), "dns:///api.ate-system.svc:443"; got != want {
		t.Errorf("conn.Target() = %q, want %q", got, want)
	}
}

func TestDial_DoesNotDoublePrefixScheme(t *testing.T) {
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, generateTestCAPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := DialerConfig{
		APIEndpoint:        "dns:///api.ate-system.svc:443",
		CAFile:             caFile,
		Namespace:          "scion",
		ServiceAccountName: "scion-substrate-broker",
	}
	conn, err := Dial(context.Background(), kubefake.NewClientset(), cfg)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer func() { _ = conn.Close() }()
	if got, want := conn.Target(), "dns:///api.ate-system.svc:443"; got != want {
		t.Errorf("conn.Target() = %q, want %q", got, want)
	}
}

func TestDial_RequiresEndpointAndCA(t *testing.T) {
	if _, err := Dial(context.Background(), kubefake.NewClientset(), DialerConfig{}); err == nil {
		t.Error("Dial() with no api_endpoint = nil error, want one")
	}
	// No CA file, no named bundle, and no matching ClusterTrustBundle in
	// the cluster: there is nothing to verify ateapi against.
	cfg := DialerConfig{APIEndpoint: "api.ate-system.svc:443", Namespace: "scion", ServiceAccountName: "broker"}
	if _, err := Dial(context.Background(), kubefake.NewClientset(), cfg); err == nil {
		t.Error("Dial() with no CA source = nil error, want one")
	}
}

// fixedClock returns a func() time.Time pinned to t, for deterministic
// expiry/refresh tests.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func TestTokenSource_CachesUntilNearExpiry(t *testing.T) {
	now := time.Now()
	calls := 0
	client := kubefake.NewClientset()
	client.PrependReactor("create", "serviceaccounts", func(action clienttesting.Action) (bool, k8sruntime.Object, error) {
		if action.GetSubresource() != "token" {
			return false, nil, nil
		}
		calls++
		return true, &authv1.TokenRequest{
			Status: authv1.TokenRequestStatus{
				Token:               "tok-1",
				ExpirationTimestamp: metav1.NewTime(now.Add(time.Hour)),
			},
		}, nil
	})

	ts := &tokenSource{client: client, audience: DefaultTokenAudience, namespace: "ate-broker", saName: "scion-broker", now: fixedClock(now)}

	for i := 0; i < 3; i++ {
		tok, err := ts.token(context.Background())
		if err != nil {
			t.Fatalf("token() error = %v", err)
		}
		if tok != "tok-1" {
			t.Errorf("token() = %q, want tok-1", tok)
		}
	}
	if calls != 1 {
		t.Errorf("CreateToken called %d times, want 1 (cached)", calls)
	}
}

func TestTokenSource_RefreshesBeforeExpiry(t *testing.T) {
	now := time.Now()
	tokenNum := 0
	client := kubefake.NewClientset()
	client.PrependReactor("create", "serviceaccounts", func(action clienttesting.Action) (bool, k8sruntime.Object, error) {
		if action.GetSubresource() != "token" {
			return false, nil, nil
		}
		tokenNum++
		return true, &authv1.TokenRequest{
			Status: authv1.TokenRequestStatus{
				Token: tokenName(tokenNum),
				// Expires in exactly tokenRefreshSkew from "now" at mint
				// time, so it's already within the refresh window.
				ExpirationTimestamp: metav1.NewTime(now.Add(tokenRefreshSkew)),
			},
		}, nil
	})

	ts := &tokenSource{client: client, audience: DefaultTokenAudience, namespace: "ate-broker", saName: "scion-broker", now: fixedClock(now)}

	first, err := ts.token(context.Background())
	if err != nil {
		t.Fatalf("token() error = %v", err)
	}
	second, err := ts.token(context.Background())
	if err != nil {
		t.Fatalf("token() error = %v", err)
	}
	if first == second {
		t.Errorf("token() returned the same token (%q) when the cached one was within the refresh skew of expiry", first)
	}
	if tokenNum != 2 {
		t.Errorf("CreateToken called %d times, want 2 (refreshed)", tokenNum)
	}
}

func TestTokenSource_EmptyTokenIsError(t *testing.T) {
	client := kubefake.NewClientset()
	client.PrependReactor("create", "serviceaccounts", func(action clienttesting.Action) (bool, k8sruntime.Object, error) {
		if action.GetSubresource() != "token" {
			return false, nil, nil
		}
		return true, &authv1.TokenRequest{}, nil
	})

	ts := &tokenSource{client: client, audience: DefaultTokenAudience, namespace: "ate-broker", saName: "scion-broker", now: time.Now}
	if _, err := ts.token(context.Background()); err == nil {
		t.Fatal("token() expected an error for an empty minted token, got nil")
	}
}

func tokenName(n int) string {
	return "tok-" + string(rune('0'+n))
}

// TestSourceCredentials_GRPCHandshakePerConnection drives a real gRPC TLS
// handshake through sourceCredentials: TLS 1.3 against a server whose cert
// chains to the source's CA, with the source consulted on each new
// connection (so a rotated CA reaches new connections).
func TestSourceCredentials_GRPCHandshakePerConnection(t *testing.T) {
	ca := newTestCA(t, "servicedns")
	serverCert, _ := ca.issue(t, "api", []string{"api.ate-system.svc"}, false)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{serverCert},
	})))
	healthpb.RegisterHealthServer(srv, health.NewServer())
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	caFile := filepath.Join(t.TempDir(), "ca.pem")
	writeFile(t, caFile, ca.pem)
	base, err := NewServerTLSSource(context.Background(), nil, DialerConfig{APIEndpoint: "api.ate-system.svc:443", CAFile: caFile})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	src := TLSConfigFunc(func(ctx context.Context) (*tls.Config, error) {
		calls.Add(1)
		return base(ctx)
	})

	check := func() {
		t.Helper()
		conn, err := grpc.NewClient("passthrough:///"+lis.Addr().String(),
			grpc.WithTransportCredentials(newSourceCredentials(src)))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
			t.Fatalf("Health.Check over sourceCredentials: %v", err)
		}
	}
	check()
	check()
	if got := calls.Load(); got < 2 {
		t.Errorf("TLS source consulted %d times for 2 connections, want once per handshake", got)
	}

	// A client trusting a different CA must fail the handshake.
	otherFile := filepath.Join(t.TempDir(), "other.pem")
	writeFile(t, otherFile, newTestCA(t, "other").pem)
	other, err := NewServerTLSSource(context.Background(), nil, DialerConfig{APIEndpoint: "api.ate-system.svc:443", CAFile: otherFile})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient("passthrough:///"+lis.Addr().String(), grpc.WithTransportCredentials(newSourceCredentials(other)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{}); err == nil {
		t.Error("Health.Check against a server signed by an untrusted CA succeeded")
	}
}

// TestSourceCredentials_ClientCertMTLS verifies that DialerConfig.ClientCertFile
// presents the client certificate during gRPC TLS 1.3 mTLS handshakes and
// reloads it when rotated on disk.
func TestSourceCredentials_ClientCertMTLS(t *testing.T) {
	serverCA, clientCA := newTestCA(t, "servicedns"), newTestCA(t, "podidentity")
	serverCert, _ := serverCA.issue(t, "api", []string{"api.ate-system.svc"}, false)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	seenCN := make(chan string, 4)
	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(&tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{serverCert},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    poolOf(clientCA),
		})),
		grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			if p, ok := peer.FromContext(ctx); ok {
				if ti, ok := p.AuthInfo.(credentials.TLSInfo); ok && len(ti.State.PeerCertificates) > 0 {
					seenCN <- ti.State.PeerCertificates[0].Subject.CommonName
				}
			}
			return handler(ctx, req)
		}),
	)
	healthpb.RegisterHealthServer(srv, health.NewServer())
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	writeFile(t, caFile, serverCA.pem)
	bundleFile := filepath.Join(dir, "client.pem")
	_, b1 := clientCA.issue(t, "broker-v1", nil, true)
	writeFile(t, bundleFile, b1)

	src, err := NewServerTLSSource(context.Background(), nil, DialerConfig{
		APIEndpoint:    "api.ate-system.svc:443",
		CAFile:         caFile,
		ClientCertFile: bundleFile,
	})
	if err != nil {
		t.Fatalf("NewServerTLSSource() error = %v", err)
	}

	callAndReadCN := func() string {
		t.Helper()
		conn, err := grpc.NewClient("passthrough:///"+lis.Addr().String(),
			grpc.WithTransportCredentials(newSourceCredentials(src)))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
			t.Fatalf("Health.Check over mTLS: %v", err)
		}
		return <-seenCN
	}

	if got := callAndReadCN(); got != "broker-v1" {
		t.Errorf("first handshake client CN = %q, want broker-v1", got)
	}
	_, b2 := clientCA.issue(t, "broker-v2", nil, true)
	writeFile(t, bundleFile, b2)
	if got := callAndReadCN(); got != "broker-v2" {
		t.Errorf("second handshake client CN = %q, want rotated broker-v2", got)
	}
}
