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

package hub

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/compute/metadata"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

const (
	// EnvHubOIDCAudience overrides the audience claim in the OIDC identity token.
	EnvHubOIDCAudience = "SCION_HUB_OIDC_AUDIENCE"

	// EnvTransportToken is the env var for the hub-provided transport OIDC token.
	EnvTransportToken = "SCION_TRANSPORT_TOKEN"

	// EnvTransportAudience is the env var for the transport token audience.
	EnvTransportAudience = "SCION_TRANSPORT_AUDIENCE"

	gcpMetadataBaseURL = "http://metadata.google.internal"

	oidcRefreshMargin = 5 * time.Minute
	oidcDefaultTTL    = 1 * time.Hour
	oidcFetchTimeout  = 2 * time.Second
)

// isOnGCPFunc detects whether we're running on GCP. Override in tests.
var isOnGCPFunc = func() bool { return metadata.OnGCE() }

// oidcTokenSource provides OIDC identity tokens for transport-layer auth.
// Implementations are thread-safe.
type oidcTokenSource interface {
	// getToken returns a valid OIDC token, refreshing if necessary.
	getToken() (string, error)
	// setToken updates the cached token and expiry. Used by the refresh path
	// to inject hub-provided tokens.
	setToken(token string, expiry time.Time)
}

// --- metadataTokenSource: fetches OIDC from GCE metadata server ---

// metadataTokenSource fetches and caches Google OIDC identity tokens from the
// GCE metadata server. Used in passthrough/on-GCE mode (the PR #307 pattern).
type metadataTokenSource struct {
	audience        string
	metadataBaseURL string
	httpClient      *http.Client

	mu        sync.RWMutex
	token     string
	expiresAt time.Time
}

func (s *metadataTokenSource) getToken() (string, error) {
	s.mu.RLock()
	if s.token != "" && time.Now().Before(s.expiresAt.Add(-oidcRefreshMargin)) {
		tok := s.token
		s.mu.RUnlock()
		return tok, nil
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	// Double-check after acquiring write lock.
	if s.token != "" && time.Now().Before(s.expiresAt.Add(-oidcRefreshMargin)) {
		return s.token, nil
	}

	url := fmt.Sprintf("%s/computeMetadata/v1/instance/service-accounts/default/identity?audience=%s&format=full",
		s.metadataBaseURL, s.audience)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("oidc: build request: %w", err)
	}
	req.Header.Set("Metadata-Flavor", "Google")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("oidc: metadata fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oidc: metadata server returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("oidc: read response: %w", err)
	}

	tok := strings.TrimSpace(string(body))
	expiry, err := ParseTokenExpiry(tok)
	if err != nil {
		expiry = time.Now().Add(oidcDefaultTTL)
	}

	s.token = tok
	s.expiresAt = expiry
	return tok, nil
}

func (s *metadataTokenSource) setToken(token string, expiry time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = token
	s.expiresAt = expiry
}

// --- injectedTokenSource: hub-provided token refreshed via tokens[] ---

// injectedTokenSource holds a transport token injected by the hub via the
// dispatch payload (cold start) and refreshed via the tokens[] array on
// subsequent refresh calls.
type injectedTokenSource struct {
	mu        sync.RWMutex
	token     string
	expiresAt time.Time
}

func (s *injectedTokenSource) getToken() (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.token == "" {
		return "", fmt.Errorf("oidc: no transport token available")
	}

	// Check if the token is within the refresh margin of expiry.
	// We still return it (it may still be valid), but log a warning.
	if !s.expiresAt.IsZero() && time.Now().After(s.expiresAt.Add(-oidcRefreshMargin)) {
		log.Debug("OIDC transport token is near expiry or expired, returning anyway")
	}

	return s.token, nil
}

func (s *injectedTokenSource) setToken(token string, expiry time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = token
	s.expiresAt = expiry
}

// --- oidcTransport: RoundTripper that injects Authorization: Bearer ---

// oidcTransport is an http.RoundTripper that injects a Google OIDC identity
// token as an Authorization header on outgoing requests.
type oidcTransport struct {
	base   http.RoundTripper
	source oidcTokenSource
}

func (t *oidcTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") == "" {
		tok, err := t.source.getToken()
		if err != nil {
			log.Debug("OIDC token fetch failed, skipping Authorization header: %v", err)
		} else {
			req = req.Clone(req.Context())
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	return t.base.RoundTrip(req)
}

// newOIDCTransport creates an oidcTransport wrapping the base transport.
func newOIDCTransport(base http.RoundTripper, source oidcTokenSource) *oidcTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &oidcTransport{
		base:   base,
		source: source,
	}
}

// configureOIDCTransport sets up the OIDC transport layer on the client.
// Token source selection:
//  1. If SCION_TRANSPORT_TOKEN env var is set → injected mode (hub-provided token).
//  2. Else if running on GCP → metadata server mode (ambient SA identity).
//  3. Else → no OIDC transport (agent uses plain HTTP).
func (c *Client) configureOIDCTransport() {
	// Check for hub-injected transport token (dispatch payload / cold start)
	if tok := os.Getenv(EnvTransportToken); tok != "" {
		source := &injectedTokenSource{}
		expiry, err := ParseTokenExpiry(tok)
		if err != nil {
			expiry = time.Now().Add(oidcDefaultTTL)
		}
		source.setToken(tok, expiry)
		c.oidcSource = source
		c.client.Transport = newOIDCTransport(c.client.Transport, source)
		log.Debug("Configured OIDC transport: injected mode (hub-provided token)")
		return
	}

	// Fall back to GCE metadata server if on GCP — but only when the scion
	// metadata server is NOT active. When SCION_METADATA_MODE is "assign",
	// iptables redirects the metadata IP (169.254.169.254) to the local scion
	// metadata server on port 18380. This makes the real GCE metadata server
	// unreachable, causing OIDC token fetches to time out and creating a
	// circular dependency (hub client → GCE metadata → scion metadata → hub
	// client). If no transport token was injected and scion metadata is active,
	// the Hub doesn't require transport-layer OIDC auth.
	if !isOnGCPFunc() {
		return
	}
	if mode := os.Getenv("SCION_METADATA_MODE"); mode != "" {
		log.Debug("Skipping OIDC metadata mode: scion metadata server active (mode=%s), GCE metadata IP is redirected", mode)
		return
	}

	audience := os.Getenv(EnvHubOIDCAudience)
	if audience == "" {
		audience = c.hubURL
	}

	source := &metadataTokenSource{
		audience:        audience,
		metadataBaseURL: gcpMetadataBaseURL,
		httpClient:      &http.Client{Timeout: oidcFetchTimeout},
	}
	c.oidcSource = source
	c.client.Transport = newOIDCTransport(c.client.Transport, source)
	log.Debug("Configured OIDC transport: metadata mode (audience=%s)", audience)
}
