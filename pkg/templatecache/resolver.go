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
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// resolver.go is the §7.3 step-3b broker-side landing: the kind-generic resource
// resolver. The download-and-cache algorithm is identical for templates,
// harness-configs, and future kinds — it touches the Hub client at only three
// points (Get metadata, request signed download URLs, download a file), all of
// which have byte-identical shapes across hubclient.Templates() and
// hubclient.HarnessConfigs(). resourceFetcher abstracts those three points so a
// single Resolver serves every kind. Hydrator (hydrator.go) is now a thin
// template-kind wrapper around Resolver, preserving its public API.

// resourceFetcher abstracts the Hub-client surface a Resolver needs, so the same
// resolve algorithm works for any resource kind.
type resourceFetcher interface {
	// Get returns the resource's ID and content hash, or id=="" if not found.
	Get(ctx context.Context, ref string) (id, contentHash string, err error)
	// RequestDownloadURLs returns signed GET URLs (with per-file hashes).
	RequestDownloadURLs(ctx context.Context, id string) (*hubclient.DownloadResponse, error)
	// DownloadFile downloads a single file from a signed URL.
	DownloadFile(ctx context.Context, url string) ([]byte, error)
}

// Resolver fetches a file-based resource from the Hub's storage backend and
// caches it locally, content-addressed by hash. It is kind-agnostic; construct
// one per kind with the matching fetcher and a kind-scoped cache.
type Resolver struct {
	cache   *Cache
	fetcher resourceFetcher
	label   string // resource kind for error/log messages, e.g. "template"
}

// NewResolver creates a resolver for a single resource kind.
func NewResolver(cache *Cache, fetcher resourceFetcher, label string) *Resolver {
	return &Resolver{cache: cache, fetcher: fetcher, label: label}
}

// Resolve fetches a resource by ref (id, slug, or name) and returns the local
// path. On a content-hash cache hit the cached copy is returned; otherwise the
// whole resource is downloaded, every file's hash is verified, and it is stored
// under its content hash.
func (r *Resolver) Resolve(ctx context.Context, ref string) (string, error) {
	if r.fetcher == nil {
		return "", fmt.Errorf("hub client not configured")
	}

	// Step 1: metadata.
	id, contentHash, err := r.fetcher.Get(ctx, ref)
	if err != nil {
		if IsHubConnectivityError(err) {
			return "", &HubConnectivityError{Cause: err}
		}
		return "", fmt.Errorf("failed to get %s metadata: %w", r.label, err)
	}
	if id == "" {
		return "", fmt.Errorf("%s not found: %s", r.label, ref)
	}

	// Step 2: content-addressed cache hit (fast path).
	if contentHash != "" {
		if cachedPath, ok := r.cache.Get(contentHash); ok {
			return cachedPath, nil
		}
	}

	// Step 3: request download URLs (includes per-file hashes).
	downloadResp, err := r.fetcher.RequestDownloadURLs(ctx, id)
	if err != nil {
		if IsHubConnectivityError(err) {
			return "", &HubConnectivityError{Cause: err}
		}
		return "", fmt.Errorf("failed to get download URLs: %w", err)
	}
	if downloadResp == nil || len(downloadResp.Files) == 0 {
		return "", fmt.Errorf("%s has no files: %s", r.label, ref)
	}

	// Step 4: download the whole resource and verify each file's hash.
	files := make(map[string][]byte, len(downloadResp.Files))
	for _, fileInfo := range downloadResp.Files {
		content, dlErr := r.fetcher.DownloadFile(ctx, fileInfo.URL)
		if dlErr != nil {
			if IsHubConnectivityError(dlErr) {
				return "", &HubConnectivityError{Cause: dlErr}
			}
			return "", fmt.Errorf("failed to download file %s: %w", fileInfo.Path, dlErr)
		}

		if fileInfo.Hash != "" {
			actualHash := transfer.HashBytes(content)
			if actualHash != fileInfo.Hash {
				return "", fmt.Errorf("hash mismatch for file %s: expected %s, got %s",
					fileInfo.Path, fileInfo.Hash, actualHash)
			}
		}

		files[fileInfo.Path] = content
	}

	// Step 5: store in cache keyed by content hash.
	if contentHash == "" {
		contentHash = computeContentHash(files)
	}
	cachePath, storeErr := r.cache.Put(contentHash, files)
	if storeErr != nil {
		return "", fmt.Errorf("failed to cache %s: %w", r.label, storeErr)
	}
	return cachePath, nil
}

// ResolveWithHash fetches a resource, using the provided hash for a fast cache
// lookup that skips the metadata round-trip on a hit.
func (r *Resolver) ResolveWithHash(ctx context.Context, ref, contentHash string) (string, error) {
	if contentHash != "" {
		if cachedPath, ok := r.cache.Get(contentHash); ok {
			return cachedPath, nil
		}
	}
	return r.Resolve(ctx, ref)
}

// computeContentHash computes the aggregate hash of a downloaded file set. It is
// the canonical transfer.ComputeContentHash applied to per-file content hashes.
func computeContentHash(files map[string][]byte) string {
	fileInfos := make([]transfer.FileInfo, 0, len(files))
	for path, content := range files {
		fileInfos = append(fileInfos, transfer.FileInfo{
			Path: path,
			Hash: transfer.HashBytes(content),
		})
	}
	return transfer.ComputeContentHash(fileInfos)
}

// --- Per-kind fetchers ----------------------------------------------------

// templateFetcher adapts hubclient.Templates() to resourceFetcher.
type templateFetcher struct{ client hubclient.Client }

func (f *templateFetcher) Get(ctx context.Context, ref string) (string, string, error) {
	t, err := f.client.Templates().Get(ctx, ref)
	if err != nil {
		return "", "", err
	}
	if t == nil {
		return "", "", nil
	}
	return t.ID, t.ContentHash, nil
}

func (f *templateFetcher) RequestDownloadURLs(ctx context.Context, id string) (*hubclient.DownloadResponse, error) {
	return f.client.Templates().RequestDownloadURLs(ctx, id)
}

func (f *templateFetcher) DownloadFile(ctx context.Context, url string) ([]byte, error) {
	return f.client.Templates().DownloadFile(ctx, url)
}

// harnessConfigFetcher adapts hubclient.HarnessConfigs() to resourceFetcher.
type harnessConfigFetcher struct{ client hubclient.Client }

func (f *harnessConfigFetcher) Get(ctx context.Context, ref string) (string, string, error) {
	hc, err := f.client.HarnessConfigs().Get(ctx, ref)
	if err != nil {
		return "", "", err
	}
	if hc == nil {
		return "", "", nil
	}
	return hc.ID, hc.ContentHash, nil
}

func (f *harnessConfigFetcher) RequestDownloadURLs(ctx context.Context, id string) (*hubclient.DownloadResponse, error) {
	return f.client.HarnessConfigs().RequestDownloadURLs(ctx, id)
}

func (f *harnessConfigFetcher) DownloadFile(ctx context.Context, url string) ([]byte, error) {
	return f.client.HarnessConfigs().DownloadFile(ctx, url)
}

// NewHarnessConfigResolver creates a Resolver for harness-configs. A nil
// hubClient yields a resolver that reports "hub client not configured".
func NewHarnessConfigResolver(cache *Cache, hubClient hubclient.Client) *Resolver {
	var f resourceFetcher
	if hubClient != nil {
		f = &harnessConfigFetcher{client: hubClient}
	}
	return NewResolver(cache, f, "harness-config")
}
