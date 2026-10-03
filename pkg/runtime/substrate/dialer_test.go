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
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	certsv1beta1 "k8s.io/api/certificates/v1beta1"
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

func TestServerTLSConfig_CAFile(t *testing.T) {
	caPEM := generateTestCAPEM(t)
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caFile, caPEM, 0644); err != nil {
		t.Fatal(err)
	}

	client := kubefake.NewClientset()
	cfg := DialerConfig{APIEndpoint: "api.ate-system.svc:443", CAFile: caFile}

	tlsCfg, err := serverTLSConfig(context.Background(), client, cfg)
	if err != nil {
		t.Fatalf("serverTLSConfig() error = %v", err)
	}
	if tlsCfg.RootCAs == nil {
		t.Fatal("serverTLSConfig() RootCAs is nil")
	}
	if tlsCfg.ServerName != "api.ate-system.svc" {
		t.Errorf("ServerName = %q, want api.ate-system.svc", tlsCfg.ServerName)
	}
	if tlsCfg.InsecureSkipVerify { //nolint:staticcheck // explicitly asserting this is never set
		t.Error("InsecureSkipVerify must never be true")
	}
}

func TestServerTLSConfig_ClusterTrustBundle(t *testing.T) {
	caPEM := generateTestCAPEM(t)
	client := kubefake.NewClientset(&certsv1beta1.ClusterTrustBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "ate-root"},
		Spec:       certsv1beta1.ClusterTrustBundleSpec{TrustBundle: string(caPEM)},
	})

	cfg := DialerConfig{APIEndpoint: "api.ate-system.svc:443", ClusterTrustBundle: "ate-root"}
	tlsCfg, err := serverTLSConfig(context.Background(), client, cfg)
	if err != nil {
		t.Fatalf("serverTLSConfig() error = %v", err)
	}
	if tlsCfg.RootCAs == nil {
		t.Fatal("serverTLSConfig() RootCAs is nil")
	}
}

func TestServerTLSConfig_ClusterTrustBundlePreferredOverCAFile(t *testing.T) {
	// When both are set, ClusterTrustBundle wins. Give the CAFile a path
	// that does not exist — if the function fell back to CAFile it would
	// fail to read it, so success here proves ClusterTrustBundle was used.
	caPEM := generateTestCAPEM(t)
	client := kubefake.NewClientset(&certsv1beta1.ClusterTrustBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "ate-root"},
		Spec:       certsv1beta1.ClusterTrustBundleSpec{TrustBundle: string(caPEM)},
	})

	cfg := DialerConfig{
		APIEndpoint:        "api.ate-system.svc:443",
		ClusterTrustBundle: "ate-root",
		CAFile:             "/nonexistent/ca.pem",
	}
	if _, err := serverTLSConfig(context.Background(), client, cfg); err != nil {
		t.Fatalf("serverTLSConfig() error = %v, want ClusterTrustBundle to take precedence over the unreadable CAFile", err)
	}
}

func TestServerTLSConfig_NeitherConfigured(t *testing.T) {
	client := kubefake.NewClientset()
	cfg := DialerConfig{APIEndpoint: "api.ate-system.svc:443"}
	_, err := serverTLSConfig(context.Background(), client, cfg)
	if err == nil {
		t.Fatal("serverTLSConfig() expected an error when no CA source is configured, got nil")
	}
}

func TestServerTLSConfig_EmptyCAFileContents(t *testing.T) {
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caFile, []byte("not a certificate"), 0644); err != nil {
		t.Fatal(err)
	}
	client := kubefake.NewClientset()
	cfg := DialerConfig{APIEndpoint: "api.ate-system.svc:443", CAFile: caFile}
	if _, err := serverTLSConfig(context.Background(), client, cfg); err == nil {
		t.Fatal("serverTLSConfig() expected an error for a CA file with no valid certificates, got nil")
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
