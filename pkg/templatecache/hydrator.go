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
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/ptone/scion-agent/pkg/hubclient"
	"github.com/ptone/scion-agent/pkg/transfer"
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

// Hydrator fetches templates from Hub storage and caches them locally.
type Hydrator struct {
	cache     *Cache
	hubClient hubclient.Client
}

// NewHydrator creates a new template hydrator.
func NewHydrator(cache *Cache, hubClient hubclient.Client) *Hydrator {
	return &Hydrator{
		cache:     cache,
		hubClient: hubClient,
	}
}

// Hydrate fetches a template from the Hub and returns the local path.
// If the template is already cached with a matching content hash, the cached version is used.
// The templateRef can be a template ID, slug, or name.
func (h *Hydrator) Hydrate(ctx context.Context, templateRef string) (string, error) {
	if h.hubClient == nil {
		return "", fmt.Errorf("hub client not configured")
	}

	// Step 1: Get template metadata from Hub
	template, err := h.hubClient.Templates().Get(ctx, templateRef)
	if err != nil {
		if IsHubConnectivityError(err) {
			return "", &HubConnectivityError{Cause: err}
		}
		return "", fmt.Errorf("failed to get template metadata: %w", err)
	}

	if template == nil {
		return "", fmt.Errorf("template not found: %s", templateRef)
	}

	// Step 2: Check if already cached with matching content hash (fast path)
	if template.ContentHash != "" {
		if cachedPath, ok := h.cache.Get(template.ID, template.ContentHash); ok {
			return cachedPath, nil
		}
		// Also check by hash alone in case it was cached under a different ID
		if cachedPath, ok := h.cache.GetByHash(template.ContentHash); ok {
			// Store reference under this template ID too
			_, _ = h.cache.Store(template.ID, template.ContentHash, nil)
			return cachedPath, nil
		}
	}

	// Step 3: Request download URLs from Hub (includes per-file hashes)
	downloadResp, err := h.hubClient.Templates().RequestDownloadURLs(ctx, template.ID)
	if err != nil {
		if IsHubConnectivityError(err) {
			return "", &HubConnectivityError{Cause: err}
		}
		return "", fmt.Errorf("failed to get download URLs: %w", err)
	}

	if len(downloadResp.Files) == 0 {
		return "", fmt.Errorf("template has no files: %s", templateRef)
	}

	// Step 4: Check for older cached version for incremental download
	var cachedHashes map[string]string
	var oldCachePath string
	if oldPath, _, hasCachedVersion := h.cache.GetAnyVersion(template.ID); hasCachedVersion {
		oldCachePath = oldPath
		cachedHashes, err = h.cache.GetFileHashes(oldPath)
		if err != nil {
			// Can't read cached hashes, fall back to full download
			cachedHashes = nil
		}
	}

	// Step 5: Download files (only changed ones if we have a cached version)
	files := make(map[string][]byte)
	var downloadedCount, skippedCount int

	for _, fileInfo := range downloadResp.Files {
		// Check if file is unchanged from cached version
		if cachedHashes != nil {
			if cachedHash, exists := cachedHashes[fileInfo.Path]; exists && cachedHash == fileInfo.Hash {
				// File unchanged, read from cache instead of downloading
				cachedFilePath := oldCachePath + "/" + fileInfo.Path
				content, readErr := readFileFromPath(cachedFilePath)
				if readErr == nil {
					files[fileInfo.Path] = content
					skippedCount++
					continue
				}
				// If read fails, fall through to download
			}
		}

		// Download the file
		content, dlErr := h.hubClient.Templates().DownloadFile(ctx, fileInfo.URL)
		if dlErr != nil {
			if IsHubConnectivityError(dlErr) {
				return "", &HubConnectivityError{Cause: dlErr}
			}
			return "", fmt.Errorf("failed to download file %s: %w", fileInfo.Path, dlErr)
		}

		// Verify hash if provided
		if fileInfo.Hash != "" {
			actualHash := transfer.HashBytes(content)
			if actualHash != fileInfo.Hash {
				return "", fmt.Errorf("hash mismatch for file %s: expected %s, got %s",
					fileInfo.Path, fileInfo.Hash, actualHash)
			}
		}

		files[fileInfo.Path] = content
		downloadedCount++
	}

	// Log incremental sync stats if we used cached files
	if skippedCount > 0 {
		// Incremental download succeeded
		_ = skippedCount // Stats available for debugging if needed
	}

	// Step 6: Store in cache
	contentHash := template.ContentHash
	if contentHash == "" {
		// Compute content hash if not provided
		contentHash = h.computeContentHash(files)
	}

	newCachePath, storeErr := h.cache.Store(template.ID, contentHash, files)
	if storeErr != nil {
		return "", fmt.Errorf("failed to cache template: %w", storeErr)
	}

	return newCachePath, nil
}

// HydrateWithHash fetches a template, using the provided hash for cache lookup.
// This is useful when the Hub dispatcher includes the content hash in the request.
func (h *Hydrator) HydrateWithHash(ctx context.Context, templateRef string, contentHash string) (string, error) {
	// Check cache first using provided hash
	if contentHash != "" {
		if cachedPath, ok := h.cache.GetByHash(contentHash); ok {
			return cachedPath, nil
		}
	}

	// Fall back to full hydration
	return h.Hydrate(ctx, templateRef)
}

// PrefetchTemplate downloads and caches a template without returning the path.
// This is useful for warming the cache in the background.
func (h *Hydrator) PrefetchTemplate(ctx context.Context, templateRef string) error {
	_, err := h.Hydrate(ctx, templateRef)
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

// computeContentHash computes an aggregate hash of all template files.
func (h *Hydrator) computeContentHash(files map[string][]byte) string {
	var fileInfos []transfer.FileInfo
	for path, content := range files {
		fileInfos = append(fileInfos, transfer.FileInfo{
			Path: path,
			Hash: transfer.HashBytes(content),
		})
	}
	return transfer.ComputeContentHash(fileInfos)
}

// readFileFromPath reads the entire contents of a file.
func readFileFromPath(path string) ([]byte, error) {
	return os.ReadFile(path)
}
