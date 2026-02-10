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

package transfer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Client handles file transfers using signed URLs.
type Client struct {
	// HTTPClient is the HTTP client used for transfers.
	// If nil, http.DefaultClient is used.
	HTTPClient *http.Client
}

// NewClient creates a new transfer client.
func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{HTTPClient: httpClient}
}

// UploadFile uploads a file to a signed URL.
// Supports both file:// URLs (local filesystem) and HTTP(S) URLs (remote storage).
func (c *Client) UploadFile(ctx context.Context, url UploadURLInfo, content io.Reader) error {
	return c.UploadFileWithMethod(ctx, url.URL, url.Method, url.Headers, content)
}

// UploadFileWithMethod uploads content to a signed URL with explicit method and headers.
// Supports both file:// URLs (local filesystem) and HTTP(S) URLs (remote storage).
func (c *Client) UploadFileWithMethod(ctx context.Context, signedURL, method string, headers map[string]string, content io.Reader) error {
	// Handle file:// URLs for local storage
	if strings.HasPrefix(signedURL, "file://") {
		return c.uploadToFile(signedURL, content)
	}

	if method == "" {
		method = http.MethodPut
	}

	// Read content into buffer for Content-Length
	var body bytes.Buffer
	if _, err := io.Copy(&body, content); err != nil {
		return fmt.Errorf("failed to read content: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, method, signedURL, &body)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.ContentLength = int64(body.Len())

	// Set headers
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("upload failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	return nil
}

// uploadToFile writes content directly to a file path from a file:// URL.
func (c *Client) uploadToFile(fileURL string, content io.Reader) error {
	// Parse file:// URL to get path
	path := strings.TrimPrefix(fileURL, "file://")

	// Ensure parent directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	// Create and write file
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to create file %s: %w", path, err)
	}
	defer f.Close()

	if _, err := io.Copy(f, content); err != nil {
		return fmt.Errorf("failed to write file %s: %w", path, err)
	}

	return nil
}

// DownloadFile downloads content from a signed URL.
// Supports both file:// URLs (local filesystem) and HTTP(S) URLs (remote storage).
func (c *Client) DownloadFile(ctx context.Context, signedURL string) ([]byte, error) {
	// Handle file:// URLs for local storage
	if strings.HasPrefix(signedURL, "file://") {
		path := strings.TrimPrefix(signedURL, "file://")
		return os.ReadFile(path)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, signedURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("download failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	return io.ReadAll(resp.Body)
}

// DownloadToFile downloads content from a signed URL and writes it to a local file.
// Creates parent directories as needed.
func (c *Client) DownloadToFile(ctx context.Context, signedURL, destPath string) error {
	content, err := c.DownloadFile(ctx, signedURL)
	if err != nil {
		return err
	}

	// Ensure parent directory exists
	dir := filepath.Dir(destPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	return os.WriteFile(destPath, content, 0644)
}

// UploadFiles uploads multiple files to their respective signed URLs.
// If progress is not nil, it is called after each file is uploaded.
func (c *Client) UploadFiles(ctx context.Context, files []FileInfo, urls []UploadURLInfo, progress ProgressCallback) error {
	// Build URL map for quick lookup
	urlMap := make(map[string]UploadURLInfo)
	for _, u := range urls {
		urlMap[u.Path] = u
	}

	for _, file := range files {
		url, ok := urlMap[file.Path]
		if !ok {
			continue // Skip files without URLs (may be existing)
		}

		f, err := os.Open(file.FullPath)
		if err != nil {
			return fmt.Errorf("failed to open file %s: %w", file.Path, err)
		}

		err = c.UploadFile(ctx, url, f)
		f.Close()
		if err != nil {
			return fmt.Errorf("failed to upload file %s: %w", file.Path, err)
		}

		if progress != nil {
			if err := progress(file, file.Size); err != nil {
				return err
			}
		}
	}

	return nil
}

// DownloadFiles downloads multiple files from signed URLs to a destination directory.
// If progress is not nil, it is called after each file is downloaded.
func (c *Client) DownloadFiles(ctx context.Context, urls []DownloadURLInfo, destDir string, progress ProgressCallback) error {
	for _, url := range urls {
		destPath := filepath.Join(destDir, filepath.FromSlash(url.Path))

		if err := c.DownloadToFile(ctx, url.URL, destPath); err != nil {
			return fmt.Errorf("failed to download file %s: %w", url.Path, err)
		}

		if progress != nil {
			file := FileInfo{
				Path: url.Path,
				Size: url.Size,
				Hash: url.Hash,
			}
			if err := progress(file, url.Size); err != nil {
				return err
			}
		}
	}

	return nil
}
