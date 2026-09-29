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

// Package substrate is a minimal, scion-owned re-implementation of the
// dialer substrate ships at internal/ateclient/builder.go. That package is
// internal/ to github.com/agent-substrate/substrate, so it cannot be
// imported; this package covers only what the scion runtime broker needs:
//
//   - an in-cluster gRPC connection to the ateapi Control service,
//     authenticated with a TokenRequest-minted ServiceAccount token
//     refreshed before it expires, with TLS verified against a configured
//     CA (never InsecureSkipVerify);
//   - a plain HTTP client for calling an actor's control server through
//     Substrate's inbound atenet-router, using the ate-target-actor header.
package substrate

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// DefaultTokenAudience is used when DialerConfig.TokenAudience is empty,
// matching the audience ateapi's own TokenRequest-based auth expects
// (substrate-runtime.md §1).
const DefaultTokenAudience = "api.ate-system.svc"

// tokenExpirySeconds is the lifetime requested for each minted token.
const tokenExpirySeconds = int64(3600)

// tokenRefreshSkew is how long before expiry a cached token is renewed, so a
// concurrent in-flight RPC never observes a token that expires mid-call.
const tokenRefreshSkew = 60 * time.Second

// serviceAccountTokenFile is the path kubelet projects the pod's own
// ServiceAccount token to, in every supported Kubernetes version.
const serviceAccountTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// serviceAccountNamespaceFile is the path kubelet projects the pod's
// namespace to, alongside serviceAccountTokenFile.
const serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// DialerConfig configures Dial.
type DialerConfig struct {
	// APIEndpoint is the ateapi Control gRPC endpoint, e.g.
	// "api.ate-system.svc:443". Required.
	APIEndpoint string
	// TokenAudience is the audience requested for the TokenRequest used to
	// authenticate to ateapi. Defaults to DefaultTokenAudience when empty.
	TokenAudience string
	// CAFile is a path to a PEM CA bundle used to verify ateapi's serving
	// certificate. Used when ClusterTrustBundle is empty.
	CAFile string
	// ClusterTrustBundle names a certificates.k8s.io/v1beta1
	// ClusterTrustBundle object whose trust bundle verifies ateapi's serving
	// certificate. Takes precedence over CAFile when both are set.
	ClusterTrustBundle string
	// ServerName overrides the TLS ServerName sent in the handshake.
	// Defaults to the host portion of APIEndpoint.
	ServerName string
	// Namespace overrides the namespace the client's own ServiceAccount is
	// looked up in. Defaults to the value in serviceAccountNamespaceFile
	// (the pod's own namespace).
	Namespace string
	// ServiceAccountName overrides the ServiceAccount name the client mints
	// tokens from. Defaults to the "sub" claim of the pod's own mounted
	// token (system:serviceaccount:<namespace>:<name>) — i.e. the broker
	// requests a token for itself, which requires the broker's
	// ServiceAccount to have RBAC permission to create tokens for itself
	// (deploy/substrate/broker.yaml grants this).
	ServiceAccountName string
}

// serverTLSConfig builds the *tls.Config used to verify ateapi's (or the
// router's) serving certificate against the configured CA.
// InsecureSkipVerify is never set, even implicitly: a cfg with neither
// ClusterTrustBundle nor CAFile is a configuration error, not a fallback to
// an unverified connection.
func serverTLSConfig(ctx context.Context, k8sClient kubernetes.Interface, cfg DialerConfig) (*tls.Config, error) {
	pool := x509.NewCertPool()

	switch {
	case cfg.ClusterTrustBundle != "":
		ctb, err := k8sClient.CertificatesV1beta1().ClusterTrustBundles().Get(ctx, cfg.ClusterTrustBundle, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("substrate: get ClusterTrustBundle %q: %w", cfg.ClusterTrustBundle, err)
		}
		if !pool.AppendCertsFromPEM([]byte(ctb.Spec.TrustBundle)) {
			return nil, fmt.Errorf("substrate: ClusterTrustBundle %q contains no valid certificates", cfg.ClusterTrustBundle)
		}
	case cfg.CAFile != "":
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("substrate: read ca_file %q: %w", cfg.CAFile, err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("substrate: ca_file %q contains no valid certificates", cfg.CAFile)
		}
	default:
		return nil, fmt.Errorf("substrate: one of cluster_trust_bundle or ca_file must be configured; InsecureSkipVerify is not supported")
	}

	serverName := cfg.ServerName
	if serverName == "" {
		serverName = hostOnly(cfg.APIEndpoint)
	}

	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    pool,
		ServerName: serverName,
	}, nil
}

// hostOnly strips a ":port" suffix from a host:port endpoint, returning the
// input unchanged if it carries no port.
func hostOnly(endpoint string) string {
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		return endpoint
	}
	return host
}

// tokenSource mints and caches a bearer token for the ateapi TokenRequest
// audience, refreshing it before it expires. It implements
// credentials.PerRPCCredentials (google.golang.org/grpc/credentials) via
// GetRequestMetadata/RequireTransportSecurity, defined in dialer_grpc.go so
// this file has no direct grpc dependency beyond what Dial needs.
type tokenSource struct {
	client    kubernetes.Interface
	audience  string
	namespace string
	saName    string

	mu        sync.Mutex
	cached    string
	expiresAt time.Time

	// now is overridable for tests.
	now func() time.Time
}

// newTokenSource builds a tokenSource for cfg, resolving namespace/SA-name
// defaults from the pod's own mounted ServiceAccount token when not
// overridden in cfg.
func newTokenSource(client kubernetes.Interface, cfg DialerConfig) (*tokenSource, error) {
	audience := cfg.TokenAudience
	if audience == "" {
		audience = DefaultTokenAudience
	}

	namespace := cfg.Namespace
	saName := cfg.ServiceAccountName
	if namespace == "" || saName == "" {
		selfNS, selfName, err := currentServiceAccountIdentity(serviceAccountTokenFile, serviceAccountNamespaceFile)
		if err != nil {
			return nil, fmt.Errorf("substrate: determine own ServiceAccount identity: %w", err)
		}
		if namespace == "" {
			namespace = selfNS
		}
		if saName == "" {
			saName = selfName
		}
	}

	return &tokenSource{
		client:    client,
		audience:  audience,
		namespace: namespace,
		saName:    saName,
		now:       time.Now,
	}, nil
}

// GetRequestMetadata implements credentials.PerRPCCredentials.
func (t *tokenSource) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
	tok, err := t.token(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]string{"authorization": "Bearer " + tok}, nil
}

// RequireTransportSecurity implements credentials.PerRPCCredentials. The
// bearer token must never be sent over a non-TLS channel.
func (t *tokenSource) RequireTransportSecurity() bool { return true }

func (t *tokenSource) token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.cached != "" && t.now().Before(t.expiresAt.Add(-tokenRefreshSkew)) {
		return t.cached, nil
	}

	exp := tokenExpirySeconds
	req := &authv1.TokenRequest{
		Spec: authv1.TokenRequestSpec{
			Audiences:         []string{t.audience},
			ExpirationSeconds: &exp,
		},
	}
	res, err := t.client.CoreV1().ServiceAccounts(t.namespace).CreateToken(ctx, t.saName, req, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("substrate: mint ateapi token for %s/%s: %w", t.namespace, t.saName, err)
	}
	if res.Status.Token == "" {
		return "", fmt.Errorf("substrate: mint ateapi token for %s/%s: empty token in response", t.namespace, t.saName)
	}

	t.cached = res.Status.Token
	if !res.Status.ExpirationTimestamp.Time.IsZero() {
		t.expiresAt = res.Status.ExpirationTimestamp.Time
	} else {
		t.expiresAt = t.now().Add(time.Duration(tokenExpirySeconds) * time.Second)
	}
	return t.cached, nil
}
