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
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	certsv1beta1 "k8s.io/api/certificates/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// testCA is a throwaway CA for TLS-source tests.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

var testSerial atomic.Int64

func newTestCA(t *testing.T, cn string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(testSerial.Add(1)),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns a leaf signed by ca, as a tls.Certificate and as a PEM
// credential bundle (certificate followed by key in one file).
func (ca *testCA) issue(t *testing.T, cn string, dnsNames []string, client bool) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	usage := x509.ExtKeyUsageServerAuth
	if client {
		usage = x509.ExtKeyUsageClientAuth
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(testSerial.Add(1)),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return pair, append(certPEM, keyPEM...)
}

func poolOf(cas ...*testCA) *x509.CertPool {
	p := x509.NewCertPool()
	for _, ca := range cas {
		p.AddCert(ca.cert)
	}
	return p
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func v1beta1CTB(name, signer string, labels map[string]string, bundle []byte) *certsv1beta1.ClusterTrustBundle {
	return &certsv1beta1.ClusterTrustBundle{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec:       certsv1beta1.ClusterTrustBundleSpec{SignerName: signer, TrustBundle: string(bundle)},
	}
}

var liveLabels = map[string]string{CanaryingLabel: CanaryingLive}

func mustConfig(t *testing.T, src TLSConfigFunc) *tls.Config {
	t.Helper()
	cfg, err := src(context.Background())
	if err != nil {
		t.Fatalf("TLS source error = %v", err)
	}
	return cfg
}

func TestServerTLSSource_CAFile(t *testing.T) {
	ca := newTestCA(t, "ca")
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	writeFile(t, caFile, ca.pem)

	src, err := NewServerTLSSource(context.Background(), nil, DialerConfig{APIEndpoint: "api.ate-system.svc:443", CAFile: caFile})
	if err != nil {
		t.Fatalf("NewServerTLSSource() error = %v", err)
	}
	a, b := mustConfig(t, src), mustConfig(t, src)
	if a == b {
		t.Error("TLS source returned the same *tls.Config twice; want a fresh config per call")
	}
	if a.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %#x, want TLS 1.3", a.MinVersion)
	}
	if a.ServerName != "api.ate-system.svc" {
		t.Errorf("ServerName = %q, want api.ate-system.svc", a.ServerName)
	}
	if a.InsecureSkipVerify { //nolint:gosec // explicitly asserting this is never set
		t.Error("InsecureSkipVerify must never be true")
	}
	if a.GetClientCertificate != nil {
		t.Error("GetClientCertificate set without a client cert file")
	}
	if !a.RootCAs.Equal(poolOf(ca)) {
		t.Error("RootCAs does not hold exactly the ca_file CA")
	}
}

func TestServerTLSSource_ServerNameOverride(t *testing.T) {
	ca := newTestCA(t, "ca")
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	writeFile(t, caFile, ca.pem)
	src, err := NewServerTLSSource(context.Background(), nil, DialerConfig{APIEndpoint: "dns:///10.0.0.1:443", CAFile: caFile, ServerName: "api.ate-system.svc"})
	if err != nil {
		t.Fatal(err)
	}
	if got := mustConfig(t, src).ServerName; got != "api.ate-system.svc" {
		t.Errorf("ServerName = %q, want the explicit override", got)
	}
}

func TestServerTLSSource_CAFileReloadsAfterRotation(t *testing.T) {
	ca1, ca2 := newTestCA(t, "ca1"), newTestCA(t, "ca2")
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	writeFile(t, caFile, ca1.pem)

	src, err := NewServerTLSSource(context.Background(), nil, DialerConfig{APIEndpoint: "api.ate-system.svc:443", CAFile: caFile})
	if err != nil {
		t.Fatal(err)
	}
	first := mustConfig(t, src)
	if !first.RootCAs.Equal(poolOf(ca1)) {
		t.Fatal("initial RootCAs is not ca1")
	}
	if again := mustConfig(t, src); again.RootCAs != first.RootCAs {
		t.Error("unchanged ca_file was re-parsed; want the cached pool reused")
	}

	writeFile(t, caFile, ca2.pem)
	if got := mustConfig(t, src).RootCAs; !got.Equal(poolOf(ca2)) {
		t.Error("RootCAs after rotation is not ca2")
	}

	// A broken file fails closed rather than silently keeping trust.
	writeFile(t, caFile, []byte("garbage"))
	if _, err := src(context.Background()); err == nil {
		t.Error("TLS source with an invalid ca_file = nil error, want one")
	}
}

func TestServerTLSSource_EmptyCAFileContents(t *testing.T) {
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	writeFile(t, caFile, []byte("not a certificate"))
	if _, err := NewServerTLSSource(context.Background(), nil, DialerConfig{APIEndpoint: "api.ate-system.svc:443", CAFile: caFile}); err == nil {
		t.Fatal("NewServerTLSSource() with a CA file holding no certificates = nil error, want one")
	}
}

func TestServerTLSSource_NoCASource(t *testing.T) {
	ctx := context.Background()
	cfg := DialerConfig{APIEndpoint: "api.ate-system.svc:443"}
	if _, err := NewServerTLSSource(ctx, nil, cfg); err == nil || !strings.Contains(err.Error(), "no CA source") {
		t.Errorf("NewServerTLSSource(nil client) error = %v, want a no-CA-source error", err)
	}
	if _, err := NewServerTLSSource(ctx, kubefake.NewClientset(), cfg); err == nil || !strings.Contains(err.Error(), ServiceDNSSignerName) {
		t.Errorf("NewServerTLSSource(no bundles) error = %v, want an error naming the signer", err)
	}
}

func TestServerTLSSource_NamedClusterTrustBundleV1beta1Fallback(t *testing.T) {
	// The fake clientset cannot serve raw v1 requests, so this exercises the
	// v1beta1 fallback for a bundle selected by name.
	ca := newTestCA(t, "named")
	client := kubefake.NewClientset(v1beta1CTB("ate-root", "example.com/other", nil, ca.pem))
	cfg := DialerConfig{APIEndpoint: "api.ate-system.svc:443", ClusterTrustBundle: "ate-root", CAFile: "/nonexistent/ca.pem"}
	src, err := NewServerTLSSource(context.Background(), client, cfg)
	if err != nil {
		t.Fatalf("NewServerTLSSource() error = %v, want the named bundle to take precedence over the unreadable ca_file", err)
	}
	if !mustConfig(t, src).RootCAs.Equal(poolOf(ca)) {
		t.Error("RootCAs is not the named bundle")
	}
}

func TestServerTLSSource_NamedClusterTrustBundleMissing(t *testing.T) {
	_, err := NewServerTLSSource(context.Background(), kubefake.NewClientset(), DialerConfig{APIEndpoint: "api.ate-system.svc:443", ClusterTrustBundle: "nope"})
	if err == nil || !strings.Contains(err.Error(), `"nope"`) {
		t.Errorf("error = %v, want one naming the missing bundle", err)
	}
}

func TestServerTLSSource_DefaultSelectionBySignerAndLabel(t *testing.T) {
	live1, live2 := newTestCA(t, "live1"), newTestCA(t, "live2")
	canary, other := newTestCA(t, "canary"), newTestCA(t, "other")
	client := kubefake.NewClientset(
		v1beta1CTB("servicedns.podcert.ate.dev:identity:primary-bundle", ServiceDNSSignerName, liveLabels, live1.pem),
		v1beta1CTB("servicedns.podcert.ate.dev:identity:secondary", ServiceDNSSignerName, liveLabels, live2.pem),
		v1beta1CTB("servicedns.podcert.ate.dev:identity:canary", ServiceDNSSignerName, map[string]string{CanaryingLabel: "canary"}, canary.pem),
		v1beta1CTB("other-signer", "example.com/other", liveLabels, other.pem),
	)
	src, err := NewServerTLSSource(context.Background(), client, DialerConfig{APIEndpoint: "api.ate-system.svc:443"})
	if err != nil {
		t.Fatalf("NewServerTLSSource() error = %v", err)
	}
	if !mustConfig(t, src).RootCAs.Equal(poolOf(live1, live2)) {
		t.Error("RootCAs is not exactly the two live servicedns bundles")
	}
}

func TestServerTLSSource_CAFileBeatsDefaultSelection(t *testing.T) {
	live, fileCA := newTestCA(t, "live"), newTestCA(t, "file")
	client := kubefake.NewClientset(v1beta1CTB("b", ServiceDNSSignerName, liveLabels, live.pem))
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	writeFile(t, caFile, fileCA.pem)
	src, err := NewServerTLSSource(context.Background(), client, DialerConfig{APIEndpoint: "api.ate-system.svc:443", CAFile: caFile})
	if err != nil {
		t.Fatal(err)
	}
	if !mustConfig(t, src).RootCAs.Equal(poolOf(fileCA)) {
		t.Error("RootCAs is not the ca_file CA")
	}
}

// fakeAPIServer serves ClusterTrustBundle lists for v1 and/or v1beta1.
type fakeAPIServer struct {
	mu        sync.Mutex
	serveV1   bool
	items     []ctbJSON
	v1Hits    int
	beta1Hits int
	selectors []string
}

func (f *fakeAPIServer) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.selectors = append(f.selectors, r.URL.Query().Get("labelSelector"))
	const (
		v1Prefix    = "/apis/certificates.k8s.io/v1/clustertrustbundles"
		beta1Prefix = "/apis/certificates.k8s.io/v1beta1/clustertrustbundles"
	)
	var apiVersion string
	var getName string
	switch {
	case r.URL.Path == v1Prefix:
		f.v1Hits++
		if !f.serveV1 {
			http.NotFound(w, r)
			return
		}
		apiVersion = "certificates.k8s.io/v1"
	case strings.HasPrefix(r.URL.Path, v1Prefix+"/"):
		f.v1Hits++
		if !f.serveV1 {
			http.NotFound(w, r)
			return
		}
		apiVersion = "certificates.k8s.io/v1"
		getName = strings.TrimPrefix(r.URL.Path, v1Prefix+"/")
	case r.URL.Path == beta1Prefix:
		f.beta1Hits++
		apiVersion = "certificates.k8s.io/v1beta1"
	case strings.HasPrefix(r.URL.Path, beta1Prefix+"/"):
		f.beta1Hits++
		apiVersion = "certificates.k8s.io/v1beta1"
		getName = strings.TrimPrefix(r.URL.Path, beta1Prefix+"/")
	default:
		http.NotFound(w, r)
		return
	}
	type item struct {
		ctbJSON
		Kind       string `json:"kind"`
		APIVersion string `json:"apiVersion"`
	}
	if getName != "" {
		for _, it := range f.items {
			if it.Metadata.Name == getName {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(item{ctbJSON: it, Kind: "ClusterTrustBundle", APIVersion: apiVersion})
				return
			}
		}
		http.NotFound(w, r)
		return
	}
	list := struct {
		Kind       string `json:"kind"`
		APIVersion string `json:"apiVersion"`
		Items      []item `json:"items"`
	}{Kind: "ClusterTrustBundleList", APIVersion: apiVersion}
	for _, it := range f.items {
		list.Items = append(list.Items, item{ctbJSON: it, Kind: "ClusterTrustBundle", APIVersion: apiVersion})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(list)
}

func ctbItem(name, signer string, labels map[string]string, bundle []byte) ctbJSON {
	var j ctbJSON
	j.Metadata.Name = name
	j.Metadata.Labels = labels
	j.Spec.SignerName = signer
	j.Spec.TrustBundle = string(bundle)
	return j
}

func newRealClient(t *testing.T, f *fakeAPIServer) kubernetes.Interface {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	client, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestServerTLSSource_DefaultSelectionPrefersV1(t *testing.T) {
	live, other := newTestCA(t, "live"), newTestCA(t, "other")
	f := &fakeAPIServer{serveV1: true, items: []ctbJSON{
		ctbItem("primary", ServiceDNSSignerName, liveLabels, live.pem),
		ctbItem("other", "example.com/other", liveLabels, other.pem),
	}}
	src, err := NewServerTLSSource(context.Background(), newRealClient(t, f), DialerConfig{APIEndpoint: "api.ate-system.svc:443"})
	if err != nil {
		t.Fatalf("NewServerTLSSource() error = %v", err)
	}
	if !mustConfig(t, src).RootCAs.Equal(poolOf(live)) {
		t.Error("RootCAs is not the live servicedns bundle")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.v1Hits == 0 || f.beta1Hits != 0 {
		t.Errorf("v1 hits = %d, v1beta1 hits = %d; want v1 only", f.v1Hits, f.beta1Hits)
	}
	if f.selectors[0] != CanaryingLabel+"="+CanaryingLive {
		t.Errorf("labelSelector = %q, want %s=%s", f.selectors[0], CanaryingLabel, CanaryingLive)
	}
}

func TestServerTLSSource_DefaultSelectionFallsBackToV1beta1(t *testing.T) {
	live := newTestCA(t, "live")
	f := &fakeAPIServer{serveV1: false, items: []ctbJSON{ctbItem("primary", ServiceDNSSignerName, liveLabels, live.pem)}}
	src, err := NewServerTLSSource(context.Background(), newRealClient(t, f), DialerConfig{APIEndpoint: "api.ate-system.svc:443"})
	if err != nil {
		t.Fatalf("NewServerTLSSource() error = %v", err)
	}
	if !mustConfig(t, src).RootCAs.Equal(poolOf(live)) {
		t.Error("RootCAs is not the live servicedns bundle")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.v1Hits == 0 || f.beta1Hits == 0 {
		t.Errorf("v1 hits = %d, v1beta1 hits = %d; want v1 tried, then v1beta1", f.v1Hits, f.beta1Hits)
	}
}

func TestServerTLSSource_NamedClusterTrustBundleV1AndFallbackAndInvalidName(t *testing.T) {
	ca := newTestCA(t, "named")

	t.Run("v1 hit", func(t *testing.T) {
		f := &fakeAPIServer{serveV1: true, items: []ctbJSON{ctbItem("ate-root", ServiceDNSSignerName, liveLabels, ca.pem)}}
		src, err := NewServerTLSSource(context.Background(), newRealClient(t, f), DialerConfig{
			APIEndpoint:        "api.ate-system.svc:443",
			ClusterTrustBundle: "ate-root",
		})
		if err != nil {
			t.Fatalf("NewServerTLSSource() error = %v", err)
		}
		if !mustConfig(t, src).RootCAs.Equal(poolOf(ca)) {
			t.Error("RootCAs is not the named v1 bundle")
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.v1Hits == 0 || f.beta1Hits != 0 {
			t.Errorf("v1 hits = %d, v1beta1 hits = %d; want v1 only", f.v1Hits, f.beta1Hits)
		}
	})

	t.Run("v1 404 falls back to v1beta1", func(t *testing.T) {
		f := &fakeAPIServer{serveV1: false, items: []ctbJSON{ctbItem("ate-root", ServiceDNSSignerName, liveLabels, ca.pem)}}
		src, err := NewServerTLSSource(context.Background(), newRealClient(t, f), DialerConfig{
			APIEndpoint:        "api.ate-system.svc:443",
			ClusterTrustBundle: "ate-root",
		})
		if err != nil {
			t.Fatalf("NewServerTLSSource() error = %v", err)
		}
		if !mustConfig(t, src).RootCAs.Equal(poolOf(ca)) {
			t.Error("RootCAs is not the named v1beta1 fallback bundle")
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.v1Hits == 0 || f.beta1Hits == 0 {
			t.Errorf("v1 hits = %d, v1beta1 hits = %d; want v1 tried then v1beta1", f.v1Hits, f.beta1Hits)
		}
	})

	t.Run("invalid path segment name rejected before hitting server", func(t *testing.T) {
		f := &fakeAPIServer{serveV1: true, items: []ctbJSON{ctbItem("ate-root", ServiceDNSSignerName, liveLabels, ca.pem)}}
		_, err := NewServerTLSSource(context.Background(), newRealClient(t, f), DialerConfig{
			APIEndpoint:        "api.ate-system.svc:443",
			ClusterTrustBundle: "../secrets",
		})
		if err == nil || !strings.Contains(err.Error(), "invalid resource name") {
			t.Errorf("NewServerTLSSource(traversal name) error = %v, want invalid resource name error", err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.v1Hits != 0 || f.beta1Hits != 0 {
			t.Errorf("traversal name reached API server (v1=%d, v1beta1=%d), want 0 hits", f.v1Hits, f.beta1Hits)
		}
	})
}

func TestCTBRoots_RefreshAfterTTLAndStaleOnError(t *testing.T) {
	ca1, ca2 := newTestCA(t, "ca1"), newTestCA(t, "ca2")
	client := kubefake.NewClientset(v1beta1CTB("b", ServiceDNSSignerName, liveLabels, ca1.pem))
	var clockMu sync.Mutex
	now := time.Unix(1_000_000, 0)
	lister := &switchableLister{inner: newCTBLister(client)}
	roots := &ctbRoots{lister: lister, now: func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}}
	advance := func(d time.Duration) {
		clockMu.Lock()
		now = now.Add(d)
		clockMu.Unlock()
	}
	ctx := context.Background()

	p, err := roots.pool(ctx)
	if err != nil || !p.Equal(poolOf(ca1)) {
		t.Fatalf("initial pool err = %v, equal ca1 = %v", err, err == nil && p.Equal(poolOf(ca1)))
	}

	// Rotate the bundle in the cluster.
	if _, err := client.CertificatesV1beta1().ClusterTrustBundles().Update(ctx, v1beta1CTB("b", ServiceDNSSignerName, liveLabels, ca2.pem), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	advance(ctbCacheTTL / 2)
	if p, _ := roots.pool(ctx); !p.Equal(poolOf(ca1)) {
		t.Error("pool refreshed before the cache TTL elapsed")
	}
	advance(ctbCacheTTL)
	if p, _ := roots.pool(ctx); !p.Equal(poolOf(ca2)) {
		t.Error("pool not refreshed to the rotated bundle after the cache TTL")
	}

	// API errors keep the last good pool for up to ctbMaxStale and back off
	// for ctbCacheTTL so repeated handshakes do not hammer an unreachable
	// apiserver.
	lister.setErr(errors.New("apiserver unavailable"))
	advance(ctbCacheTTL + time.Second)
	if p, err := roots.pool(ctx); err != nil || !p.Equal(poolOf(ca2)) {
		t.Errorf("pool during a refresh failure: err = %v; want the last good pool", err)
	}
	callsAfterFailure := lister.callCount()
	if p, err := roots.pool(ctx); err != nil || !p.Equal(poolOf(ca2)) {
		t.Errorf("immediate repeat call during outage: err = %v; want cached stale pool", err)
	}
	advance(ctbCacheTTL / 2)
	if p, err := roots.pool(ctx); err != nil || !p.Equal(poolOf(ca2)) {
		t.Errorf("mid-backoff call during outage: err = %v; want cached stale pool", err)
	}
	if got := lister.callCount(); got != callsAfterFailure {
		t.Errorf("lister called %d times during ctbCacheTTL outage backoff, want %d (no extra calls)", got, callsAfterFailure)
	}

	advance(ctbMaxStale)
	if _, err := roots.pool(ctx); err == nil {
		t.Error("pool past ctbMaxStale with refreshes failing = nil error, want one")
	}
}

func TestCTBRoots_ConcurrentCallerRespectsOwnContext(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var closeOnce sync.Once
	releaseNow := func() { closeOnce.Do(func() { close(release) }) }
	defer releaseNow()

	ca := newTestCA(t, "ca")
	lister := &blockingLister{
		started: started,
		release: release,
		bundles: []trustBundle{{Name: "b", SignerName: ServiceDNSSignerName, Labels: liveLabels, TrustBundle: string(ca.pem)}},
	}
	roots := &ctbRoots{lister: lister, now: time.Now}

	caller1Done := make(chan error, 1)
	go func() {
		_, err := roots.pool(context.Background())
		caller1Done <- err
	}()
	<-started

	// A second caller whose context cancels promptly must return immediately
	// rather than blocking behind caller 1's stalled apiserver RPC.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	start := time.Now()
	_, err := roots.pool(ctx2)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("caller 2 error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("caller 2 waited %v, want prompt return on ctx cancel", elapsed)
	}

	releaseNow()
	if err := <-caller1Done; err != nil {
		t.Fatalf("caller 1 error = %v, want nil once apiserver responds", err)
	}
}

type blockingLister struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	bundles []trustBundle
}

func (b *blockingLister) get(ctx context.Context, _ string) (trustBundle, error) {
	b.once.Do(func() { close(b.started) })
	select {
	case <-b.release:
		return b.bundles[0], nil
	case <-ctx.Done():
		return trustBundle{}, ctx.Err()
	}
}

func (b *blockingLister) list(ctx context.Context, _ string) ([]trustBundle, error) {
	b.once.Do(func() { close(b.started) })
	select {
	case <-b.release:
		return b.bundles, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type switchableLister struct {
	inner ctbLister
	mu    sync.Mutex
	err   error
	calls atomic.Int32
}

func (s *switchableLister) setErr(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

func (s *switchableLister) callCount() int32 {
	return s.calls.Load()
}

func (s *switchableLister) get(ctx context.Context, name string) (trustBundle, error) {
	s.calls.Add(1)
	s.mu.Lock()
	err := s.err
	s.mu.Unlock()
	if err != nil {
		return trustBundle{}, err
	}
	return s.inner.get(ctx, name)
}

func (s *switchableLister) list(ctx context.Context, sel string) ([]trustBundle, error) {
	s.calls.Add(1)
	s.mu.Lock()
	err := s.err
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.inner.list(ctx, sel)
}

// startTLSServer runs a TLS 1.2+ server that requires a client certificate
// signed by clientCA and reports, per connection, the negotiated version
// and the client certificate's common name.
func startTLSServer(t *testing.T, serverCert tls.Certificate, clientCA *testCA) (addr string, seen <-chan [2]string) {
	t.Helper()
	ch := make(chan [2]string, 8)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    poolOf(clientCA),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				tc := c.(*tls.Conn)
				if err := tc.Handshake(); err != nil {
					ch <- [2]string{"error", err.Error()}
					return
				}
				st := tc.ConnectionState()
				cn := ""
				if len(st.PeerCertificates) > 0 {
					cn = st.PeerCertificates[0].Subject.CommonName
				}
				ver := "other"
				if st.Version == tls.VersionTLS13 {
					ver = "1.3"
				}
				ch <- [2]string{ver, cn}
				_, _ = io.Copy(io.Discard, c)
			}(c)
		}
	}()
	return ln.Addr().String(), ch
}

func TestRouterTLSSource_HandshakeTLS13AndClientCertReReadPerHandshake(t *testing.T) {
	serverCA, clientCA := newTestCA(t, "servicedns"), newTestCA(t, "podidentity")
	serverCert, _ := serverCA.issue(t, "router", []string{"atenet-router.ate-system.svc"}, false)
	addr, seen := startTLSServer(t, serverCert, clientCA)

	dir := t.TempDir()
	caFile := filepath.Join(dir, "router-ca.pem")
	writeFile(t, caFile, serverCA.pem)
	bundle := filepath.Join(dir, "credential-bundle.pem")
	_, b1 := clientCA.issue(t, "client-1", nil, true)
	writeFile(t, bundle, b1)

	sc := &config.V1SubstrateConfig{
		RouterEndpoint:       "https://atenet-router.ate-system.svc",
		RouterCAFile:         caFile,
		RouterClientCertFile: bundle,
		CAFile:               "/nonexistent/ignored-because-router_ca_file-wins.pem",
	}
	src, err := NewRouterTLSSource(context.Background(), nil, sc)
	if err != nil {
		t.Fatalf("NewRouterTLSSource() error = %v", err)
	}

	handshake := func() [2]string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		conn, err := src.DialTLSContext(ctx, "tcp", addr)
		if err != nil {
			t.Fatalf("DialTLSContext() error = %v", err)
		}
		defer func() { _ = conn.Close() }()
		if v := conn.(*tls.Conn).ConnectionState().Version; v != tls.VersionTLS13 {
			t.Errorf("negotiated version = %#x, want TLS 1.3", v)
		}
		select {
		case got := <-seen:
			return got
		case <-ctx.Done():
			t.Fatal("server did not report the handshake")
		}
		return [2]string{}
	}

	if got := handshake(); got != [2]string{"1.3", "client-1"} {
		t.Errorf("first handshake = %v, want TLS 1.3 with client-1", got)
	}
	_, b2 := clientCA.issue(t, "client-2", nil, true)
	writeFile(t, bundle, b2)
	if got := handshake(); got != [2]string{"1.3", "client-2"} {
		t.Errorf("second handshake = %v, want the rotated client-2 certificate", got)
	}
}

func TestRouterTLSSource_RejectsUntrustedServer(t *testing.T) {
	serverCA, otherCA, clientCA := newTestCA(t, "servicedns"), newTestCA(t, "other"), newTestCA(t, "podidentity")
	serverCert, _ := serverCA.issue(t, "router", []string{"atenet-router.ate-system.svc"}, false)
	addr, _ := startTLSServer(t, serverCert, clientCA)

	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	writeFile(t, caFile, otherCA.pem)
	bundle := filepath.Join(dir, "bundle.pem")
	_, b := clientCA.issue(t, "client", nil, true)
	writeFile(t, bundle, b)

	src, err := NewRouterTLSSource(context.Background(), nil, &config.V1SubstrateConfig{
		RouterEndpoint: "https://atenet-router.ate-system.svc", CAFile: caFile, RouterClientCertFile: bundle,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if conn, err := src.DialTLSContext(ctx, "tcp", addr); err == nil {
		_ = conn.Close()
		t.Fatal("handshake against a server signed by an untrusted CA succeeded")
	}
}

func TestRouterTLSSource_Defaults(t *testing.T) {
	ca, clientCA := newTestCA(t, "ca"), newTestCA(t, "podidentity")
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	writeFile(t, caFile, ca.pem)
	bundle := filepath.Join(dir, "credential-bundle.pem")
	_, b := clientCA.issue(t, "client", nil, true)
	writeFile(t, bundle, b)
	ctx := context.Background()

	// The default bundle path does not exist in the test environment; eager
	// validation must fail at construction naming DefaultRouterClientCertFile.
	if _, err := NewRouterTLSSource(ctx, nil, &config.V1SubstrateConfig{
		RouterEndpoint: "https://atenet-router.ate-system.svc",
		CAFile:         caFile,
	}); err == nil || !strings.Contains(err.Error(), DefaultRouterClientCertFile) {
		t.Fatalf("NewRouterTLSSource(missing default client cert) error = %v, want eager error naming %s", err, DefaultRouterClientCertFile)
	}

	src, err := NewRouterTLSSource(ctx, nil, &config.V1SubstrateConfig{
		RouterEndpoint:       "https://atenet-router.ate-system.svc",
		CAFile:               caFile,
		RouterClientCertFile: bundle,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := mustConfig(t, src)
	if cfg.ServerName != "atenet-router.ate-system.svc" {
		t.Errorf("ServerName = %q, want the router_endpoint host", cfg.ServerName)
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %#x, want TLS 1.3", cfg.MinVersion)
	}
	if cfg.GetClientCertificate == nil {
		t.Fatal("router TLS has no client certificate; mTLS is required")
	}

	src, err = NewRouterTLSSource(ctx, nil, &config.V1SubstrateConfig{
		RouterEndpoint:       "https://10.0.0.5",
		RouterServerName:     "atenet-router.ate-system.svc",
		CAFile:               caFile,
		RouterClientCertFile: bundle,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := mustConfig(t, src).ServerName; got != "atenet-router.ate-system.svc" {
		t.Errorf("ServerName = %q, want router_server_name", got)
	}

	if _, err := NewRouterTLSSource(ctx, nil, &config.V1SubstrateConfig{CAFile: caFile, RouterClientCertFile: bundle}); err == nil {
		t.Error("NewRouterTLSSource() with no router endpoint or server name = nil error, want one")
	}
	if _, err := NewRouterTLSSource(ctx, nil, nil); err == nil {
		t.Error("NewRouterTLSSource(nil) = nil error, want one")
	}
}

func TestRouterTLSSource_RouterCAFileErrorNamesField(t *testing.T) {
	dir := t.TempDir()
	badRouterCA := filepath.Join(dir, "bad-router-ca.pem")
	writeFile(t, badRouterCA, []byte("not-a-cert"))
	clientCA := newTestCA(t, "podidentity")
	bundle := filepath.Join(dir, "bundle.pem")
	_, b := clientCA.issue(t, "client", nil, true)
	writeFile(t, bundle, b)

	_, err := NewRouterTLSSource(context.Background(), nil, &config.V1SubstrateConfig{
		RouterEndpoint:       "https://atenet-router.ate-system.svc",
		RouterCAFile:         badRouterCA,
		RouterClientCertFile: bundle,
	})
	if err == nil || !strings.Contains(err.Error(), "router_ca_file") {
		t.Errorf("NewRouterTLSSource(invalid router_ca_file) error = %v, want error naming router_ca_file", err)
	}
}

func TestRouterTLSSource_FallsBackToClusterTrustBundles(t *testing.T) {
	live, clientCA := newTestCA(t, "live"), newTestCA(t, "podidentity")
	bundle := filepath.Join(t.TempDir(), "bundle.pem")
	_, b := clientCA.issue(t, "client", nil, true)
	writeFile(t, bundle, b)
	client := kubefake.NewClientset(v1beta1CTB("b", ServiceDNSSignerName, liveLabels, live.pem))
	src, err := NewRouterTLSSource(context.Background(), client, &config.V1SubstrateConfig{
		RouterEndpoint:       "https://atenet-router.ate-system.svc",
		RouterClientCertFile: bundle,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !mustConfig(t, src).RootCAs.Equal(poolOf(live)) {
		t.Error("router RootCAs is not the live servicedns bundle")
	}
}

func TestEndpointHost(t *testing.T) {
	cases := map[string]string{
		"":                                            "",
		"api.ate-system.svc:443":                      "api.ate-system.svc",
		"api.ate-system.svc":                          "api.ate-system.svc",
		"dns:///api.ate-system.svc:443":               "api.ate-system.svc",
		"dns://10.0.0.10/api.ate-system.svc:443":      "api.ate-system.svc",
		"https://atenet-router.ate-system.svc":        "atenet-router.ate-system.svc",
		"https://atenet-router.ate-system.svc:8443/x": "atenet-router.ate-system.svc",
		"atenet-router.ate-system.svc:8444":           "atenet-router.ate-system.svc",
	}
	for in, want := range cases {
		if got := endpointHost(in); got != want {
			t.Errorf("endpointHost(%q) = %q, want %q", in, got, want)
		}
	}
}
