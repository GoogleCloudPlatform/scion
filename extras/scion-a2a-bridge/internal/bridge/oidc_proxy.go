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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"golang.org/x/sync/singleflight"
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
// A singleflight.Group deduplicates concurrent fetches for the same key,
// preventing cache stampedes when the cache is empty or expired.
type oidcProxyCache struct {
	mu      sync.RWMutex
	entries map[string]*oidcCacheEntry
	ttl     time.Duration
	flight  singleflight.Group
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

	body, err := s.fetchCachedOIDC(r.Context(), "openid-configuration", hubURL)
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
	if _, err := w.Write(rewritten); err != nil {
		s.log.Debug("failed to write OIDC discovery response", "error", err)
	}
}

// handleJWKSProxy proxies the hub's /.well-known/jwks.json endpoint publicly.
func (s *Server) handleJWKSProxy(w http.ResponseWriter, r *http.Request) {
	cfg := s.effectiveConfig()
	hubEndpoint := strings.TrimRight(cfg.Hub.Endpoint, "/")
	hubURL := hubEndpoint + "/.well-known/jwks.json"

	body, err := s.fetchCachedOIDC(r.Context(), "jwks", hubURL)
	if err != nil {
		s.log.Error("failed to fetch JWKS from hub", "error", err)
		http.Error(w, "failed to fetch JWKS from hub", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	if _, err := w.Write(body); err != nil {
		s.log.Debug("failed to write JWKS response", "error", err)
	}
}

// fetchCachedOIDC fetches an OIDC endpoint from the hub, using the cache
// when available. Concurrent requests for the same cache key are deduplicated
// via singleflight to prevent cache stampedes. The bridge's transport auth
// (IAP credentials) is applied to reach hubs behind identity-aware proxies.
func (s *Server) fetchCachedOIDC(ctx context.Context, cacheKey, hubURL string) ([]byte, error) {
	cache := s.bridge.oidcCache

	// Fast path: return from cache if present and not expired.
	if cache != nil {
		if cached := cache.get(cacheKey); cached != nil {
			return cached, nil
		}
	}

	// fetchFromHub performs the actual HTTP request to the hub.
	fetchFromHub := func() ([]byte, error) {
		client := s.bridge.oidcHTTPClient()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, hubURL, nil)
		if err != nil {
			return nil, fmt.Errorf("creating request for %s: %w", hubURL, err)
		}

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", hubURL, err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: status %d", hubURL, resp.StatusCode)
		}

		body, err := io.ReadAll(io.LimitReader(resp.Body, oidcProxyMaxBody+1))
		if err != nil {
			return nil, fmt.Errorf("reading response from %s: %w", hubURL, err)
		}
		if int64(len(body)) > oidcProxyMaxBody {
			return nil, fmt.Errorf("response from %s exceeded maximum size of %d bytes", hubURL, oidcProxyMaxBody)
		}

		return body, nil
	}

	// Without a cache, fetch directly.
	if cache == nil {
		return fetchFromHub()
	}

	// Use singleflight to deduplicate concurrent fetches for the same key.
	// This prevents a thundering herd when the cache is empty or expired.
	val, err, _ := cache.flight.Do(cacheKey, func() (interface{}, error) {
		// Double-check after winning the singleflight race.
		if cached := cache.get(cacheKey); cached != nil {
			return cached, nil
		}

		body, err := fetchFromHub()
		if err != nil {
			return nil, err
		}

		cache.set(cacheKey, body)
		return body, nil
	})
	if err != nil {
		return nil, err
	}

	return val.([]byte), nil
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

// oidcHTTPClient returns a shared HTTP client configured with the bridge's
// transport auth (IAP / Cloud Run invoker) for reaching the hub's OIDC
// endpoints. The client is created once and reused for connection pooling.
func (b *Bridge) oidcHTTPClient() *http.Client {
	b.oidcClientOnce.Do(func() {
		transport := http.DefaultTransport
		if b.transportSrc != nil {
			transport = transportauth.Wrap(transport, b.transportSrc, b.transportMode)
		}
		b.oidcClient = &http.Client{
			Transport: transport,
			Timeout:   10 * time.Second,
		}
	})
	return b.oidcClient
}
