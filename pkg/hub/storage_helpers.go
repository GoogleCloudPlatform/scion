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
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// fileUploadConcurrency bounds how many of a resource's files upload at once
// (Phase 4). Storage backends (GCS / local FS) are safe for concurrent uploads
// to distinct object paths, so this is the only cap; a small bound keeps a
// single large resource fast without overwhelming the backend. Combined with the
// per-resource pool (resourceImportConcurrency) the worst-case concurrent upload
// count stays modest for the ≤~dozen-item imports this serves.
const fileUploadConcurrency = 8

// generateUploadURLs generates signed PUT URLs for a list of files under basePath.
// Returns the upload URL infos, a manifest URL (if possible), and any error.
func generateUploadURLs(ctx context.Context, stor storage.Storage, basePath string, files []FileUploadRequest) ([]UploadURLInfo, string, error) {
	uploadURLs := make([]UploadURLInfo, 0, len(files))
	var lastErr error
	for _, file := range files {
		objectPath := basePath + "/" + file.Path
		signedURL, err := stor.GenerateSignedURL(ctx, objectPath, storage.SignedURLOptions{
			Method:  "PUT",
			Expires: SignedURLExpiry,
		})
		if err != nil {
			lastErr = err
			continue
		}
		uploadURLs = append(uploadURLs, UploadURLInfo{
			Path:    file.Path,
			URL:     signedURL.URL,
			Method:  signedURL.Method,
			Headers: signedURL.Headers,
			Expires: signedURL.Expires,
		})
	}

	if len(uploadURLs) == 0 && len(files) > 0 && lastErr != nil {
		return nil, "", lastErr
	}

	// Generate manifest URL
	var manifestURL string
	manifestPath := basePath + "/manifest.json"
	signedURL, err := stor.GenerateSignedURL(ctx, manifestPath, storage.SignedURLOptions{
		Method:      "PUT",
		Expires:     SignedURLExpiry,
		ContentType: "application/json",
	})
	if err == nil {
		manifestURL = signedURL.URL
	}

	return uploadURLs, manifestURL, nil
}

// verifyAndFinalizeFiles verifies files exist in storage and computes content hash.
// Returns the content hash string.
func verifyAndFinalizeFiles(ctx context.Context, stor storage.Storage, basePath string, files []store.TemplateFile) (string, error) {
	for _, file := range files {
		objectPath := basePath + "/" + file.Path
		exists, err := stor.Exists(ctx, objectPath)
		if err != nil || !exists {
			return "", &fileNotFoundError{path: file.Path}
		}
	}
	return computeContentHash(files), nil
}

// fileNotFoundError is returned when a file is not found during verification.
type fileNotFoundError struct {
	path string
}

func (e *fileNotFoundError) Error() string {
	return "file not found: " + e.path
}

// toResourceFiles converts a collected file list into the resource file manifest
// shape stored on records. Use it where a manifest is needed without uploading —
// e.g. building a content-hash preview during re-sync to decide whether anything
// changed. (The upload helper builds the manifest incrementally, appending only
// successfully uploaded files, so it does not use this.)
func toResourceFiles(files []transfer.FileInfo) []store.TemplateFile {
	out := make([]store.TemplateFile, len(files))
	for i, fi := range files {
		out[i] = store.TemplateFile{
			Path: fi.Path,
			Size: fi.Size,
			Hash: fi.Hash,
			Mode: fi.Mode,
		}
	}
	return out
}

// uploadResourceFiles uploads a collected directory of resource files to the
// storage backend under storagePath, one object per file. It returns the
// manifest of uploaded files and the set of object paths written (used by
// callers that reconcile stale objects). Any open/upload failure aborts the
// whole upload and returns an error: a partial upload would register a resource
// with a subset of files and a content hash computed over that subset, which
// later corrupts agent provisioning while masquerading as healthy. label
// prefixes error messages ("template bootstrap" / "harness config bootstrap").
//
// This is shared bootstrap mechanics for the resource-storage refactor (§7.3):
// templates and harness-configs both route their import/sync upload loop through
// it, and it is the basis for a future ResourceStore.Bootstrap.
//
// Uploads run concurrently with a bounded worker pool (fileUploadConcurrency,
// Phase 4) since each file is an independent object; errgroup.WithContext
// cancels the in-flight uploads as soon as one fails, preserving the fail-fast
// contract above. Each worker writes its result into its own slot, so the
// returned manifest preserves input order without locking.
func uploadResourceFiles(ctx context.Context, stor storage.Storage, storagePath string, files []transfer.FileInfo, label string) ([]store.TemplateFile, map[string]struct{}, error) {
	type uploadResult struct {
		file       store.TemplateFile
		objectPath string
	}
	results := make([]uploadResult, len(files))

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(fileUploadConcurrency)
	for i, fi := range files {
		i, fi := i, fi
		g.Go(func() error {
			// Abort early if the group's context is already cancelled — either the
			// request was cancelled (client disconnected) or a sibling upload
			// failed. Returning the error keeps the fail-fast contract: a cancelled
			// upload must not produce a partial manifest. (g.Wait reports the first
			// error, so the original upload failure still wins over this Canceled.)
			if err := ctx.Err(); err != nil {
				return err
			}

			objectPath := storagePath + "/" + fi.Path

			f, err := os.Open(fi.FullPath)
			if err != nil {
				return fmt.Errorf("%s: failed to open file %s: %w", label, fi.Path, err)
			}

			_, err = stor.Upload(ctx, objectPath, f, storage.UploadOptions{})
			_ = f.Close()
			if err != nil {
				return fmt.Errorf("%s: failed to upload file %s: %w", label, fi.Path, err)
			}

			results[i] = uploadResult{
				file: store.TemplateFile{
					Path: fi.Path,
					Size: fi.Size,
					Hash: fi.Hash,
					Mode: fi.Mode,
				},
				objectPath: objectPath,
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, nil, err
	}

	uploaded := make([]store.TemplateFile, 0, len(files))
	written := make(map[string]struct{}, len(files))
	for _, r := range results {
		uploaded = append(uploaded, r.file)
		written[r.objectPath] = struct{}{}
	}
	return uploaded, written, nil
}

// reconcileResourceStorage deletes objects under storagePath that are not in the
// keep set, so files removed from a resource don't linger in storage after a
// re-sync. List/delete failures are logged and skipped (best-effort), matching
// the template reconcile behavior this consolidates. name is included in warn
// messages to identify the resource.
func reconcileResourceStorage(ctx context.Context, stor storage.Storage, storagePath, name string, keep map[string]struct{}, log *slog.Logger, label string) {
	listResult, err := stor.List(ctx, storage.ListOptions{Prefix: storagePath + "/"})
	if err != nil {
		log.Warn(label+": failed to list storage for reconcile",
			"resource", name, "prefix", storagePath, "error", err)
		return
	}
	for _, obj := range listResult.Objects {
		if _, keepObj := keep[obj.Name]; keepObj {
			continue
		}
		if err := stor.Delete(ctx, obj.Name); err != nil {
			log.Warn(label+": failed to delete stale object",
				"resource", name, "object", obj.Name, "error", err)
		}
	}
}

// generateDownloadURLs generates signed GET URLs for files under basePath.
// Returns the download URL infos, a manifest URL (if possible), the expiry time, and any error.
// Returns a hard error if signing fails for any listed file — callers must not
// serve a partial URL list, as that produces opaque hydration failures (issue #373).
func generateDownloadURLs(ctx context.Context, stor storage.Storage, basePath string, files []store.TemplateFile) ([]DownloadURLInfo, string, time.Time, error) {
	downloadURLs := make([]DownloadURLInfo, 0, len(files))
	expires := time.Now().Add(SignedURLExpiry)

	for _, file := range files {
		objectPath := basePath + "/" + file.Path
		signedURL, err := stor.GenerateSignedURL(ctx, objectPath, storage.SignedURLOptions{
			Method:  "GET",
			Expires: SignedURLExpiry,
		})
		if err != nil {
			return nil, "", expires, fmt.Errorf("storage object missing: %s (run validate to check storage consistency): %w", file.Path, err)
		}
		downloadURLs = append(downloadURLs, DownloadURLInfo{
			Path: file.Path,
			URL:  signedURL.URL,
			Size: file.Size,
			Hash: file.Hash,
		})
	}

	// Generate manifest URL
	var manifestURL string
	manifestPath := basePath + "/manifest.json"
	signedURL, err := stor.GenerateSignedURL(ctx, manifestPath, storage.SignedURLOptions{
		Method:  "GET",
		Expires: SignedURLExpiry,
	})
	if err != nil {
		return nil, "", expires, fmt.Errorf("storage object missing: manifest.json (run validate to check storage consistency): %w", err)
	}
	if signedURL != nil {
		manifestURL = signedURL.URL
	}

	return downloadURLs, manifestURL, expires, nil
}

// rewriteLocalUploadURLs rewrites file:// URLs to HTTP proxy URLs pointing to
// the hub's own file upload endpoint. This is necessary because local storage
// generates file:// signed URLs that reference server-side paths, which are not
// accessible when the client is on a different machine.
//
// For each URL with a file:// scheme, it is replaced with:
//
//	<hubEndpoint>/api/v1/<resourceType>/<resourceID>/files/<filePath>
//
// with method PUT. The client's authenticated HTTP transport handles auth.
// requestBaseURL derives the external base URL from the incoming HTTP request.
// It trusts X-Forwarded-Proto and X-Forwarded-Host when present, which assumes
// the hub is behind a trusted reverse proxy that overwrites those headers.
// Otherwise it falls back to the request's TLS state and Host header.
func requestBaseURL(r *http.Request) string {
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}

	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		return proto + "://" + host
	}
	if r.TLS != nil {
		return "https://" + host
	}
	return "http://" + host
}

func rewriteLocalUploadURLs(urls []UploadURLInfo, hubEndpoint, resourceType, resourceID string) []UploadURLInfo {
	if hubEndpoint == "" {
		return urls
	}
	hubEndpoint = strings.TrimRight(hubEndpoint, "/")
	for i := range urls {
		if strings.HasPrefix(urls[i].URL, "file://") {
			urls[i].URL = fmt.Sprintf("%s/api/v1/%s/%s/files/%s", hubEndpoint, resourceType, resourceID, urls[i].Path)
			urls[i].Method = http.MethodPut
			urls[i].Headers = map[string]string{
				"Content-Type": "application/octet-stream",
			}
		}
	}
	return urls
}

// rewriteLocalDownloadURLs rewrites file:// URLs to HTTP proxy URLs pointing to
// the hub's own file read endpoint for downloads. Same rationale as
// rewriteLocalUploadURLs — file:// URLs reference server-side paths that are
// inaccessible from remote clients.
func rewriteLocalDownloadURLs(urls []DownloadURLInfo, hubEndpoint, resourceType, resourceID string) []DownloadURLInfo {
	if hubEndpoint == "" {
		return urls
	}
	hubEndpoint = strings.TrimRight(hubEndpoint, "/")
	for i := range urls {
		if strings.HasPrefix(urls[i].URL, "file://") {
			urls[i].URL = hubEndpoint + "/api/v1/" + resourceType + "/" + resourceID + "/files/" + urls[i].Path + "?raw=1"
		}
	}
	return urls
}
