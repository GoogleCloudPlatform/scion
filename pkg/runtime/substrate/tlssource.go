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
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// DefaultRouterClientCertFile is the projected podidentity credential bundle
// (certificate chain and private key) presented to atenet-router for mTLS.
const DefaultRouterClientCertFile = "/run/podidentity.podcert.ate.dev/credential-bundle.pem"

// RouterClientCertFile returns the effective router client credential
// bundle path.
func RouterClientCertFile(sc *config.V1SubstrateConfig) string {
	if sc != nil && sc.RouterClientCertFile != "" {
		return sc.RouterClientCertFile
	}
	return DefaultRouterClientCertFile
}

// ServiceDNSSignerName is the signer of the ClusterTrustBundles that verify
// Substrate v1.0.0 serving certificates (ateapi and atenet-router).
const ServiceDNSSignerName = "servicedns.podcert.ate.dev/identity"

// CanaryingLabel and CanaryingLive select the live (non-canary) trust
// bundles among those published for ServiceDNSSignerName.
const (
	CanaryingLabel = "podcert.ate.dev/canarying"
	CanaryingLive  = "live"
)

// ctbCacheTTL is how long a ClusterTrustBundle-derived root pool is reused
// before it is fetched again, so a rotated bundle reaches new handshakes
// within this window. A package variable so tests can shorten it.
var ctbCacheTTL = 30 * time.Second

// ctbMaxStale bounds how long the last successfully fetched bundle keeps
// serving handshakes while refreshes fail (an API-server outage must not
// break every new ateapi connection at once, but must not pin an old CA
// forever either).
var ctbMaxStale = time.Hour

// ctbFetchTimeout bounds a single ClusterTrustBundle refresh RPC so a stuck
// apiserver cannot wedge a background refresh goroutine indefinitely.
var ctbFetchTimeout = 15 * time.Second

// TLSConfigFunc returns the client *tls.Config for one new connection. Each
// call returns a fresh config, so CA rotation applies to every handshake
// started after it; callers must not cache the result across connections.
type TLSConfigFunc func(ctx context.Context) (*tls.Config, error)

// DialTLSContext dials addr and completes a TLS handshake with a config
// obtained from f. It matches http.Transport.DialTLSContext. No NextProtos
// are set unless the source sets them, so the connection speaks HTTP/1.1.
func (f TLSConfigFunc) DialTLSContext(ctx context.Context, network, addr string) (net.Conn, error) {
	cfg, err := f(ctx)
	if err != nil {
		return nil, err
	}
	d := &tls.Dialer{Config: cfg}
	return d.DialContext(ctx, network, addr)
}

// tlsSourceOptions configures newTLSSource.
type tlsSourceOptions struct {
	// serverName is sent as SNI and verified against the serving cert.
	serverName string
	// caFile is a PEM CA bundle, re-read when its content changes.
	caFile string
	// caField names the config field supplying caFile ("ca_file" or
	// "router_ca_file") for error messages.
	caField string
	// clusterTrustBundle names one ClusterTrustBundle. Takes precedence
	// over caFile.
	clusterTrustBundle string
	// clientCertFile is an optional PEM credential bundle (certificate
	// chain and private key in one file), re-read on every handshake.
	clientCertFile string
	// what names the peer in error messages ("ateapi", "atenet-router").
	what string
}

// NewServerTLSSource returns the TLS source used to verify ateapi's serving
// certificate. Root CAs come from, in order of precedence:
//
//  1. cfg.ClusterTrustBundle, by name (certificates.k8s.io/v1, falling back
//     to v1beta1);
//  2. cfg.CAFile, re-read when its content changes;
//  3. every ClusterTrustBundle with signer ServiceDNSSignerName and label
//     CanaryingLabel=CanaryingLive (v1, falling back to v1beta1), pooled.
//
// ServerName is cfg.ServerName, else the host of cfg.APIEndpoint. When
// cfg.ClientCertFile is set it is presented as the client certificate. The
// source (and client certificate bundle, when set) is exercised once here, so
// a configuration that yields no CA certificates or an unreadable client cert
// fails at construction. InsecureSkipVerify is never set.
// k8s may be nil only when cfg.CAFile is set and cfg.ClusterTrustBundle is
// not.
func NewServerTLSSource(ctx context.Context, k8s kubernetes.Interface, cfg DialerConfig) (TLSConfigFunc, error) {
	serverName := cfg.ServerName
	if serverName == "" {
		serverName = endpointHost(cfg.APIEndpoint)
	}
	return newTLSSource(ctx, k8s, tlsSourceOptions{
		serverName:         serverName,
		caFile:             cfg.CAFile,
		caField:            "ca_file",
		clusterTrustBundle: cfg.ClusterTrustBundle,
		clientCertFile:     cfg.ClientCertFile,
		what:               "ateapi",
	})
}

// NewRouterTLSSource returns the mTLS source for atenet-router. Root CAs
// come from router_ca_file when set, otherwise from the same sources as
// ateapi (cluster_trust_bundle, ca_file, then the default
// ClusterTrustBundle selection; see NewServerTLSSource). The client
// certificate is RouterClientCertFile(sc), validated eagerly at construction
// and re-read on every handshake (pod certificates live at most 24h).
// ServerName is router_server_name, else the host of router_endpoint, else
// the host of router_connect_endpoint.
func NewRouterTLSSource(ctx context.Context, k8s kubernetes.Interface, sc *config.V1SubstrateConfig) (TLSConfigFunc, error) {
	if sc == nil {
		return nil, errors.New("substrate: router TLS: no substrate configuration")
	}
	serverName := sc.RouterServerName
	if serverName == "" {
		serverName = endpointHost(sc.RouterEndpoint)
	}
	if serverName == "" {
		serverName = endpointHost(sc.RouterConnectEndpoint)
	}
	opts := tlsSourceOptions{
		serverName:     serverName,
		clientCertFile: RouterClientCertFile(sc),
		what:           "atenet-router",
	}
	if sc.RouterCAFile != "" {
		opts.caFile = sc.RouterCAFile
		opts.caField = "router_ca_file"
	} else {
		opts.caFile = sc.CAFile
		opts.caField = "ca_file"
		opts.clusterTrustBundle = sc.ClusterTrustBundle
	}
	return newTLSSource(ctx, k8s, opts)
}

func newTLSSource(ctx context.Context, k8s kubernetes.Interface, opts tlsSourceOptions) (TLSConfigFunc, error) {
	if opts.serverName == "" {
		return nil, fmt.Errorf("substrate: %s TLS: cannot determine the server name; set the endpoint or an explicit server name", opts.what)
	}
	var roots rootSource
	switch {
	case opts.clusterTrustBundle != "":
		if k8s == nil {
			return nil, fmt.Errorf("substrate: %s TLS: cluster_trust_bundle %q needs a Kubernetes client", opts.what, opts.clusterTrustBundle)
		}
		roots = &ctbRoots{lister: newCTBLister(k8s), name: opts.clusterTrustBundle, now: time.Now}
	case opts.caFile != "":
		roots = &fileRoots{path: opts.caFile, field: opts.caField}
	default:
		if k8s == nil {
			return nil, fmt.Errorf("substrate: %s TLS: no CA source configured (set ca_file or cluster_trust_bundle, or run in-cluster to select ClusterTrustBundles signed by %s); InsecureSkipVerify is not supported", opts.what, ServiceDNSSignerName)
		}
		roots = &ctbRoots{lister: newCTBLister(k8s), now: time.Now}
	}
	src := &tlsSource{opts: opts, roots: roots}
	if _, err := src.roots.pool(ctx); err != nil {
		return nil, fmt.Errorf("substrate: %s TLS: %w", opts.what, err)
	}
	if opts.clientCertFile != "" {
		if _, err := loadCredentialBundle(opts.clientCertFile, opts.what); err != nil {
			return nil, err
		}
	}
	return src.config, nil
}

// tlsSource builds a fresh *tls.Config per call from its root source.
type tlsSource struct {
	opts  tlsSourceOptions
	roots rootSource
}

func (s *tlsSource) config(ctx context.Context) (*tls.Config, error) {
	pool, err := s.roots.pool(ctx)
	if err != nil {
		return nil, fmt.Errorf("substrate: %s TLS: %w", s.opts.what, err)
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: s.opts.serverName,
		RootCAs:    pool,
	}
	if path := s.opts.clientCertFile; path != "" {
		what := s.opts.what
		cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return loadCredentialBundle(path, what)
		}
	}
	return cfg, nil
}

// loadCredentialBundle reads a PEM file holding a certificate chain and its
// private key (the podcert "credential-bundle.pem" layout).
func loadCredentialBundle(path, what string) (*tls.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("substrate: %s client certificate: read %q: %w", what, path, err)
	}
	cert, err := tls.X509KeyPair(data, data)
	if err != nil {
		return nil, fmt.Errorf("substrate: %s client certificate: parse %q (want certificate chain and private key in one PEM file): %w", what, path, err)
	}
	return &cert, nil
}

// rootSource yields the current root CA pool. Implementations cache and
// return a pool that is never mutated after it is returned.
type rootSource interface {
	pool(ctx context.Context) (*x509.CertPool, error)
}

// fileRoots re-reads a PEM CA file on every call and re-parses it only when
// its content changed, so an atomically swapped projected volume (or an
// operator-rotated file) applies to the next handshake.
type fileRoots struct {
	path  string
	field string

	mu     sync.Mutex
	raw    []byte
	cached *x509.CertPool
}

func (f *fileRoots) fieldName() string {
	if f.field != "" {
		return f.field
	}
	return "ca_file"
}

func (f *fileRoots) pool(context.Context) (*x509.CertPool, error) {
	data, err := os.ReadFile(f.path)
	if err != nil {
		return nil, fmt.Errorf("read %s %q: %w", f.fieldName(), f.path, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cached != nil && bytes.Equal(data, f.raw) {
		return f.cached, nil
	}
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("%s %q contains no valid certificates", f.fieldName(), f.path)
	}
	f.raw = data
	f.cached = p
	return p, nil
}

// trustBundle is the version-independent subset of a ClusterTrustBundle.
type trustBundle struct {
	Name        string
	SignerName  string
	Labels      map[string]string
	TrustBundle string
}

// ctbLister reads ClusterTrustBundles from one API version.
type ctbLister interface {
	get(ctx context.Context, name string) (trustBundle, error)
	list(ctx context.Context, labelSelector string) ([]trustBundle, error)
}

// ctbRefresh represents one in-flight ClusterTrustBundle refresh shared
// across concurrent callers so handshakes never hold ctbRoots.mu across a
// network RPC and remain individually cancellable via their own ctx.
type ctbRefresh struct {
	done chan struct{}
	pool *x509.CertPool
	err  error
}

// ctbRoots builds a root pool from ClusterTrustBundles: the one named, or
// (name empty) every bundle with signer ServiceDNSSignerName and label
// CanaryingLabel=CanaryingLive. The pool is cached for ctbCacheTTL; a
// failed refresh backs off for ctbCacheTTL while serving the last good pool
// for up to ctbMaxStale.
type ctbRoots struct {
	lister ctbLister
	name   string
	now    func() time.Time

	mu          sync.Mutex
	cached      *x509.CertPool
	fetchedAt   time.Time
	nextRefresh time.Time
	inflight    *ctbRefresh
}

func (c *ctbRoots) pool(ctx context.Context) (*x509.CertPool, error) {
	c.mu.Lock()
	now := c.now()
	if c.cached != nil && now.Before(c.nextRefresh) {
		p := c.cached
		c.mu.Unlock()
		return p, nil
	}
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	flight := c.inflight
	if flight == nil {
		flight = &ctbRefresh{done: make(chan struct{})}
		c.inflight = flight
		c.mu.Unlock()
		go func() {
			defer close(flight.done)
			fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ctbFetchTimeout)
			defer cancel()
			p, err := c.fetch(fetchCtx)

			c.mu.Lock()
			defer c.mu.Unlock()
			c.inflight = nil
			doneAt := c.now()
			if err == nil {
				c.cached = p
				c.fetchedAt = doneAt
				c.nextRefresh = doneAt.Add(ctbCacheTTL)
				flight.pool = p
				return
			}
			if c.cached != nil && doneAt.Sub(c.fetchedAt) < ctbMaxStale {
				next := doneAt.Add(ctbCacheTTL)
				if staleEnd := c.fetchedAt.Add(ctbMaxStale); next.After(staleEnd) {
					next = staleEnd
				}
				c.nextRefresh = next
				flight.pool = c.cached
				return
			}
			flight.err = err
		}()
	} else {
		c.mu.Unlock()
	}

	select {
	case <-flight.done:
		if flight.pool != nil {
			return flight.pool, nil
		}
		return nil, flight.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *ctbRoots) fetch(ctx context.Context) (*x509.CertPool, error) {
	p := x509.NewCertPool()
	if c.name != "" {
		tb, err := c.lister.get(ctx, c.name)
		if err != nil {
			return nil, fmt.Errorf("get ClusterTrustBundle %q: %w", c.name, err)
		}
		if !p.AppendCertsFromPEM([]byte(tb.TrustBundle)) {
			return nil, fmt.Errorf("ClusterTrustBundle %q contains no valid certificates", c.name)
		}
		return p, nil
	}
	bundles, err := c.lister.list(ctx, CanaryingLabel+"="+CanaryingLive)
	if err != nil {
		return nil, fmt.Errorf("list ClusterTrustBundles (label %s=%s): %w", CanaryingLabel, CanaryingLive, err)
	}
	sort.Slice(bundles, func(i, j int) bool { return bundles[i].Name < bundles[j].Name })
	var used []string
	for _, tb := range bundles {
		// The label selector is applied server-side; re-check it so a
		// lister that ignores it can never widen the trust set.
		if tb.SignerName != ServiceDNSSignerName || tb.Labels[CanaryingLabel] != CanaryingLive {
			continue
		}
		if p.AppendCertsFromPEM([]byte(tb.TrustBundle)) {
			used = append(used, tb.Name)
		}
	}
	if len(used) == 0 {
		return nil, fmt.Errorf("no ClusterTrustBundle with signer %s and label %s=%s holds a valid certificate; set ca_file or cluster_trust_bundle", ServiceDNSSignerName, CanaryingLabel, CanaryingLive)
	}
	return p, nil
}

// newCTBLister returns a lister that tries certificates.k8s.io/v1 and falls
// back to v1beta1 when v1 is not served (or not reachable through this
// client).
func newCTBLister(k8s kubernetes.Interface) ctbLister {
	return &fallbackCTBLister{
		primary:  &v1CTBLister{rc: k8s.CertificatesV1().RESTClient()},
		fallback: &v1beta1CTBLister{k8s: k8s},
	}
}

// fallbackCTBLister asks primary first and fallback on any primary error.
// Both versions are views of the same stored objects, so a successful
// primary answer is never second-guessed.
type fallbackCTBLister struct {
	primary, fallback ctbLister
}

func (l *fallbackCTBLister) get(ctx context.Context, name string) (trustBundle, error) {
	tb, err := l.primary.get(ctx, name)
	if err == nil {
		return tb, nil
	}
	tb, err2 := l.fallback.get(ctx, name)
	if err2 != nil {
		return trustBundle{}, fmt.Errorf("v1: %v; v1beta1: %w", err, err2)
	}
	return tb, nil
}

func (l *fallbackCTBLister) list(ctx context.Context, labelSelector string) ([]trustBundle, error) {
	tbs, err := l.primary.list(ctx, labelSelector)
	if err == nil {
		return tbs, nil
	}
	tbs, err2 := l.fallback.list(ctx, labelSelector)
	if err2 != nil {
		return nil, fmt.Errorf("v1: %v; v1beta1: %w", err, err2)
	}
	return tbs, nil
}

// errCTBv1Unavailable reports that the client cannot issue raw
// certificates.k8s.io/v1 requests (e.g. the client-go fake clientset).
var errCTBv1Unavailable = errors.New("certificates.k8s.io/v1 ClusterTrustBundles are not available through this client")

const ctbV1Path = "/apis/certificates.k8s.io/v1/clustertrustbundles"

// v1CTBLister reads certificates.k8s.io/v1 ClusterTrustBundles over the raw
// REST client, because the vendored client-go has no typed v1 client for
// them. The JSON shape matches v1beta1 (metadata, spec.signerName,
// spec.trustBundle).
type v1CTBLister struct {
	rc rest.Interface
}

type ctbJSON struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		SignerName  string `json:"signerName"`
		TrustBundle string `json:"trustBundle"`
	} `json:"spec"`
}

func (j ctbJSON) bundle() trustBundle {
	return trustBundle{Name: j.Metadata.Name, SignerName: j.Spec.SignerName, Labels: j.Metadata.Labels, TrustBundle: j.Spec.TrustBundle}
}

func (l *v1CTBLister) client() (rest.Interface, error) {
	if l.rc == nil {
		return nil, errCTBv1Unavailable
	}
	if rc, ok := l.rc.(*rest.RESTClient); ok && rc == nil {
		return nil, errCTBv1Unavailable
	}
	return l.rc, nil
}

func (l *v1CTBLister) get(ctx context.Context, name string) (trustBundle, error) {
	rc, err := l.client()
	if err != nil {
		return trustBundle{}, err
	}
	raw, err := rc.Get().AbsPath(ctbV1Path).Name(name).Do(ctx).Raw()
	if err != nil {
		return trustBundle{}, err
	}
	var obj ctbJSON
	if err := json.Unmarshal(raw, &obj); err != nil {
		return trustBundle{}, fmt.Errorf("decode ClusterTrustBundle: %w", err)
	}
	return obj.bundle(), nil
}

func (l *v1CTBLister) list(ctx context.Context, labelSelector string) ([]trustBundle, error) {
	rc, err := l.client()
	if err != nil {
		return nil, err
	}
	raw, err := rc.Get().AbsPath(ctbV1Path).Param("labelSelector", labelSelector).Do(ctx).Raw()
	if err != nil {
		return nil, err
	}
	var obj struct {
		Items []ctbJSON `json:"items"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("decode ClusterTrustBundleList: %w", err)
	}
	out := make([]trustBundle, 0, len(obj.Items))
	for _, it := range obj.Items {
		out = append(out, it.bundle())
	}
	return out, nil
}

// v1beta1CTBLister reads certificates.k8s.io/v1beta1 ClusterTrustBundles
// through the typed client.
type v1beta1CTBLister struct {
	k8s kubernetes.Interface
}

func (l *v1beta1CTBLister) get(ctx context.Context, name string) (trustBundle, error) {
	ctb, err := l.k8s.CertificatesV1beta1().ClusterTrustBundles().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return trustBundle{}, err
	}
	return trustBundle{Name: ctb.Name, SignerName: ctb.Spec.SignerName, Labels: ctb.Labels, TrustBundle: ctb.Spec.TrustBundle}, nil
}

func (l *v1beta1CTBLister) list(ctx context.Context, labelSelector string) ([]trustBundle, error) {
	res, err := l.k8s.CertificatesV1beta1().ClusterTrustBundles().List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, err
	}
	out := make([]trustBundle, 0, len(res.Items))
	for _, ctb := range res.Items {
		out = append(out, trustBundle{Name: ctb.Name, SignerName: ctb.Spec.SignerName, Labels: ctb.Labels, TrustBundle: ctb.Spec.TrustBundle})
	}
	return out, nil
}

// endpointHost returns the host part of an endpoint written as host:port,
// a bare host, a URL ("https://host[:port]/..."), or a gRPC target
// ("dns:///host:port", "dns://resolver/host:port"). Returns "" when no host
// can be found.
func endpointHost(endpoint string) string {
	e := strings.TrimSpace(endpoint)
	if e == "" {
		return ""
	}
	if strings.Contains(e, "://") {
		u, err := url.Parse(e)
		if err != nil {
			return ""
		}
		switch u.Scheme {
		case "dns", "passthrough", "unix", "xds":
			if p := strings.TrimPrefix(u.Path, "/"); p != "" {
				return hostOnly(p)
			}
		}
		return u.Hostname()
	}
	return hostOnly(e)
}
