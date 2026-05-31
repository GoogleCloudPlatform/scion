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

package templatecache

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// HubConnectivityError indicates the Hub is unreachable.
type HubConnectivityError struct {
	Cause error
}

func (e *HubConnectivityError) Error() string {
	return fmt.Sprintf("hub is unreachable: %v", e.Cause)
}

func (e *HubConnectivityError) Unwrap() error {
	return e.Cause
}

// IsHubConnectivityError returns true if the error indicates Hub connectivity issues.
func IsHubConnectivityError(err error) bool {
	if err == nil {
		return false
	}

	// Check for our custom error type
	var hubErr *HubConnectivityError
	if errors.As(err, &hubErr) {
		return true
	}

	// Check for common network errors
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}

	// Check for connection refused
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}

	// Check for URL errors (typically DNS failures)
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}

	// Check error message for common patterns
	errMsg := err.Error()
	connectivityPatterns := []string{
		"connection refused",
		"no such host",
		"network is unreachable",
		"dial tcp",
		"dial udp",
		"timeout",
		"deadline exceeded",
	}
	for _, pattern := range connectivityPatterns {
		if strings.Contains(strings.ToLower(errMsg), pattern) {
			return true
		}
	}

	return false
}

// Hydrator fetches templates from Hub storage and caches them locally. It is a
// thin template-kind wrapper around the kind-generic Resolver (resolver.go);
// its public API is preserved for existing call sites.
type Hydrator struct {
	r *Resolver
}

// NewHydrator creates a new template hydrator. A nil hubClient yields a resolver
// that reports "hub client not configured" rather than panicking.
func NewHydrator(cache *Cache, hubClient hubclient.Client) *Hydrator {
	var f resourceFetcher
	if hubClient != nil {
		f = &templateFetcher{client: hubClient}
	}
	return &Hydrator{r: NewResolver(cache, f, "template")}
}

// Hydrate fetches a template from the Hub and returns the local path.
// If the template's content hash is already cached, the cached version is used.
// Otherwise the whole resource is downloaded, hash-verified, and stored under
// its content hash. The templateRef can be a template ID, slug, or name.
func (h *Hydrator) Hydrate(ctx context.Context, templateRef string) (string, error) {
	return h.r.Resolve(ctx, templateRef)
}

// HydrateWithHash fetches a template, using the provided hash for a fast cache
// lookup. This is useful when the Hub dispatcher includes the content hash in
// the request, letting the broker skip the metadata round-trip on a cache hit.
func (h *Hydrator) HydrateWithHash(ctx context.Context, templateRef string, contentHash string) (string, error) {
	return h.r.ResolveWithHash(ctx, templateRef, contentHash)
}

// PrefetchTemplate downloads and caches a template without returning the path.
// This is useful for warming the cache in the background.
func (h *Hydrator) PrefetchTemplate(ctx context.Context, templateRef string) error {
	_, err := h.r.Resolve(ctx, templateRef)
	return err
}

// HydratorConfig holds configuration for the hydrator.
type HydratorConfig struct {
	// CacheDir is the directory for the template cache.
	CacheDir string

	// CacheMaxSize is the maximum cache size in bytes.
	CacheMaxSize int64

	// HubEndpoint is the Hub API endpoint.
	HubEndpoint string

	// HubToken is the authentication token for the Hub.
	HubToken string

	// DownloadTimeout is the timeout for downloading template files.
	DownloadTimeout time.Duration
}

// DefaultHydratorConfig returns the default hydrator configuration.
func DefaultHydratorConfig() HydratorConfig {
	return HydratorConfig{
		CacheDir:        "", // Will be set based on ~/.scion/cache/templates
		CacheMaxSize:    DefaultMaxSize,
		DownloadTimeout: 5 * time.Minute,
	}
}
