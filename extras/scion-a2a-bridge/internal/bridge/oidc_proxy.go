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

package bridge

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

const (
	// oidcProxyCacheTTL is how long proxied OIDC responses are cached.
	oidcProxyCacheTTL = 5 * time.Minute

	// oidcProxyMaxBody is the maximum response body size from the hub.
	oidcProxyMaxBody = 1 << 20 // 1 MB
)

// oidcCacheEntry holds a cached OIDC proxy response.
type oidcCacheEntry struct {
	body      []byte
	fetchedAt time.Time
}

// oidcProxyCache provides a simple TTL cache for proxied OIDC responses.
type oidcProxyCache struct {
	mu      sync.RWMutex
	entries map[string]*oidcCacheEntry
	ttl     time.Duration
}

func newOIDCProxyCache(ttl time.Duration) *oidcProxyCache {
	return &oidcProxyCache{
		entries: make(map[string]*oidcCacheEntry),
		ttl:     ttl,
	}
}

// get returns the cached body for key, or nil if expired/missing.
func (c *oidcProxyCache) get(key string) []byte {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[key]
	if !ok || time.Since(entry.fetchedAt) > c.ttl {
		return nil
	}
	return entry.body
}

// set stores a response body in the cache.
func (c *oidcProxyCache) set(key string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = &oidcCacheEntry{
		body:      body,
		fetchedAt: time.Now(),
	}
}

// handleOIDCDiscoveryProxy proxies the hub's /.well-known/openid-configuration
// endpoint publicly. The jwks_uri field is rewritten to point to the bridge's
// own /.well-known/jwks.json endpoint so federation partners can fetch the JWKS
// without IAP credentials.
func (s *Server) handleOIDCDiscoveryProxy(w http.ResponseWriter, r *http.Request) {
	cfg := s.effectiveConfig()
	hubEndpoint := strings.TrimRight(cfg.Hub.Endpoint, "/")
	hubURL := hubEndpoint + "/.well-known/openid-configuration"

	body, err := s.fetchCachedOIDC("openid-configuration", hubURL)
	if err != nil {
		s.log.Error("failed to fetch OIDC discovery from hub", "error", err)
		http.Error(w, "failed to fetch OIDC discovery from hub", http.StatusBadGateway)
		return
	}

	// Rewrite jwks_uri to point to the bridge's own JWKS proxy endpoint.
	rewritten, err := rewriteJWKSURI(body, cfg.Bridge.ExternalURL)
	if err != nil {
		s.log.Error("failed to rewrite OIDC discovery document", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Write(rewritten)
}

// handleJWKSProxy proxies the hub's /.well-known/jwks.json endpoint publicly.
func (s *Server) handleJWKSProxy(w http.ResponseWriter, r *http.Request) {
	cfg := s.effectiveConfig()
	hubEndpoint := strings.TrimRight(cfg.Hub.Endpoint, "/")
	hubURL := hubEndpoint + "/.well-known/jwks.json"

	body, err := s.fetchCachedOIDC("jwks", hubURL)
	if err != nil {
		s.log.Error("failed to fetch JWKS from hub", "error", err)
		http.Error(w, "failed to fetch JWKS from hub", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Write(body)
}

// fetchCachedOIDC fetches an OIDC endpoint from the hub, using the cache
// when available. The bridge's transport auth (IAP credentials) is applied
// to reach hubs behind identity-aware proxies.
func (s *Server) fetchCachedOIDC(cacheKey, hubURL string) ([]byte, error) {
	if s.bridge.oidcCache != nil {
		if cached := s.bridge.oidcCache.get(cacheKey); cached != nil {
			return cached, nil
		}
	}

	client := s.bridge.oidcHTTPClient()

	resp, err := client.Get(hubURL)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", hubURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", hubURL, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, oidcProxyMaxBody))
	if err != nil {
		return nil, fmt.Errorf("reading response from %s: %w", hubURL, err)
	}

	if s.bridge.oidcCache != nil {
		s.bridge.oidcCache.set(cacheKey, body)
	}

	return body, nil
}

// rewriteJWKSURI takes the raw OIDC discovery JSON and rewrites the jwks_uri
// field to point to the bridge's own JWKS proxy endpoint.
func rewriteJWKSURI(body []byte, bridgeExternalURL string) ([]byte, error) {
	var doc map[string]interface{}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("unmarshal OIDC discovery: %w", err)
	}

	bridgeBase := strings.TrimRight(bridgeExternalURL, "/")
	doc["jwks_uri"] = bridgeBase + "/.well-known/jwks.json"

	rewritten, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("marshal rewritten OIDC discovery: %w", err)
	}
	return rewritten, nil
}

// oidcHTTPClient returns an HTTP client configured with the bridge's transport
// auth (IAP / Cloud Run invoker) for reaching the hub's OIDC endpoints.
func (b *Bridge) oidcHTTPClient() *http.Client {
	transport := http.DefaultTransport
	if b.transportSrc != nil {
		transport = transportauth.Wrap(transport, b.transportSrc, b.transportMode)
	}
	return &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
	}
}
