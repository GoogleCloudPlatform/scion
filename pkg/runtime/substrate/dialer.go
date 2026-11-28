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
//     refreshed before it expires, with TLS 1.3 verified against the
//     servicedns ClusterTrustBundles or a configured CA, reloaded on
//     rotation (never InsecureSkipVerify; see tlssource.go);
//   - the mTLS TLS source used to reach actors through Substrate's inbound
//     atenet-router, using the ate-target-actor header.
package substrate

import (
	"context"
	"fmt"
	"net"
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
	// "api.ate-system.svc:443". Required. Dialed as "dns:///<endpoint>"
	// with round_robin across the headless Service's pods unless it already
	// carries a gRPC scheme ("dns:///...", "passthrough:///...").
	APIEndpoint string
	// TokenAudience is the audience requested for the TokenRequest used to
	// authenticate to ateapi. Defaults to DefaultTokenAudience when empty.
	TokenAudience string
	// CAFile is a path to a PEM CA bundle used to verify ateapi's serving
	// certificate (e.g. the projected /run/servicedns-ca/trust-bundle.pem),
	// re-read when its content changes. Used when ClusterTrustBundle is
	// empty. When both are empty, every ClusterTrustBundle with signer
	// ServiceDNSSignerName and label CanaryingLabel=CanaryingLive is pooled.
	CAFile string
	// ClusterTrustBundle names a ClusterTrustBundle object
	// (certificates.k8s.io/v1, falling back to v1beta1) whose trust bundle
	// verifies ateapi's serving certificate. Takes precedence over CAFile.
	ClusterTrustBundle string
	// ClientCertFile is an optional PEM credential bundle (certificate chain
	// and private key in one file, e.g. the podidentity
	// credential-bundle.pem) presented to ateapi for mTLS. Re-read on every
	// handshake. When ateapi accepts the certificate, it authenticates the
	// caller by the certificate's SPIFFE ID and ignores the bearer token.
	ClientCertFile string
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
// GetRequestMetadata/RequireTransportSecurity, defined in grpc.go so
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
