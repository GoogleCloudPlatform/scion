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
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

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

// generateDownloadURLs generates signed GET URLs for files under basePath.
// Returns the download URL infos, a manifest URL (if possible), the expiry time, and any error.
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
			continue
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
	signedURL, _ := stor.GenerateSignedURL(ctx, manifestPath, storage.SignedURLOptions{
		Method:  "GET",
		Expires: SignedURLExpiry,
	})
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
// It uses X-Forwarded-Proto and X-Forwarded-Host if set (reverse proxy), falling
// back to the request's TLS state and Host header.
func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	} else if r.TLS != nil {
		scheme = "https"
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

func rewriteLocalUploadURLs(urls []UploadURLInfo, hubEndpoint, resourceType, resourceID, authHeader string) []UploadURLInfo {
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
			if authHeader != "" {
				urls[i].Headers["Authorization"] = authHeader
			}
		}
	}
	return urls
}

// rewriteLocalDownloadURLs rewrites file:// URLs to HTTP proxy URLs pointing to
// the hub's own file read endpoint for downloads. Same rationale as
// rewriteLocalUploadURLs — file:// URLs reference server-side paths that are
// inaccessible from remote clients.
func rewriteLocalDownloadURLs(urls []DownloadURLInfo, hubEndpoint, resourceType, resourceID, authHeader string) []DownloadURLInfo {
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
