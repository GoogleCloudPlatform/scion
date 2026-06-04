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
	"sync"
	"time"

	"cloud.google.com/go/compute/metadata"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

const (
	// EnvHubOIDCAudience overrides the audience claim in the OIDC identity token.
	EnvHubOIDCAudience = "SCION_HUB_OIDC_AUDIENCE"

	gcpMetadataBaseURL = "http://metadata.google.internal"

	oidcRefreshMargin = 5 * time.Minute
	oidcDefaultTTL    = 1 * time.Hour
	oidcFetchTimeout  = 2 * time.Second
)

// isOnGCPFunc detects whether we're running on GCP. Override in tests.
var isOnGCPFunc = func() bool { return metadata.OnGCE() }

// oidcTokenSource fetches and caches Google OIDC identity tokens from the
// GCE metadata server.
type oidcTokenSource struct {
	audience        string
	metadataBaseURL string
	httpClient      *http.Client

	mu        sync.RWMutex
	token     string
	expiresAt time.Time
}

func (s *oidcTokenSource) getToken() (string, error) {
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
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oidc: metadata server returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("oidc: read response: %w", err)
	}

	tok := string(body)
	expiry, err := ParseTokenExpiry(tok)
	if err != nil {
		expiry = time.Now().Add(oidcDefaultTTL)
	}

	s.token = tok
	s.expiresAt = expiry
	return tok, nil
}

// oidcTransport is an http.RoundTripper that injects a Google OIDC identity
// token as an Authorization header on outgoing requests.
type oidcTransport struct {
	base   http.RoundTripper
	source *oidcTokenSource
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

func newOIDCTransport(base http.RoundTripper, audience, metadataBaseURL string) *oidcTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &oidcTransport{
		base: base,
		source: &oidcTokenSource{
			audience:        audience,
			metadataBaseURL: metadataBaseURL,
			httpClient:      &http.Client{Timeout: oidcFetchTimeout},
		},
	}
}

// maybeConfigureOIDC wraps the client's HTTP transport with an OIDC token
// injector when running on GCP. This enables transparent authentication
// against Cloud Run-hosted hubs.
func (c *Client) maybeConfigureOIDC() {
	if !isOnGCPFunc() {
		return
	}

	audience := os.Getenv(EnvHubOIDCAudience)
	if audience == "" {
		audience = c.hubURL
	}

	c.client.Transport = newOIDCTransport(c.client.Transport, audience, gcpMetadataBaseURL)
	log.Debug("Configured OIDC transport for Cloud Run auth (audience=%s)", audience)
}
