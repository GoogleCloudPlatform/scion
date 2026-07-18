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

package transportauth

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// MetadataSource fetches and caches Google OIDC identity tokens from the
// GCE metadata server. Used in passthrough/on-GCE mode.
type MetadataSource struct {
	audience        string
	metadataBaseURL string
	httpClient      *http.Client

	mu        sync.RWMutex
	token     string
	expiresAt time.Time
}

// NewMetadataSource creates a MetadataSource that fetches identity tokens
// for the given audience from the GCE metadata server.
func NewMetadataSource(audience string) *MetadataSource {
	return &MetadataSource{
		audience:        audience,
		metadataBaseURL: gcpMetadataBaseURL,
		httpClient:      &http.Client{Timeout: FetchTimeout},
	}
}

// NewMetadataSourceWithURL is like NewMetadataSource but allows overriding
// the metadata server base URL (for testing).
func NewMetadataSourceWithURL(audience, metadataBaseURL string) *MetadataSource {
	return &MetadataSource{
		audience:        audience,
		metadataBaseURL: metadataBaseURL,
		httpClient:      &http.Client{Timeout: FetchTimeout},
	}
}

func (s *MetadataSource) Token() (string, error) {
	s.mu.RLock()
	if s.token != "" && time.Now().Before(s.expiresAt.Add(-RefreshMargin)) {
		tok := s.token
		s.mu.RUnlock()
		return tok, nil
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	// Double-check after acquiring write lock.
	if s.token != "" && time.Now().Before(s.expiresAt.Add(-RefreshMargin)) {
		return s.token, nil
	}

	fetchURL := fmt.Sprintf("%s/computeMetadata/v1/instance/service-accounts/default/identity?audience=%s&format=full",
		s.metadataBaseURL, url.QueryEscape(s.audience))

	req, err := http.NewRequest("GET", fetchURL, nil)
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
		expiry = time.Now().Add(DefaultTTL)
	}

	s.token = tok
	s.expiresAt = expiry
	return tok, nil
}

func (s *MetadataSource) SetToken(token string, expiry time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = token
	s.expiresAt = expiry
}

func (s *MetadataSource) Expiry() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.expiresAt
}

// Audience returns the OIDC audience this source requests tokens for.
func (s *MetadataSource) Audience() string {
	return s.audience
}
