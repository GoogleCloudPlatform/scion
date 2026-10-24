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
	"os"

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
	// Get returns the resource's ID, content hash and recorded file
	// manifest (whose Mode fields name executable files), or id=="" if not
	// found.
	Get(ctx context.Context, ref string) (id, contentHash string, files []hubclient.TemplateFile, err error)
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
// path. On a cache hit for the files the metadata manifest records (EntryKey)
// the cached copy is returned; otherwise the whole resource is downloaded,
// every file's hash is verified, and it is stored under the EntryKey of the
// downloaded files.
func (r *Resolver) Resolve(ctx context.Context, ref string) (string, error) {
	if r.fetcher == nil {
		return "", fmt.Errorf("hub client not configured")
	}

	// Step 1: metadata. The hub's content hash is not used: the cache key
	// is computed from the manifest (ptone/scion#5658).
	id, _, metaFiles, err := r.fetcher.Get(ctx, ref)
	if err != nil {
		if IsHubConnectivityError(err) {
			return "", &HubConnectivityError{Cause: err}
		}
		return "", fmt.Errorf("failed to get %s metadata: %w", r.label, err)
	}
	if id == "" {
		return "", fmt.Errorf("%s not found: %s", r.label, ref)
	}

	// Step 2: content-addressed cache hit (fast path), keyed by the paths,
	// modes and file hashes the metadata manifest records (EntryKey). The
	// key does not depend on which content hash version the hub sends
	// (ptone/scion#5658). A manifest without per-file hashes cannot name
	// an entry, so the resource is downloaded.
	if key := manifestEntryKey(metaFiles); key != "" {
		if cachedPath, ok := r.cache.Get(key); ok {
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

	// Step 5: store in cache keyed by what was downloaded: the paths, the
	// verified content hashes and the modes the files are written with
	// (EntryKey). The key therefore always describes the stored files, even
	// when a commit landed between the metadata read in step 1 and the
	// download (ptone/scion#4221), and whichever content hash version the
	// hub records.
	modes := downloadModes(downloadResp.Files, metaFiles)
	cachePath, storeErr := r.cache.PutWithModes(downloadedEntryKey(files, modes), files, modes)
	if storeErr != nil {
		return "", fmt.Errorf("failed to cache %s: %w", r.label, storeErr)
	}
	return cachePath, nil
}

// ResolveWithHash fetches a resource, always verifying the current content hash
// with the hub to prevent serving stale cached content.
//
// Deprecated: ResolveWithHash now always verifies with the hub. Use Resolve
// directly. The contentHash parameter is ignored.
//
// The caller's contentHash (from the dispatch) may be stale if the hub
// re-bootstrapped the resource after the agent was created but before the
// broker resolved it, or if the dispatch raced with a bootstrap. To guard
// against this, ResolveWithHash always calls Resolve, which fetches the
// current metadata from the hub and keys the cache lookup by the current
// manifest. On a cache hit the cached directory is returned immediately (no
// download); on a miss, which includes a contentHash naming an entry stored
// under an earlier key scheme, the resource is downloaded and cached under the
// current key.
func (r *Resolver) ResolveWithHash(ctx context.Context, ref, contentHash string) (string, error) {
	return r.Resolve(ctx, ref)
}

// manifestEntryKey returns the cache entry key for the files a metadata
// manifest records, or "" when the manifest is empty or an entry has no
// content hash (it cannot name the files then).
func manifestEntryKey(files []hubclient.TemplateFile) string {
	if len(files) == 0 {
		return ""
	}
	infos := make([]transfer.FileInfo, len(files))
	for i, f := range files {
		if !transfer.IsContentHash(f.Hash) {
			return ""
		}
		infos[i] = transfer.FileInfo{Path: f.Path, Hash: f.Hash, Mode: f.Mode}
	}
	return EntryKey(infos)
}

// downloadedEntryKey returns the cache entry key for a downloaded file set
// written with the given modes.
func downloadedEntryKey(files map[string][]byte, modes map[string]os.FileMode) string {
	infos := make([]transfer.FileInfo, 0, len(files))
	for path, content := range files {
		infos = append(infos, transfer.FileInfo{
			Path: path,
			Hash: transfer.HashBytes(content),
			Mode: transfer.FormatMode(modes[path]),
		})
	}
	return EntryKey(infos)
}

// downloadModes returns the normalised mode of every downloaded file,
// keyed by path. A hub that carries modes (ptone/scion#5652) names one on
// every download entry, and those win: they describe the version the URLs
// were signed for. An older hub sends none; the modes the metadata
// manifest records are used instead, for entries with the same path and
// content hash (a commit landing between the two reads cannot attach
// another version's modes to the files). A file with neither is 0644.
func downloadModes(dl []hubclient.DownloadURLInfo, meta []hubclient.TemplateFile) map[string]os.FileMode {
	hubSendsModes := false
	for _, f := range dl {
		if f.Mode != "" {
			hubSendsModes = true
			break
		}
	}
	// Only an older hub's response needs the metadata manifest.
	var metaByPath map[string]hubclient.TemplateFile
	if !hubSendsModes {
		metaByPath = make(map[string]hubclient.TemplateFile, len(meta))
		for _, f := range meta {
			metaByPath[f.Path] = f
		}
	}
	modes := make(map[string]os.FileMode, len(dl))
	for _, f := range dl {
		mode := f.Mode
		if !hubSendsModes {
			if m, ok := metaByPath[f.Path]; ok && (f.Hash == "" || m.Hash == f.Hash) {
				mode = m.Mode
			}
		}
		modes[f.Path] = transfer.NormalizeMode(mode)
	}
	return modes
}

// --- Per-kind fetchers ----------------------------------------------------

// templateFetcher adapts hubclient.Templates() to resourceFetcher.
type templateFetcher struct{ client hubclient.Client }

func (f *templateFetcher) Get(ctx context.Context, ref string) (string, string, []hubclient.TemplateFile, error) {
	t, err := f.client.Templates().Get(ctx, ref)
	if err != nil {
		return "", "", nil, err
	}
	if t == nil {
		return "", "", nil, nil
	}
	return t.ID, t.ContentHash, t.Files, nil
}

func (f *templateFetcher) RequestDownloadURLs(ctx context.Context, id string) (*hubclient.DownloadResponse, error) {
	return f.client.Templates().RequestDownloadURLs(ctx, id)
}

func (f *templateFetcher) DownloadFile(ctx context.Context, url string) ([]byte, error) {
	return f.client.Templates().DownloadFile(ctx, url)
}

// harnessConfigFetcher adapts hubclient.HarnessConfigs() to resourceFetcher.
type harnessConfigFetcher struct{ client hubclient.Client }

func (f *harnessConfigFetcher) Get(ctx context.Context, ref string) (string, string, []hubclient.TemplateFile, error) {
	hc, err := f.client.HarnessConfigs().Get(ctx, ref)
	if err != nil {
		return "", "", nil, err
	}
	if hc == nil {
		return "", "", nil, nil
	}
	return hc.ID, hc.ContentHash, hc.Files, nil
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
